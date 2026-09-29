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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

// Profile is a quota profile, aisa/quotas/<name> in Consul KV.
type Profile struct {
	// TokensPerHour is how many tokens a consumer may use in any hour. 0 allows none.
	TokensPerHour int64
}

// Lister reads the quota profiles, from Consul KV outside tests. It works like consul.List:
// with index 0 it answers at once, with an earlier index it waits for a change.
type Lister interface {
	List(ctx context.Context, index uint64) ([]consul.Pair, uint64, error)
}

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

// ProfileOptions configure Profiles.
type ProfileOptions struct {
	// RetryMin is the wait after a failed load; it doubles with every further failure, up to
	// RetryMax. 1 s and 1 min when zero. It is also the least time between two requests to
	// the source, so a source that answers at once cannot make aisa ask it in a tight loop.
	RetryMin, RetryMax time.Duration
	// OnLoad is called after every load, when set.
	OnLoad func(LoadStats)
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

// Ready returns nil once the profiles have been read, for aisa's readiness.
func (p *Profiles) Ready(context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return ErrNotLoaded
	}
	return nil
}

// Run reads the profiles, then follows their changes until ctx is done. When the source cannot
// be read, the profiles read before stay in use and it tries again after RetryMin, doubling up
// to RetryMax.
func (p *Profiles) Run(ctx context.Context) {
	var index uint64
	applied := false
	retry := p.opts.RetryMin
	for {
		started := time.Now()
		pairs, next, err := p.src.List(ctx, index)
		if ctx.Err() != nil {
			return
		}
		wait := time.Duration(0)
		if err != nil {
			p.log.Error("reading quota profiles failed; keeping the ones read before",
				"error", err, "retry_in", retry.String())
			if p.opts.OnLoad != nil {
				p.opts.OnLoad(LoadStats{Err: err})
			}
			wait = retry
			retry = min(retry*2, p.opts.RetryMax)
		} else {
			retry = p.opts.RetryMin
			// A blocking query that timed out answers with the same index and nothing new.
			if !applied || next != index {
				p.apply(pairs)
				applied = true
			}
			index = nextIndex(index, next)
			wait = p.opts.RetryMin - time.Since(started)
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// nextIndex is the index of the next blocking query, following Consul's advice: an index that
// went backwards, as after a restore of Consul's data, starts over with a query that answers
// at once, and an index is never below 1.
func nextIndex(prev, next uint64) uint64 {
	if next < prev {
		return 0
	}
	return max(next, 1)
}

// apply replaces the profiles with those read.
func (p *Profiles) apply(pairs []consul.Pair) {
	valid := map[string]Profile{}
	invalid := map[string]error{}
	for _, kv := range pairs {
		// The prefix itself, or a folder under it.
		if kv.Key == "" || strings.HasSuffix(kv.Key, "/") {
			continue
		}
		pr, err := parseProfile(kv.Value)
		if err != nil {
			invalid[kv.Key] = err
			continue
		}
		valid[kv.Key] = pr
	}
	names := make([]string, 0, len(invalid))
	for name := range invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Consumers with this profile are denied with 503 until it is fixed.
		p.log.Error("invalid quota profile", "profile", name, "error", invalid[name])
	}

	p.mu.Lock()
	p.loaded, p.valid, p.invalid = true, valid, invalid
	p.mu.Unlock()
	p.log.Info("quota profiles loaded", "valid", len(valid), "invalid", len(invalid))
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
