// Package quotas enforces token quotas (docs/features/quotas-and-budgets.md): a consumer's
// quota profile in Consul KV gives the tokens it may use per hour, and a sliding window of
// counters in Redis holds what it has used.
package quotas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/consumers"
)

// Profile is a quota profile, aisa/quotas/<name> in Consul KV.
type Profile struct {
	// TokensPerHour is how many tokens a consumer may use in any hour. 0 allows none.
	TokensPerHour int64
}

// Lister reads the quota profiles, from Consul KV outside tests; see consul.Lister.
type Lister = consul.Lister

// ConsulSource reads the profiles under a prefix of Consul KV, such as "aisa/quotas/".
type ConsulSource struct {
	Client *consul.Client
	Prefix string
	// Wait is how long a blocking query waits; 5 minutes when zero.
	Wait time.Duration
}

// List implements Lister.
func (s ConsulSource) List(ctx context.Context, index uint64) ([]consul.Pair, uint64, error) {
	wait := s.Wait
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	pairs, idx, err := s.Client.List(ctx, s.Prefix, index, wait)
	if err != nil {
		return nil, 0, fmt.Errorf("list quota profiles: %w", err)
	}
	// Names are what follows the prefix.
	for i := range pairs {
		pairs[i].Key = strings.TrimPrefix(pairs[i].Key, s.Prefix)
	}
	return pairs, idx, nil
}

// LoadStats describe a load of the profiles, for metrics.
type LoadStats struct {
	// Err is nil when the load replaced the profiles.
	Err error
	// Valid and Invalid count the profiles that could and could not be read.
	Valid, Invalid int
}

// Referrers lists the consumers, whose declarations name quota profiles; *consumers.Declarations
// is one.
type Referrers interface {
	// Snapshot returns the declarations that could be read, by consumer, and whether they have
	// been read at all.
	Snapshot() (valid map[string]consumers.Declaration, invalid map[string]bool, loaded bool)
}

// ProfileOptions configure Profiles.
type ProfileOptions struct {
	// RetryMin is the wait after a failed load; it doubles with every further failure, up to
	// RetryMax. 1 s and 1 min when zero. It is also the least time between two requests to
	// the source, so a source that answers at once cannot make aisa ask it in a tight loop.
	RetryMin, RetryMax time.Duration
	// OnLoad is called after every load, when set.
	OnLoad func(LoadStats)
	// Default names the quota profile of a consumer whose declaration names none
	// (AISA_DEFAULT_QUOTA_PROFILE); empty: there is none. Ready waits for it, and Missing
	// reports it while it is not in Consul.
	Default string
	// Consumers are the consumers whose declared profiles Missing checks; nil: none.
	Consumers Referrers
}

// Profiles holds the quota profiles, kept up to date with the source.
type Profiles struct {
	src  Lister
	opts ProfileOptions
	log  *slog.Logger

	mu      sync.RWMutex
	loaded  bool
	valid   map[string]Profile
	invalid map[string]error
	// defaultSeen: the default profile has been read, valid, at some load.
	defaultSeen bool
}

// NewProfiles returns an empty set of profiles. Call Run to fill it.
func NewProfiles(src Lister, log *slog.Logger, opts ProfileOptions) *Profiles {
	if opts.RetryMin <= 0 {
		opts.RetryMin = time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = time.Minute
	}
	return &Profiles{src: src, opts: opts, log: log}
}

// Errors of Get.
var (
	// ErrNotLoaded: the profiles have not been read yet.
	ErrNotLoaded = errors.New("quota profiles not loaded yet")
	// ErrUnknownProfile: no profile has the name.
	ErrUnknownProfile = errors.New("unknown quota profile")
	// ErrDefaultMissing: the default profile has not been read yet, so a consumer without a
	// profile of its own cannot be served.
	ErrDefaultMissing = errors.New("default quota profile not in Consul")
)

// Get returns the profile of a name. A profile that exists but cannot be read returns its
// error, so a broken profile does not pass for one without a limit.
func (p *Profiles) Get(name string) (Profile, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return Profile{}, ErrNotLoaded
	}
	if pr, ok := p.valid[name]; ok {
		return pr, nil
	}
	if err, ok := p.invalid[name]; ok {
		return Profile{}, fmt.Errorf("quota profile %q: %w", name, err)
	}
	return Profile{}, fmt.Errorf("%w %q", ErrUnknownProfile, name)
}

// Ready returns nil once the profiles have been read and, when a default profile is set, it
// has been among them at some load, for aisa's readiness: a fresh install whose default names
// no profile never becomes ready. A default that is removed later is reported by Missing, not
// by taking aisa out of service, since the consumers with a profile of their own are unaffected.
func (p *Profiles) Ready(context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return ErrNotLoaded
	}
	if p.opts.Default != "" && !p.defaultSeen {
		return fmt.Errorf("%w: %q", ErrDefaultMissing, p.opts.Default)
	}
	return nil
}

// Missing are the quota profiles that are named and not in Consul, what
// aisa_quota_profiles_missing shows.
type Missing struct {
	// Default: the default profile is not among those read.
	Default bool
	// Consumers maps each consumer whose declared profile is not among those read to that
	// profile.
	Consumers map[string]string
}

// Missing reports the profiles that the default and the consumers' declarations name and that
// are not in Consul; their consumers are denied with 503. A profile that is there and cannot be
// parsed is not missing: it is in aisa_quota_profiles_invalid. Before the profiles or the
// consumers have been read, nothing is missing.
func (p *Profiles) Missing() Missing {
	var declared map[string]consumers.Declaration
	if p.opts.Consumers != nil {
		if valid, _, loaded := p.opts.Consumers.Snapshot(); loaded {
			declared = valid
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	var m Missing
	if !p.loaded {
		return m
	}
	m.Default = p.opts.Default != "" && !p.has(p.opts.Default)
	for name, decl := range declared {
		if decl.QuotaProfile == "" || p.has(decl.QuotaProfile) {
			continue
		}
		if m.Consumers == nil {
			m.Consumers = make(map[string]string)
		}
		m.Consumers[name] = decl.QuotaProfile
	}
	return m
}

// has says whether a profile of the name was read, valid or not. The caller holds mu.
func (p *Profiles) has(name string) bool {
	if _, ok := p.valid[name]; ok {
		return true
	}
	_, ok := p.invalid[name]
	return ok
}

// Collectors are the metrics of Missing, aisa_quota_profiles_missing{source}, for aisa's
// registry. They are computed when scraped, so they are current after a change of the
// profiles and of the consumers alike.
func (p *Profiles) Collectors() []prometheus.Collector {
	gauge := func(source string, value func(Missing) float64) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "aisa_quota_profiles_missing",
			Help:        "Quota profiles that are named and not in Consul, by who names them: the default profile (source=default) or a consumer's declaration (source=consumer, one per consumer). Their consumers are answered 503.",
			ConstLabels: prometheus.Labels{"source": source},
		}, func() float64 { return value(p.Missing()) })
	}
	return []prometheus.Collector{
		gauge("default", func(m Missing) float64 {
			if m.Default {
				return 1
			}
			return 0
		}),
		gauge("consumer", func(m Missing) float64 { return float64(len(m.Consumers)) }),
	}
}

// logMissing names the missing profiles after a load. The decision names them again, at every
// request of their consumers.
func (p *Profiles) logMissing() {
	m := p.Missing()
	if m.Default {
		p.log.Error("the default quota profile is not in Consul; consumers without a profile of their own are denied with 503",
			"profile", p.opts.Default)
	}
	for _, name := range slices.Sorted(maps.Keys(m.Consumers)) {
		p.log.Error("a consumer's quota profile is not in Consul; the consumer is denied with 503",
			"consumer", name, "profile", m.Consumers[name])
	}
}

// Run reads the profiles, then follows their changes until ctx is done. When the source cannot
// be read, the profiles read before stay in use and it tries again after RetryMin, doubling up
// to RetryMax.
func (p *Profiles) Run(ctx context.Context) {
	consul.Follow(ctx, p.src, consul.FollowOptions{RetryMin: p.opts.RetryMin, RetryMax: p.opts.RetryMax}, p.apply, func(err error) {
		p.log.Error("reading quota profiles failed; keeping the ones read before", "error", err)
		if p.opts.OnLoad != nil {
			p.opts.OnLoad(LoadStats{Err: err})
		}
	})
}

// apply replaces the profiles with those read.
func (p *Profiles) apply(pairs []consul.Pair) {
	valid, invalid, names := consul.ParseEntries(pairs, parseProfile)
	for _, name := range names {
		// Consumers with this profile are denied with 503 until it is fixed.
		p.log.Error("invalid quota profile", "profile", name, "error", invalid[name])
	}

	p.mu.Lock()
	p.loaded, p.valid, p.invalid = true, valid, invalid
	if _, ok := valid[p.opts.Default]; ok && p.opts.Default != "" {
		p.defaultSeen = true
	}
	p.mu.Unlock()
	p.log.Info("quota profiles loaded", "valid", len(valid), "invalid", len(invalid))
	p.logMissing()
	if p.opts.OnLoad != nil {
		p.opts.OnLoad(LoadStats{Valid: len(valid), Invalid: len(invalid)})
	}
}

// parseProfile reads a profile's JSON value, such as {"tokens_per_hour": 200000}. Fields it
// does not know are ignored, so a later version can add some, but tokens_per_hour must be there.
func parseProfile(value []byte) (Profile, error) {
	var raw struct {
		TokensPerHour *int64 `json:"tokens_per_hour"`
	}
	if err := json.Unmarshal(value, &raw); err != nil {
		return Profile{}, fmt.Errorf("want a JSON object with tokens_per_hour: %w", err)
	}
	if raw.TokensPerHour == nil {
		return Profile{}, errors.New("tokens_per_hour is missing")
	}
	if *raw.TokensPerHour < 0 {
		return Profile{}, fmt.Errorf("tokens_per_hour is negative: %d", *raw.TokensPerHour)
	}
	return Profile{TokensPerHour: *raw.TokensPerHour}, nil
}
