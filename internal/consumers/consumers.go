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

// Options configure a Store.
type Options struct {
	// Refresh is the interval of the periodic reload.
	Refresh time.Duration
	// MissRefresh is the least time between two reloads caused by unknown keys, so random keys
	// cannot make aisa call Vault on every request.
	MissRefresh time.Duration
	// MaxStale is how old the loaded consumers may get while reloads fail. After that, lookups
	// return Unavailable and aisa fails closed.
	MaxStale time.Duration
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// Store holds the consumers by key hash.
type Store struct {
	src  Source
	opts Options
	log  *slog.Logger

	loadMu sync.Mutex // one load at a time

	mu          sync.RWMutex
	byHash      map[string]Consumer
	loadedAt    time.Time // last successful load
	lastAttempt time.Time // last load, successful or not
}

// New returns an empty store. Call Load or Run to fill it.
func New(src Source, log *slog.Logger, opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
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
	// The request waits for this reload, and the gateway waits for the request (APISIX
	// forward-auth: 3 s by default), so a slow Vault must not hold it up for long.
	loadCtx, cancel := context.WithTimeout(ctx, missLoadTimeout)
	defer cancel()
	if err := s.Load(loadCtx); err != nil {
		s.log.Warn("reloading consumers after an unknown key failed", "error", err)
	}
	return s.find(hash)
}

// missLoadTimeout bounds a reload that a request with an unknown key waits for.
const missLoadTimeout = 2 * time.Second

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

// Run loads the consumers now and then every Refresh until ctx is done.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.Refresh)
	defer t.Stop()
	for {
		if err := s.Load(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("loading consumers failed; keeping the ones loaded before", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Load reads all consumers and replaces the loaded ones. When any read fails, the loaded
// consumers stay as they were: a partial set would reject keys that are valid.
func (s *Store) Load(ctx context.Context) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.Lock()
	s.lastAttempt = s.opts.Now()
	s.mu.Unlock()

	names, err := s.src.Names(ctx)
	if err != nil {
		return fmt.Errorf("list consumers: %w", err)
	}
	byHash := make(map[string]Consumer)
	owner := make(map[string]string) // hash → consumer name, to find a hash used twice
	dup := make(map[string]bool)
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			continue // a subdirectory, not a consumer
		}
		data, err := s.src.Read(ctx, name)
		if errors.Is(err, ErrGone) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read consumer %s: %w", name, err)
		}
		c := Consumer{Name: name, QuotaProfile: stringField(data, "quota_profile")}
		hashes, bad := parseHashes(stringField(data, "key_sha256"))
		if bad > 0 {
			s.log.Warn("consumer has key_sha256 values that are not SHA-256 hex digests; they are ignored",
				"consumer", name, "ignored", bad)
		}
		if len(hashes) == 0 {
			s.log.Warn("consumer has no valid key_sha256 and cannot authenticate", "consumer", name)
			continue
		}
		for _, h := range hashes {
			if other, ok := owner[h]; ok && other != name {
				dup[h] = true
				s.log.Error("two consumers have the same key hash; neither can use it",
					"consumer", name, "other", other)
				continue
			}
			owner[h] = name
			byHash[h] = c
		}
	}
	for h := range dup {
		delete(byHash, h)
	}

	s.mu.Lock()
	s.byHash = byHash
	s.loadedAt = s.opts.Now()
	s.mu.Unlock()
	s.log.Debug("consumers loaded", "keys", len(byHash))
	return nil
}

// parseHashes splits a key_sha256 value into hashes. It holds one hash, or several separated by
// commas or spaces while a key is rotated. It returns the valid ones in lower case and how many
// were not SHA-256 hex digests.
func parseHashes(v string) (hashes []string, bad int) {
	for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
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

// Names lists the consumers. A path without consumers is an empty list.
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
