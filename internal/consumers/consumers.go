// Package consumers keeps the consumers and the SHA-256 hashes of their keys in memory, loaded
// from Vault (docs/concepts/VAULT.md), so validating a key does not call Vault on every request.
package consumers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hlan-net/aisa/internal/vault"
)

// Consumer is an application or person with a key.
type Consumer struct {
	// Name is the consumer's name, the last element of its Vault path.
	Name string
	// QuotaProfile names the consumer's quota profile in Consul, empty when none is set.
	QuotaProfile string
}

// Result of a lookup.
type Result int

const (
	// Found: the key belongs to a consumer.
	Found Result = iota
	// Unknown: no consumer has the key.
	Unknown
	// Unavailable: the consumers could not be loaded recently enough to decide.
	Unavailable
)

// Source reads consumers, from Vault outside tests.
type Source interface {
	// Names lists the consumers. No consumers at all is an empty list, not an error.
	Names(ctx context.Context) ([]string, error)
	// Read returns a consumer's secret data. A consumer deleted since Names returns ErrGone.
	Read(ctx context.Context, name string) (map[string]any, error)
}

// ErrGone is returned by Source.Read for a consumer that no longer exists.
var ErrGone = errors.New("consumer no longer exists")

// Stats describe a load, for metrics.
type Stats struct {
	// Err is nil when the load replaced the loaded consumers.
	Err error
	// Consumers is how many consumers can authenticate, Keys how many key hashes they have
	// together, Unreadable how many consumers Vault listed and aisa could not read. When Err is
	// set they are those of the last load that succeeded.
	Consumers, Keys, Unreadable int
}

// Options configure a Store.
type Options struct {
	// Refresh is the interval of the periodic reload.
	Refresh time.Duration
	// RetryMin is the wait after a failed load; it doubles with every further failure, up to
	// Refresh. 1 s when zero.
	RetryMin time.Duration
	// MissRefresh is the least time between two reloads caused by unknown keys, so random keys
	// cannot make aisa call Vault on every request.
	MissRefresh time.Duration
	// MissTimeout bounds how long a request with an unknown key waits for a reload; the gateway
	// waits for that request, often for only a few seconds. 2 s when zero.
	MissTimeout time.Duration
	// MaxStale is how old the loaded consumers may get while reloads fail. After that, lookups
	// return Unavailable and aisa fails closed. It is also how long the keys of one consumer
	// are kept while that consumer cannot be read.
	MaxStale time.Duration
	// OnLoad is called after every load, when set.
	OnLoad func(Stats)
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// entry is a consumer as it was read.
type entry struct {
	consumer Consumer
	hashes   []string
	readAt   time.Time
}

// loadCall is a load in progress, which others can wait for.
type loadCall struct {
	done chan struct{}
	err  error // valid once done is closed
}

// Store holds the consumers by key hash.
type Store struct {
	src  Source
	opts Options
	log  *slog.Logger

	mu          sync.RWMutex
	byHash      map[string]Consumer
	byName      map[string]entry // replaced as a whole by every load, never changed in place
	unreadable  int              // consumers that the last successful load could not read
	loadedAt    time.Time        // last successful load
	lastAttempt time.Time        // begin or end of the last load, successful or not
	inflight    *loadCall        // nil when no load is in progress
}

// New returns an empty store. Call Load or Run to fill it.
func New(src Source, log *slog.Logger, opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = time.Second
	}
	if opts.MissTimeout <= 0 {
		opts.MissTimeout = 2 * time.Second
	}
	return &Store{src: src, opts: opts, log: log}
}

// Lookup finds the consumer of a plaintext key. An unknown key triggers a reload, at most once
// per MissRefresh, so a key created in Vault a moment ago is accepted.
func (s *Store) Lookup(ctx context.Context, key string) (Consumer, Result) {
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])

	c, res := s.find(hash)
	if res != Unknown {
		return c, res
	}
	s.mu.RLock()
	recent := s.opts.Now().Sub(s.lastAttempt) < s.opts.MissRefresh
	s.mu.RUnlock()
	if recent {
		return Consumer{}, Unknown
	}
	s.reloadFor(ctx)
	return s.find(hash)
}

// reloadFor reloads the consumers for a request with an unknown key, or joins the load in
// progress, and waits for it no longer than MissTimeout.
func (s *Store) reloadFor(ctx context.Context) {
	call, started := s.begin()
	if started {
		// Not tied to the request: other requests wait for this load too, and it must not end
		// because this client went away.
		go func() {
			loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.MissTimeout)
			defer cancel()
			err := s.load(loadCtx)
			if err != nil {
				s.log.Warn("reloading consumers after an unknown key failed", "error", err)
			}
			s.finish(call, err)
		}()
	}
	t := time.NewTimer(s.opts.MissTimeout)
	defer t.Stop()
	select {
	case <-call.done:
	case <-t.C:
	case <-ctx.Done():
	}
}

func (s *Store) find(hash string) (Consumer, Result) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.loadedAt.IsZero() || s.opts.Now().Sub(s.loadedAt) > s.opts.MaxStale {
		return Consumer{}, Unavailable
	}
	if c, ok := s.byHash[hash]; ok {
		return c, Found
	}
	return Consumer{}, Unknown
}

// Ready returns nil when lookups can be answered, for aisa's readiness.
func (s *Store) Ready(context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.loadedAt.IsZero() {
		return errors.New("consumers not loaded yet")
	}
	if age := s.opts.Now().Sub(s.loadedAt); age > s.opts.MaxStale {
		return fmt.Errorf("consumers last loaded %s ago", age.Round(time.Second))
	}
	return nil
}

// Run loads the consumers now and then every Refresh until ctx is done. After a failed load it
// tries again sooner, so a Vault that was not ready when aisa started costs seconds, not a
// whole interval.
func (s *Store) Run(ctx context.Context) {
	retry := s.opts.RetryMin
	for {
		wait := s.opts.Refresh
		if err := s.Load(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = min(retry, s.opts.Refresh)
			retry *= 2
			s.log.Error("loading consumers failed; keeping the ones loaded before",
				"error", err, "retry_in", wait.String())
		} else {
			retry = s.opts.RetryMin
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Load reads all consumers and replaces the loaded ones, or waits for the load in progress and
// returns its result. It returns when ctx is done, also while it waits.
func (s *Store) Load(ctx context.Context) error {
	call, started := s.begin()
	if started {
		s.finish(call, s.load(ctx))
	}
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return fmt.Errorf("waiting for the load in progress: %w", ctx.Err())
	}
}

// begin returns the load in progress, or registers a new one that the caller must run and
// finish.
func (s *Store) begin() (call *loadCall, started bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight != nil {
		return s.inflight, false
	}
	s.inflight = &loadCall{done: make(chan struct{})}
	s.lastAttempt = s.opts.Now()
	return s.inflight, true
}

func (s *Store) finish(call *loadCall, err error) {
	s.mu.Lock()
	s.inflight = nil
	// Counted from the end as well: a load that took long must not be followed by the next one
	// at once.
	s.lastAttempt = s.opts.Now()
	stats := Stats{Err: err, Consumers: countConsumers(s.byName), Keys: len(s.byHash), Unreadable: s.unreadable}
	s.mu.Unlock()

	call.err = err
	close(call.done)
	if s.opts.OnLoad != nil {
		s.opts.OnLoad(stats)
	}
}

func countConsumers(byName map[string]entry) int {
	n := 0
	for _, e := range byName {
		if len(e.hashes) > 0 {
			n++
		}
	}
	return n
}

// load reads the consumers from the source. It fails, and leaves the loaded consumers as they
// were, when they cannot be listed or none of them can be read: then Vault is the problem.
//
// A single consumer that cannot be read does not stop the others from being loaded. It keeps
// the keys it had when it was last read, for MaxStale at most: dropping them would reject keys
// that are valid, and keeping them for good would accept keys that were revoked since.
func (s *Store) load(ctx context.Context) error {
	names, err := s.src.Names(ctx)
	if err != nil {
		return fmt.Errorf("list consumers: %w", err)
	}
	s.mu.RLock()
	prev := s.byName
	s.mu.RUnlock()
	now := s.opts.Now()

	byName := make(map[string]entry, len(names))
	listed, failed := 0, 0
	var lastErr error
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			continue // a subdirectory, not a consumer
		}
		listed++
		e, err := s.readConsumer(ctx, name, now)
		switch {
		case errors.Is(err, ErrGone):
			// Deleted between the listing and the read.
		case err != nil && ctx.Err() != nil:
			return fmt.Errorf("load interrupted: %w", err)
		case err != nil:
			failed++
			lastErr = err
			if old, ok := prev[name]; ok && now.Sub(old.readAt) <= s.opts.MaxStale {
				byName[name] = old
				s.log.Error("consumer cannot be read; keeping its keys as they were read before",
					"consumer", name, "read", old.readAt, "error", err)
			} else {
				s.log.Error("consumer cannot be read and cannot authenticate", "consumer", name, "error", err)
			}
		default:
			byName[name] = e
		}
	}
	if listed > 0 && failed == listed {
		return fmt.Errorf("none of the %d consumers could be read: %w", listed, lastErr)
	}
	if len(byName) == 0 && len(prev) > 0 {
		s.log.Warn("Vault lists no consumers any more; every key is rejected from now on")
	}
	byHash := s.index(byName)

	s.mu.Lock()
	s.byName = byName
	s.byHash = byHash
	s.unreadable = failed
	s.loadedAt = now
	s.mu.Unlock()
	s.log.Debug("consumers loaded", "consumers", len(byName), "keys", len(byHash), "unreadable", failed)
	return nil
}

// index maps the key hashes to their consumers. A hash that two consumers have belongs to
// neither.
func (s *Store) index(byName map[string]entry) map[string]Consumer {
	byHash := make(map[string]Consumer)
	dup := make(map[string]bool)
	for name, e := range byName {
		for _, h := range e.hashes {
			if other, ok := byHash[h]; ok && other.Name != name {
				dup[h] = true
				s.log.Error("two consumers have the same key hash; neither can use it",
					"consumer", name, "other", other.Name)
				continue
			}
			byHash[h] = e.consumer
		}
	}
	for h := range dup {
		delete(byHash, h)
	}
	return byHash
}

// readConsumer reads one consumer and its valid key hashes. A consumer without a valid hash is
// read, and cannot authenticate; a consumer deleted in the meantime is ErrGone.
func (s *Store) readConsumer(ctx context.Context, name string, now time.Time) (entry, error) {
	data, err := s.src.Read(ctx, name)
	if err != nil {
		return entry{}, fmt.Errorf("read consumer %s: %w", name, err)
	}
	hashes, bad := parseHashes(stringField(data, "key_sha256"))
	if bad > 0 {
		s.log.Warn("consumer has key_sha256 values that are not SHA-256 hex digests; they are ignored",
			"consumer", name, "ignored", bad)
	}
	if len(hashes) == 0 {
		s.log.Warn("consumer has no valid key_sha256 and cannot authenticate", "consumer", name)
	}
	return entry{
		consumer: Consumer{Name: name, QuotaProfile: stringField(data, "quota_profile")},
		hashes:   hashes,
		readAt:   now,
	}, nil
}

// parseHashes splits a key_sha256 value into hashes. It holds one hash, or several separated by
// commas or white space, line ends of any kind included, while a key is rotated. It returns the
// valid ones in lower case and how many were not SHA-256 hex digests.
func parseHashes(v string) (hashes []string, bad int) {
	for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		f = strings.ToLower(f)
		if b, err := hex.DecodeString(f); err != nil || len(b) != sha256.Size {
			bad++
			continue
		}
		hashes = append(hashes, f)
	}
	return hashes, bad
}

func stringField(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return strings.TrimSpace(s)
}

// Vault reads consumers from the KV v2 path <mount>/<prefix>/consumers, one secret per
// consumer (docs/concepts/VAULT.md).
type Vault struct {
	Client interface {
		List(ctx context.Context, mount, path string) ([]string, error)
		Read(ctx context.Context, mount, path string) (map[string]any, error)
	}
	Mount  string
	Prefix string
}

// Names lists the consumers. A path without consumers is an empty list: that is how Vault
// answers before the first consumer is created and after the last one is deleted. A mount that
// does not exist is an error, not an empty list.
func (v Vault) Names(ctx context.Context) ([]string, error) {
	names, err := v.Client.List(ctx, v.Mount, v.Prefix+"/consumers")
	if errors.Is(err, vault.ErrNotFound) {
		return nil, nil
	}
	return names, err
}

// Read returns a consumer's secret; a consumer deleted in the meantime is ErrGone.
func (v Vault) Read(ctx context.Context, name string) (map[string]any, error) {
	data, err := v.Client.Read(ctx, v.Mount, v.Prefix+"/consumers/"+name)
	if errors.Is(err, vault.ErrNotFound) {
		return nil, ErrGone
	}
	return data, err
}
