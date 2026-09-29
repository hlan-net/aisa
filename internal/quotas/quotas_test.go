package quotas

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// loaded returns profiles that have read pairs.
func loaded(pairs ...consul.Pair) *Profiles {
	p := NewProfiles(nil, discard, ProfileOptions{})
	p.apply(pairs)
	return p
}

func TestProfiles(t *testing.T) {
	p := NewProfiles(nil, discard, ProfileOptions{})
	if _, err := p.Get("batch"); !errors.Is(err, ErrNotLoaded) {
		t.Errorf("before a load: err = %v, want ErrNotLoaded", err)
	}
	if p.Ready(context.Background()) == nil {
		t.Error("ready before a load")
	}

	var stats LoadStats
	p.opts.OnLoad = func(s LoadStats) { stats = s }
	p.apply([]consul.Pair{
		{Key: ""}, // the prefix itself
		{Key: "sub/"},
		{Key: "batch", Value: []byte(`{"tokens_per_hour": 200000}`)},
		{Key: "blocked", Value: []byte(`{"tokens_per_hour": 0, "note": "unknown fields are ignored"}`)},
		{Key: "typo", Value: []byte(`{"token_per_hour": 5}`)},
		{Key: "negative", Value: []byte(`{"tokens_per_hour": -1}`)},
		{Key: "fraction", Value: []byte(`{"tokens_per_hour": 1.5}`)},
		{Key: "text", Value: []byte(`200000`)},
	})
	if p.Ready(context.Background()) != nil {
		t.Error("not ready after a load")
	}
	if stats.Valid != 2 || stats.Invalid != 4 {
		t.Errorf("stats = %+v, want 2 valid and 4 invalid", stats)
	}
	if pr, err := p.Get("batch"); err != nil || pr.TokensPerHour != 200000 {
		t.Errorf("batch = %+v, %v", pr, err)
	}
	if pr, err := p.Get("blocked"); err != nil || pr.TokensPerHour != 0 {
		t.Errorf("blocked = %+v, %v", pr, err)
	}
	for _, name := range []string{"typo", "negative", "fraction", "text"} {
		if _, err := p.Get(name); err == nil || errors.Is(err, ErrUnknownProfile) {
			t.Errorf("%s: err = %v, want a parse error", name, err)
		}
	}
	if _, err := p.Get("nope"); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("nope: err = %v, want ErrUnknownProfile", err)
	}
}

// fakeSource answers List from a queue of answers, then blocks until ctx is done.
type fakeSource struct {
	mu      sync.Mutex
	answers []answer
	indexes []uint64 // the index of every call
}

type answer struct {
	pairs []consul.Pair
	index uint64
	err   error
}

func (f *fakeSource) List(ctx context.Context, index uint64) ([]consul.Pair, uint64, error) {
	f.mu.Lock()
	f.indexes = append(f.indexes, index)
	if len(f.answers) == 0 {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	f.mu.Unlock()
	return a.pairs, a.index, a.err
}

func (f *fakeSource) calls() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.indexes...)
}

func TestProfilesRun(t *testing.T) {
	src := &fakeSource{answers: []answer{
		{err: errors.New("consul is down")},
		{pairs: []consul.Pair{{Key: "batch", Value: []byte(`{"tokens_per_hour": 10}`)}}, index: 5},
		{pairs: []consul.Pair{{Key: "batch", Value: []byte(`{"tokens_per_hour": 99}`)}}, index: 5}, // timed out: ignored
		{err: errors.New("consul is down again")},                                                  // keeps what it has
		{pairs: []consul.Pair{{Key: "batch", Value: []byte(`{"tokens_per_hour": 20}`)}}, index: 9},
		{pairs: nil, index: 2}, // the index went backwards
	}}
	var mu sync.Mutex
	var loads []LoadStats
	p := NewProfiles(src, discard, ProfileOptions{
		RetryMin: time.Millisecond, RetryMax: 2 * time.Millisecond,
		OnLoad: func(s LoadStats) { mu.Lock(); loads = append(loads, s); mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(src.calls()) < 7 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got, want := src.calls(), []uint64{0, 0, 5, 5, 5, 9, 0}; !equal(got, want) {
		t.Errorf("indexes asked = %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	// error, load, error, load, load (empty)
	if len(loads) != 5 || loads[0].Err == nil || loads[1].Valid != 1 || loads[2].Err == nil || loads[4].Valid != 0 {
		t.Errorf("loads = %+v", loads)
	}
	if _, err := p.Get("batch"); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("after the empty load: err = %v, want ErrUnknownProfile", err)
	}
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newWindow(t *testing.T) (*Window, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewWindow(rdb, "aisa:"), mr
}

func TestWindow(t *testing.T) {
	w, mr := newWindow(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 12, 30, 20, 0, time.UTC)
	mr.SetTime(now) // for the expiry of the buckets

	for _, add := range []struct {
		ago    time.Duration
		tokens int64
	}{
		{0, 5}, {10 * time.Second, 7}, // this minute
		{59 * time.Minute, 11},   // the oldest minute in the window
		{61 * time.Minute, 1000}, // outside the window
	} {
		if err := w.Add(ctx, "chat-ui", add.tokens, now.Add(-add.ago)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Add(ctx, "batch-jobs", 3, now); err != nil {
		t.Fatal(err)
	}

	usage, err := w.Usage(ctx, "chat-ui", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != buckets || usage[buckets-1] != 12 || usage[0] != 11 {
		t.Errorf("usage: newest %d, oldest %d, %d buckets; want 12, 11, %d", usage[buckets-1], usage[0], len(usage), buckets)
	}
	var sum int64
	for _, n := range usage {
		sum += n
	}
	if sum != 23 {
		t.Errorf("sum = %d, want 23", sum)
	}

	key := "aisa:quota:{chat-ui}:" + itoa(minute(now))
	if mr.TTL(key) <= 0 {
		t.Errorf("%s has no expiry", key)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestWindowRedisDown(t *testing.T) {
	w, mr := newWindow(t)
	mr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Add(ctx, "chat-ui", 1, time.Now()); err == nil {
		t.Error("Add: no error")
	}
	if _, err := w.Usage(ctx, "chat-ui", time.Now()); err == nil {
		t.Error("Usage: no error")
	}
}

func TestWindowBrokenBucket(t *testing.T) {
	w, mr := newWindow(t)
	now := time.Now()
	if err := mr.Set("aisa:quota:{chat-ui}:"+itoa(minute(now)), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Usage(context.Background(), "chat-ui", now); err == nil {
		t.Error("no error for a bucket that is not a number")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 30, 20, 0, time.UTC)
	usage := make([]int64, buckets)
	usage[0], usage[1], usage[buckets-1] = 50, 30, 30 // 110 tokens

	// Limit 100: the oldest bucket (minute 11:31) leaves at 12:31:00, 40 s from now.
	if got := retryAfter(usage, 100, now); got != 40*time.Second {
		t.Errorf("limit 100: %s, want 40s", got)
	}
	// Limit 50: after the two oldest leave, 30 are left: at 12:32:00.
	if got := retryAfter(usage, 50, now); got != 100*time.Second {
		t.Errorf("limit 50: %s, want 1m40s", got)
	}
	// Limit 0 is never reached.
	if got := retryAfter(usage, 0, now); got != window {
		t.Errorf("limit 0: %s, want %s", got, window)
	}
	// Never below a second.
	if got := retryAfter(usage, 100, time.Date(2026, 9, 29, 12, 30, 59, 999e6, time.UTC)); got != time.Second {
		t.Errorf("at the end of a minute: %s, want 1s", got)
	}
}

// fakeCounter is a Counter with fixed usage or an error.
type fakeCounter struct {
	usage []int64
	err   error
	added []int64
	at    []time.Time
}

func (f *fakeCounter) Add(_ context.Context, _ string, tokens int64, at time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.added = append(f.added, tokens)
	f.at = append(f.at, at)
	return nil
}

func (f *fakeCounter) Usage(context.Context, string, time.Time) ([]int64, error) {
	return f.usage, f.err
}

func TestCheck(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 30, 20, 0, time.UTC)
	profiles := loaded(
		consul.Pair{Key: "batch", Value: []byte(`{"tokens_per_hour": 100}`)},
		consul.Pair{Key: "broken", Value: []byte(`{}`)},
	)
	usage := func(total int64) []int64 { u := make([]int64, buckets); u[0] = total; return u }

	for _, tc := range []struct {
		name     string
		profiles *Profiles
		consumer consumers.Consumer
		counter  *fakeCounter
		want     Outcome
		errors   float64
	}{
		{"no profile", profiles, consumers.Consumer{Name: "a"}, &fakeCounter{}, Allow, 0},
		{"below the limit", profiles, consumers.Consumer{Name: "a", QuotaProfile: "batch"}, &fakeCounter{usage: usage(99)}, Allow, 0},
		{"at the limit", profiles, consumers.Consumer{Name: "a", QuotaProfile: "batch"}, &fakeCounter{usage: usage(100)}, Exhausted, 0},
		{"unknown profile", profiles, consumers.Consumer{Name: "a", QuotaProfile: "nope"}, &fakeCounter{}, Unavailable, 0},
		{"broken profile", profiles, consumers.Consumer{Name: "a", QuotaProfile: "broken"}, &fakeCounter{}, Unavailable, 0},
		{"profiles not loaded", NewProfiles(nil, discard, ProfileOptions{}), consumers.Consumer{Name: "a", QuotaProfile: "batch"}, &fakeCounter{}, Unavailable, 0},
		{"redis down", profiles, consumers.Consumer{Name: "a", QuotaProfile: "batch"}, &fakeCounter{err: errors.New("down")}, Allow, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			q := New(tc.profiles, tc.counter, m, discard, Options{Now: func() time.Time { return now }})
			checkVerdict(t, q.Check(context.Background(), tc.consumer), tc.want)
			if got := testutil.ToFloat64(m.QuotaErrors.WithLabelValues(metrics.QuotaOpCheck)); got != tc.errors {
				t.Errorf("check errors = %v, want %v", got, tc.errors)
			}
		})
	}
}

// checkVerdict checks a verdict of TestCheck, whose exhausted consumer has used 100 of 100.
func checkVerdict(t *testing.T, v Verdict, want Outcome) {
	t.Helper()
	if v.Outcome != want {
		t.Errorf("outcome = %v, want %v (%+v)", v.Outcome, want, v)
	}
	if v.Outcome == Unavailable && v.Err == nil {
		t.Error("unavailable without an error")
	}
	if v.Outcome == Exhausted && (v.RetryAfter <= 0 || v.Used != 100 || v.Limit != 100) {
		t.Errorf("exhausted: %+v", v)
	}
}

func TestCharge(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 30, 20, 0, time.UTC)
	c := &fakeCounter{}
	m := metrics.New("test")
	q := New(loaded(), c, m, discard, Options{Now: func() time.Time { return now }})
	ctx := context.Background()

	for _, tc := range []struct {
		consumer string
		tokens   int64
		at       time.Time
	}{
		{"chat-ui", 10, now.Add(-time.Minute)},
		{"chat-ui", 20, now.Add(time.Hour)}, // in the future: now
		{"chat-ui", 30, time.Time{}},        // unknown: now
		{"chat-ui", 40, now.Add(-window)},   // left the window
		{"", 50, now},
		{metrics.ConsumerUnknown, 60, now},
		{"chat-ui", 0, now},
	} {
		if err := q.Charge(ctx, tc.consumer, tc.tokens, tc.at); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.added) != 3 || c.added[0] != 10 || c.added[1] != 20 || c.added[2] != 30 {
		t.Errorf("added = %v, want [10 20 30]", c.added)
	}
	if !c.at[1].Equal(now) || !c.at[2].Equal(now) {
		t.Errorf("times = %v, want the future and the unknown one at now", c.at)
	}

	c.err = errors.New("down")
	if err := q.Charge(ctx, "chat-ui", 1, now); err == nil {
		t.Error("no error while Redis is down")
	}
	if got := testutil.ToFloat64(m.QuotaErrors.WithLabelValues(metrics.QuotaOpCharge)); got != 1 {
		t.Errorf("charge errors = %v, want 1", got)
	}
}

// TestQuotaWithRedis runs the checks and charges against a Redis.
func TestQuotaWithRedis(t *testing.T) {
	w, mr := newWindow(t)
	now := time.Date(2026, 9, 29, 12, 30, 20, 0, time.UTC)
	mr.SetTime(now)
	clock := now
	q := New(loaded(consul.Pair{Key: "batch", Value: []byte(`{"tokens_per_hour": 100}`)}), w,
		metrics.New("test"), discard, Options{Now: func() time.Time { return clock }})
	ctx := context.Background()
	c := consumers.Consumer{Name: "batch-jobs", QuotaProfile: "batch"}

	if v := q.Check(ctx, c); v.Outcome != Allow || v.Used != 0 {
		t.Fatalf("fresh: %+v", v)
	}
	if err := q.Charge(ctx, c.Name, 60, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := q.Charge(ctx, c.Name, 50, now); err != nil {
		t.Fatal(err)
	}
	v := q.Check(ctx, c)
	if v.Outcome != Exhausted || v.Used != 110 {
		t.Fatalf("after 110 tokens: %+v", v)
	}
	// The 60 tokens of 12:00 leave the window when it begins at 12:01, at 13:00.
	if want := time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC).Sub(now); v.RetryAfter != want {
		t.Errorf("retry after %s, want %s", v.RetryAfter, want)
	}
	clock = now.Add(v.RetryAfter)
	if v := q.Check(ctx, c); v.Outcome != Allow || v.Used != 50 {
		t.Errorf("after retry-after: %+v", v)
	}
}
