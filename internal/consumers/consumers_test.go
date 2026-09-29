package consumers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/vault"
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type fakeSource struct {
	mu      sync.Mutex
	data    map[string]map[string]any
	fail    error
	listing int
}

func (f *fakeSource) Names(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listing++
	if f.fail != nil {
		return nil, f.fail
	}
	var names []string
	for n := range f.data {
		names = append(names, n)
	}
	return names, nil
}

func (f *fakeSource) Read(_ context.Context, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.data[name]
	if !ok {
		return nil, ErrGone
	}
	return d, nil
}

func (f *fakeSource) set(name string, d map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[name] = d
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newStore(src Source) (*Store, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s := New(src, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Refresh: time.Minute, MissRefresh: 5 * time.Second, MaxStale: 15 * time.Minute, Now: c.now,
	})
	return s, c
}

func TestLookup(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{
		"chat-ui":    {"key_sha256": hashOf("key-chat"), "quota_profile": "interactive"},
		"batch-jobs": {"key_sha256": strings.ToUpper(hashOf("key-batch"))},
	}}
	s, _ := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}

	c, res := s.Lookup(ctx, "key-chat")
	if res != Found || c.Name != "chat-ui" || c.QuotaProfile != "interactive" {
		t.Errorf("key-chat: %+v, %v", c, res)
	}
	if c, res := s.Lookup(ctx, "key-batch"); res != Found || c.Name != "batch-jobs" {
		t.Errorf("upper-case hash: %+v, %v", c, res)
	}
	if _, res := s.Lookup(ctx, "nope"); res != Unknown {
		t.Errorf("unknown key: %v", res)
	}
	if err := s.Ready(ctx); err != nil {
		t.Errorf("Ready: %v", err)
	}
}

func TestRotationWithTwoHashes(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{
		"chat-ui": {"key_sha256": hashOf("old") + ", " + hashOf("new")},
	}}
	s, _ := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"old", "new"} {
		if c, res := s.Lookup(ctx, k); res != Found || c.Name != "chat-ui" {
			t.Errorf("%s: %+v, %v", k, c, res)
		}
	}
}

func TestInvalidAndDuplicateHashes(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{
		"a":      {"key_sha256": hashOf("shared") + " not-a-hash"},
		"b":      {"key_sha256": hashOf("shared") + "," + hashOf("only-b")},
		"broken": {"key_sha256": "abc"},
		"nokey":  {"quota_profile": "batch"},
		"dir/":   {},
	}}
	s, _ := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, res := s.Lookup(ctx, "shared"); res != Unknown {
		t.Errorf("a hash used by two consumers must not authenticate either: %v", res)
	}
	if c, res := s.Lookup(ctx, "only-b"); res != Found || c.Name != "b" {
		t.Errorf("b's own key: %+v, %v", c, res)
	}
}

func TestUnknownKeyReloadsAtMostOncePerInterval(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{}}
	s, clk := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	src.set("new-app", map[string]any{"key_sha256": hashOf("fresh")})

	// Within MissRefresh of the last load: no reload, the key is unknown.
	if _, res := s.Lookup(ctx, "fresh"); res != Unknown {
		t.Errorf("right after a load: %v, want Unknown", res)
	}
	clk.advance(6 * time.Second)
	if c, res := s.Lookup(ctx, "fresh"); res != Found || c.Name != "new-app" {
		t.Errorf("after MissRefresh: %+v, %v", c, res)
	}
	listings := src.listing
	for range 10 {
		s.Lookup(ctx, "random")
	}
	if src.listing != listings {
		t.Errorf("unknown keys caused %d more loads, want 0 within MissRefresh", src.listing-listings)
	}
}

func TestFailedLoadKeepsConsumersUntilMaxStale(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{"chat-ui": {"key_sha256": hashOf("k")}}}
	s, clk := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	src.fail = errors.New("vault down")
	clk.advance(10 * time.Minute)
	if err := s.Load(ctx); err == nil {
		t.Fatal("want the load to fail")
	}
	if _, res := s.Lookup(ctx, "k"); res != Found {
		t.Errorf("10 min after the last load: %v, want Found", res)
	}
	clk.advance(6 * time.Minute)
	if _, res := s.Lookup(ctx, "k"); res != Unavailable {
		t.Errorf("16 min after the last load: %v, want Unavailable", res)
	}
	if err := s.Ready(ctx); err == nil {
		t.Error("Ready: want an error when the consumers are too old")
	}
}

func TestNotLoadedIsUnavailable(t *testing.T) {
	src := &fakeSource{fail: errors.New("vault down")}
	s, _ := newStore(src)
	if _, res := s.Lookup(context.Background(), "k"); res != Unavailable {
		t.Errorf("before any load: %v, want Unavailable", res)
	}
	if err := s.Ready(context.Background()); err == nil {
		t.Error("Ready: want an error before the first load")
	}
}

func TestRunLoadsAndStops(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{"a": {"key_sha256": hashOf("k")}}}
	s := New(src, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Refresh: 10 * time.Millisecond, MissRefresh: time.Second, MaxStale: time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for s.Ready(ctx) != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("not ready after Run: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

type fakeKV struct{ data map[string]map[string]any }

func (f fakeKV) List(_ context.Context, mount, path string) ([]string, error) {
	var names []string
	for k := range f.data {
		if n, ok := strings.CutPrefix(k, mount+"/"+path+"/"); ok {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, vault.ErrNotFound
	}
	return names, nil
}

func (f fakeKV) Read(_ context.Context, mount, path string) (map[string]any, error) {
	d, ok := f.data[mount+"/"+path]
	if !ok {
		return nil, vault.ErrNotFound
	}
	return d, nil
}

func TestVaultSource(t *testing.T) {
	ctx := context.Background()
	empty := Vault{Client: fakeKV{}, Mount: "secret", Prefix: "aisa"}
	if names, err := empty.Names(ctx); err != nil || len(names) != 0 {
		t.Errorf("no consumers: %v, %v; want an empty list", names, err)
	}

	v := Vault{Client: fakeKV{data: map[string]map[string]any{
		"secret/aisa/consumers/chat-ui": {"key_sha256": hashOf("k")},
	}}, Mount: "secret", Prefix: "aisa"}
	names, err := v.Names(ctx)
	if err != nil || len(names) != 1 || names[0] != "chat-ui" {
		t.Fatalf("Names = %v, %v", names, err)
	}
	if _, err := v.Read(ctx, "gone"); !errors.Is(err, ErrGone) {
		t.Errorf("Read of a deleted consumer: %v, want ErrGone", err)
	}
	s, _ := newStore(v)
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if c, res := s.Lookup(ctx, "k"); res != Found || c.Name != "chat-ui" {
		t.Errorf("Lookup = %+v, %v", c, res)
	}
}

// slowSource blocks until its context is done, like a Vault that does not answer.
type slowSource struct{ fakeSource }

func (s *slowSource) Names(ctx context.Context) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestUnknownKeyReloadIsBounded(t *testing.T) {
	src := &slowSource{}
	s := New(src, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Refresh: time.Minute, MissRefresh: time.Millisecond, MaxStale: time.Minute,
	})
	// Loaded once, so lookups can be answered; then Vault stops answering.
	s.mu.Lock()
	s.byHash, s.loadedAt = map[string]Consumer{}, time.Now()
	s.mu.Unlock()
	time.Sleep(2 * time.Millisecond)

	start := time.Now()
	if _, res := s.Lookup(context.Background(), "unknown"); res != Unknown {
		t.Errorf("result = %v, want Unknown", res)
	}
	if d := time.Since(start); d > missLoadTimeout+time.Second {
		t.Errorf("lookup took %s, want it bounded by %s", d, missLoadTimeout)
	}
}
