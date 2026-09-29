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
	"sync/atomic"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/vault"
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type fakeSource struct {
	mu       sync.Mutex
	data     map[string]map[string]any
	fail     error            // of the listing
	failRead map[string]error // of reading a consumer
	listing  int
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
	if err := f.failRead[name]; err != nil {
		return nil, err
	}
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

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newStore(src Source) (*Store, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s := New(src, quiet(), Options{
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
	s := New(src, quiet(), Options{
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

const missTimeout = 100 * time.Millisecond

// newLoadedStore returns a store that was loaded once, so lookups can be answered.
func newLoadedStore(src Source, o Options) *Store {
	s := New(src, quiet(), o)
	s.mu.Lock()
	s.byHash, s.loadedAt = map[string]entry{}, time.Now()
	s.mu.Unlock()
	time.Sleep(2 * time.Millisecond) // past MissRefresh
	return s
}

func TestUnknownKeyReloadIsBounded(t *testing.T) {
	s := newLoadedStore(&slowSource{}, Options{
		Refresh: time.Minute, MissRefresh: time.Millisecond, MissTimeout: missTimeout, MaxStale: time.Minute,
	})
	start := time.Now()
	if _, res := s.Lookup(context.Background(), "unknown"); res != Unknown {
		t.Errorf("result = %v, want Unknown", res)
	}
	if d := time.Since(start); d > missTimeout+time.Second {
		t.Errorf("lookup took %s, want it bounded by %s", d, missTimeout)
	}
}

// blockingSource answers the listing only when it is released, like a Vault that is slow.
type blockingSource struct {
	fakeSource
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSource) Names(ctx context.Context) ([]string, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeSource.Names(ctx)
}

func TestUnknownKeyDoesNotWaitForALoadInProgress(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{}, 1), release: make(chan struct{})}
	src.data = map[string]map[string]any{}
	s := newLoadedStore(src, Options{
		Refresh: time.Minute, MissRefresh: time.Millisecond, MissTimeout: missTimeout, MaxStale: time.Minute,
	})

	// The periodic load hangs on a slow Vault, with a context that lasts.
	loaded := make(chan error, 1)
	go func() { loaded <- s.Load(context.Background()) }()
	<-src.entered
	time.Sleep(2 * time.Millisecond) // past MissRefresh since that load began

	start := time.Now()
	if _, res := s.Lookup(context.Background(), "unknown"); res != Unknown {
		t.Errorf("result = %v, want Unknown", res)
	}
	if d := time.Since(start); d > missTimeout+time.Second {
		t.Errorf("lookup took %s while a load was in progress, want it bounded by %s", d, missTimeout)
	}
	close(src.release)
	if err := <-loaded; err != nil {
		t.Errorf("the load in progress: %v", err)
	}
}

func TestRequestsThatWaitedDoNotLoadAgain(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{}, 1), release: make(chan struct{})}
	src.data = map[string]map[string]any{}
	s := newLoadedStore(src, Options{
		Refresh: time.Minute, MissRefresh: time.Millisecond, MissTimeout: 5 * time.Second, MaxStale: time.Minute,
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { s.Lookup(context.Background(), "unknown") })
	}
	<-src.entered
	time.Sleep(20 * time.Millisecond) // the other requests have joined the load by now
	close(src.release)
	wg.Wait()
	if src.listing != 1 {
		t.Errorf("%d loads for 8 requests that arrived together, want 1", src.listing)
	}
}

func TestLoadGivesUpWaitingWhenItsContextIsDone(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s := New(src, quiet(), Options{Refresh: time.Minute, MissRefresh: time.Second, MaxStale: time.Minute})
	go func() { _ = s.Load(context.Background()) }()
	<-src.entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Load(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline of the waiting caller", err)
	}
	close(src.release)
}

func TestOneUnreadableConsumerDoesNotStopTheOthers(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{
		"chat-ui":    {"key_sha256": hashOf("chat")},
		"batch-jobs": {"key_sha256": hashOf("batch")},
	}}
	s, clk := newStore(src)
	var stats Stats
	s.opts.OnLoad = func(st Stats) { stats = st }
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if stats.Consumers != 2 || stats.Keys != 2 || stats.Unreadable != 0 || stats.Err != nil {
		t.Errorf("stats = %+v, want 2 consumers, 2 keys, none unreadable", stats)
	}

	// batch-jobs cannot be read any more, and a new consumer appears.
	src.mu.Lock()
	src.failRead = map[string]error{"batch-jobs": errors.New("permission denied")}
	src.data["new-app"] = map[string]any{"key_sha256": hashOf("new")}
	src.mu.Unlock()
	clk.advance(time.Minute)
	if err := s.Load(ctx); err != nil {
		t.Fatalf("one unreadable consumer failed the load: %v", err)
	}
	for key, want := range map[string]string{"chat": "chat-ui", "new": "new-app", "batch": "batch-jobs"} {
		if c, res := s.Lookup(ctx, key); res != Found || c.Name != want {
			t.Errorf("%s: %+v, %v; want %s", key, c, res, want)
		}
	}
	if stats.Consumers != 3 || stats.Unreadable != 1 {
		t.Errorf("stats = %+v, want 3 consumers, 1 unreadable", stats)
	}

	// Its keys are kept for MaxStale, not for good: they may have been revoked since.
	clk.advance(15 * time.Minute)
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, res := s.Lookup(ctx, "batch"); res != Unknown {
		t.Errorf("a consumer unreadable for more than MaxStale: %v, want Unknown", res)
	}
	if _, res := s.Lookup(ctx, "chat"); res != Found {
		t.Errorf("the readable consumer: %v, want Found", res)
	}
}

func TestNewConsumerThatCannotBeReadIsLeftOut(t *testing.T) {
	src := &fakeSource{
		data:     map[string]map[string]any{"chat-ui": {"key_sha256": hashOf("chat")}, "bad%name": {"key_sha256": hashOf("bad")}},
		failRead: map[string]error{"bad%name": errors.New("invalid URL escape")},
	}
	s, _ := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, res := s.Lookup(ctx, "chat"); res != Found {
		t.Errorf("the readable consumer: %v, want Found", res)
	}
	if _, res := s.Lookup(ctx, "bad"); res != Unknown {
		t.Errorf("the unreadable consumer: %v, want Unknown", res)
	}
}

func TestLoadFailsWhenNoConsumerCanBeRead(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{
		"chat-ui":    {"key_sha256": hashOf("chat")},
		"batch-jobs": {"key_sha256": hashOf("batch")},
	}}
	s, clk := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	down := errors.New("vault sealed")
	src.mu.Lock()
	src.failRead = map[string]error{"chat-ui": down, "batch-jobs": down}
	src.mu.Unlock()
	clk.advance(time.Minute)
	if err := s.Load(ctx); !errors.Is(err, down) {
		t.Errorf("err = %v, want the load to fail with Vault's error", err)
	}
	if _, res := s.Lookup(ctx, "chat"); res != Found {
		t.Errorf("after the failed load: %v, want Found", res)
	}
	clk.advance(15 * time.Minute)
	if _, res := s.Lookup(ctx, "chat"); res != Unavailable {
		t.Errorf("16 min after the last load: %v, want Unavailable", res)
	}
}

func TestEmptyListRejectsEveryKey(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{"chat-ui": {"key_sha256": hashOf("chat")}}}
	s, clk := newStore(src)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	// The last consumer is deleted in Vault: that must take effect.
	src.mu.Lock()
	delete(src.data, "chat-ui")
	src.mu.Unlock()
	clk.advance(time.Minute)
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, res := s.Lookup(ctx, "chat"); res != Unknown {
		t.Errorf("after the consumer was deleted: %v, want Unknown", res)
	}
}

func TestRunRetriesAFailedLoadSoon(t *testing.T) {
	src := &fakeSource{
		data: map[string]map[string]any{"a": {"key_sha256": hashOf("k")}},
		fail: errors.New("vault is starting"),
	}
	var failures atomic.Int32
	s := New(src, quiet(), Options{
		Refresh: time.Hour, RetryMin: 5 * time.Millisecond, MissRefresh: time.Second, MaxStale: 2 * time.Hour,
		OnLoad: func(st Stats) {
			if st.Err != nil {
				failures.Add(1)
			}
			if failures.Load() == 3 {
				src.mu.Lock()
				src.fail = nil // Vault is up
				src.mu.Unlock()
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for s.Ready(ctx) != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("not ready although Vault came up: %v; with Refresh of an hour the retry must come sooner", err)
	}
	if n := failures.Load(); n != 3 {
		t.Errorf("failures = %d, want 3", n)
	}
}

func TestParseHashes(t *testing.T) {
	a, b := hashOf("a"), hashOf("b")
	for name, tc := range map[string]struct {
		in   string
		want []string
		bad  int
	}{
		"one":                    {a, []string{a}, 0},
		"comma":                  {a + "," + b, []string{a, b}, 0},
		"comma and space":        {a + ", " + b, []string{a, b}, 0},
		"lines":                  {a + "\n" + b, []string{a, b}, 0},
		"lines from a CRLF file": {a + "\r\n" + b + "\r\n", []string{a, b}, 0},
		"tabs":                   {a + "\t" + b, []string{a, b}, 0},
		"upper case":             {strings.ToUpper(a), []string{a}, 0},
		"one invalid":            {a + " zz", []string{a}, 1},
		"too short":              {"abcd", nil, 1},
		"empty":                  {"", nil, 0},
	} {
		got, bad := parseHashes(tc.in)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") || bad != tc.bad {
			t.Errorf("%s: %v, %d bad; want %v, %d bad", name, got, bad, tc.want, tc.bad)
		}
	}
}

func TestUnknownKeyJoinsALoadThatHasJustBegun(t *testing.T) {
	src := &blockingSource{entered: make(chan struct{}, 1), release: make(chan struct{})}
	src.data = map[string]map[string]any{}
	s := newLoadedStore(src, Options{
		Refresh: time.Minute, MissRefresh: time.Hour, MissTimeout: 5 * time.Second, MaxStale: time.Minute,
	})

	// A load begins, well within MissRefresh of the lookup below, and finds a new consumer.
	loaded := make(chan error, 1)
	go func() { loaded <- s.Load(context.Background()) }()
	<-src.entered
	src.mu.Lock()
	src.data["new-app"] = map[string]any{"key_sha256": hashOf("new")}
	src.mu.Unlock()

	found := make(chan Result, 1)
	go func() {
		_, res := s.Lookup(context.Background(), "new")
		found <- res
	}()
	time.Sleep(20 * time.Millisecond) // the lookup is waiting for the load by now
	close(src.release)
	if res := <-found; res != Found {
		t.Errorf("a key that the load in progress finds: %v, want Found", res)
	}
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
}

func TestKeysOfAnUnreadableConsumerExpireBetweenLoads(t *testing.T) {
	src := &fakeSource{data: map[string]map[string]any{
		"chat-ui":    {"key_sha256": hashOf("chat")},
		"batch-jobs": {"key_sha256": hashOf("batch")},
	}}
	s, clk := newStore(src)
	s.opts.MissRefresh = time.Hour // no reload on a miss: the expiry is the lookup's own
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.failRead = map[string]error{"batch-jobs": errors.New("permission denied")}
	src.mu.Unlock()
	clk.advance(time.Minute)
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}

	// 15 min 1 s after batch-jobs was read, 14 min 1 s after the last load.
	clk.advance(14*time.Minute + time.Second)
	if _, res := s.Lookup(ctx, "batch"); res != Unknown {
		t.Errorf("keys read more than MaxStale ago: %v, want Unknown", res)
	}
	if _, res := s.Lookup(ctx, "chat"); res != Found {
		t.Errorf("the readable consumer: %v, want Found", res)
	}
}

func TestRetryStopsDoublingAtRefresh(t *testing.T) {
	retry := time.Second
	for range 100 {
		retry = nextRetry(retry, time.Minute)
		if retry <= 0 || retry > time.Minute {
			t.Fatalf("retry = %s, want within (0, 1m]", retry)
		}
	}
	if retry != time.Minute {
		t.Errorf("retry = %s after 100 failures, want 1m", retry)
	}
}
