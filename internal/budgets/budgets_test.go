package budgets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestBudgets(t *testing.T) {
	b := NewBudgets(nil, discard, Options{})
	if _, err := b.Get("chat-ui"); !errors.Is(err, ErrNotLoaded) {
		t.Errorf("before a load: err = %v, want ErrNotLoaded", err)
	}
	if b.Ready(context.Background()) == nil {
		t.Error("ready before a load")
	}
	var stats LoadStats
	b.opts.OnLoad = func(s LoadStats) { stats = s }
	b.apply([]consul.Pair{
		{Key: ""},
		{Key: "chat-ui", Value: []byte(`{"monthly_tokens": 20000000, "soft_ratio": 0.9, "on_exhausted": "reject"}`)},
		{Key: "batch-jobs", Value: []byte(`{"monthly_tokens": 5000000, "on_exhausted": "downgrade", "downgrade_to": "qwen3", "note": "ignored"}`)},
		{Key: "minimal", Value: []byte(`{"monthly_tokens": 0}`)},
		{Key: "no-tokens", Value: []byte(`{"soft_ratio": 0.5}`)},
		{Key: "negative", Value: []byte(`{"monthly_tokens": -1}`)},
		{Key: "ratio", Value: []byte(`{"monthly_tokens": 1, "soft_ratio": 1.5}`)},
		{Key: "mode", Value: []byte(`{"monthly_tokens": 1, "on_exhausted": "ignore"}`)},
		{Key: "no-target", Value: []byte(`{"monthly_tokens": 1, "on_exhausted": "downgrade"}`)},
		{Key: "text", Value: []byte(`5000000`)},
	})
	if b.Ready(context.Background()) != nil {
		t.Error("not ready after a load")
	}
	if stats.Valid != 3 || stats.Invalid != 6 {
		t.Errorf("stats = %+v, want 3 valid and 6 invalid", stats)
	}
	if bud, err := b.Get("chat-ui"); err != nil || bud != (Budget{MonthlyTokens: 20000000, SoftRatio: 0.9, OnExhausted: Reject}) {
		t.Errorf("chat-ui = %+v, %v", bud, err)
	}
	if bud, err := b.Get("batch-jobs"); err != nil || bud != (Budget{MonthlyTokens: 5000000, SoftRatio: DefaultSoftRatio, OnExhausted: Downgrade, DowngradeTo: "qwen3"}) {
		t.Errorf("batch-jobs = %+v, %v", bud, err)
	}
	if bud, err := b.Get("minimal"); err != nil || bud.MonthlyTokens != 0 || bud.OnExhausted != Reject {
		t.Errorf("minimal = %+v, %v", bud, err)
	}
	if _, err := b.Get("nobody"); !errors.Is(err, ErrNoBudget) {
		t.Errorf("nobody: err = %v, want ErrNoBudget", err)
	}
	for _, name := range []string{"no-tokens", "negative", "ratio", "mode", "no-target", "text"} {
		if _, err := b.Get(name); err == nil || errors.Is(err, ErrNoBudget) {
			t.Errorf("%s: err = %v, want the parse error", name, err)
		}
	}
	if n := len(b.Names()); n != 3 {
		t.Errorf("Names = %d, want 3", n)
	}
}

// fakeKV is Consul KV in memory.
type fakeKV struct {
	mu    sync.Mutex
	data  map[string][]byte
	puts  int
	fail  bool
	lists int
}

func (f *fakeKV) List(_ context.Context, prefix string, _ uint64, _ time.Duration) ([]consul.Pair, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.fail {
		return nil, 0, errors.New("consul is down")
	}
	var pairs []consul.Pair
	for k, v := range f.data {
		if strings.HasPrefix(k, prefix) {
			pairs = append(pairs, consul.Pair{Key: k, Value: v})
		}
	}
	return pairs, 1, nil
}

func (f *fakeKV) Put(_ context.Context, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.fail {
		return errors.New("consul is down")
	}
	if f.data == nil {
		f.data = make(map[string][]byte)
	}
	f.data[key] = value
	return nil
}

func (f *fakeKV) tokens(t *testing.T, key string) int64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var s state
	if err := json.Unmarshal(f.data[key], &s); err != nil {
		t.Fatalf("state %s = %q: %v", key, f.data[key], err)
	}
	return s.Tokens
}

type paidModels map[string]bool

func (p paidModels) Paid(model string) bool { return p[model] }

// newMonthly returns budgets with chat-ui (reject) and batch-jobs (downgrade to qwen3), a
// counter in miniredis, and the clock at 2026-10-21 12:00 in Helsinki.
func newMonthly(t *testing.T, kv *fakeKV) (*Monthly, *miniredis.Miniredis, *metrics.Metrics) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	b := NewBudgets(nil, discard, Options{})
	b.apply([]consul.Pair{
		{Key: "chat-ui", Value: []byte(`{"monthly_tokens": 1000, "soft_ratio": 0.8}`)},
		{Key: "batch-jobs", Value: []byte(`{"monthly_tokens": 100, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}`)},
		{Key: "broken", Value: []byte(`{}`)},
	})
	loc, err := time.LoadLocation("Europe/Helsinki")
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New("test")
	mo := NewMonthly(b, NewMonths(rdb, "aisa:"), kv, paidModels{"cloud-large": true}, m, discard, MonthlyOptions{
		Location:    loc,
		StatePrefix: "aisa/state/",
		Now:         func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, loc) },
	})
	return mo, mr, m
}

func TestPeriod(t *testing.T) {
	mo, _, _ := newMonthly(t, &fakeKV{})
	// 2026-10-31 23:30 in Helsinki is 21:30 UTC: October there, and the end is November's start.
	loc := mo.opts.Location
	period, end := mo.Period(time.Date(2026, 10, 31, 21, 30, 0, 0, time.UTC))
	if period != "2026-10" || !end.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, loc)) {
		t.Errorf("period = %s, end = %v", period, end)
	}
	// 2026-11-01 00:30 in Helsinki is still October in UTC, but November there.
	if period, _ := mo.Period(time.Date(2026, 10, 31, 22, 30, 0, 0, time.UTC)); period != "2026-11" {
		t.Errorf("period = %s, want 2026-11", period)
	}
}

func TestCheckAndCharge(t *testing.T) {
	ctx := context.Background()
	kv := &fakeKV{}
	mo, mr, m := newMonthly(t, kv)

	if v := mo.Check(ctx, "nobody", "cloud-large"); v.Outcome != NoBudget {
		t.Errorf("no budget: %+v", v)
	}
	if v := mo.Check(ctx, "broken", "cloud-large"); v.Outcome != Unavailable || v.Err == nil {
		t.Errorf("broken budget: %+v", v)
	}
	v := mo.Check(ctx, "chat-ui", "cloud-large")
	if v.Outcome != Allow || !v.Applies || v.Limit != 1000 || v.Used != 0 || v.Remaining != 1000 || v.Soft {
		t.Errorf("fresh: %+v", v)
	}

	// 800 of 1000 used: soft, still allowed; 1000: exhausted on a priced model only.
	if err := mo.Charge(ctx, "chat-ui", 800, time.Time{}); err != nil {
		t.Fatal(err)
	}
	v = mo.Check(ctx, "chat-ui", "cloud-large")
	if v.Outcome != Allow || !v.Soft || v.Remaining != 200 {
		t.Errorf("soft: %+v", v)
	}
	if err := mo.Charge(ctx, "chat-ui", 200, time.Time{}); err != nil {
		t.Fatal(err)
	}
	v = mo.Check(ctx, "chat-ui", "cloud-large")
	if v.Outcome != Exhausted || v.Remaining != 0 || v.OnExhausted != Reject {
		t.Errorf("exhausted: %+v", v)
	}
	if v := mo.Check(ctx, "chat-ui", "qwen3"); v.Outcome != Allow || v.Applies || v.Remaining != 0 {
		t.Errorf("exhausted, free model: %+v", v)
	}
	if n := testutil.ToFloat64(m.BudgetSpent.WithLabelValues("chat-ui", "2026-10")); n != 1000 {
		t.Errorf("aisa_budget_spent = %v, want 1000", n)
	}

	// The downgrade budget: its verdict names the model.
	if err := mo.Charge(ctx, "batch-jobs", 100, time.Time{}); err != nil {
		t.Fatal(err)
	}
	v = mo.Check(ctx, "batch-jobs", "cloud-large")
	if v.Outcome != Exhausted || v.OnExhausted != Downgrade || v.DowngradeTo != "qwen3" {
		t.Errorf("downgrade: %+v", v)
	}

	// Nothing is charged to nobody, or without tokens.
	if err := mo.Charge(ctx, "", 5, time.Time{}); err != nil {
		t.Error(err)
	}
	if err := mo.Charge(ctx, metrics.ConsumerUnknown, 5, time.Time{}); err != nil {
		t.Error(err)
	}
	if err := mo.Charge(ctx, "chat-ui", 0, time.Time{}); err != nil {
		t.Error(err)
	}
	if keys := mr.Keys(); len(keys) != 2 {
		t.Errorf("redis keys = %v, want chat-ui's and batch-jobs' month", keys)
	}

	// A charge in another month counts there; the key expires after the month and a while.
	if err := mo.Charge(ctx, "chat-ui", 5, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if n, _ := mr.Get("aisa:budget:{chat-ui}:2026-10"); n != "1000" {
		t.Errorf("october = %s, want 1000", n)
	}
	if n, _ := mr.Get("aisa:budget:{chat-ui}:2026-10"); n != "1000" {
		t.Errorf("october = %s", n)
	}
	if n, _ := mr.Get("aisa:budget:{chat-ui}:2026-10"); n == "" {
		t.Error("october missing")
	}
	if ttl := mr.TTL("aisa:budget:{chat-ui}:2026-10"); ttl <= 0 || ttl > 46*24*time.Hour {
		t.Errorf("october ttl = %v", ttl)
	}

	// Persist: the dirty months are copied to Consul, then nothing is until the next charge.
	mo.persist(ctx)
	if got := kv.tokens(t, "aisa/state/chat-ui/2026-10"); got != 1000 {
		t.Errorf("persisted october = %d, want 1000", got)
	}
	if got := kv.tokens(t, "aisa/state/chat-ui/2026-09"); got != 5 {
		t.Errorf("persisted september = %d, want 5", got)
	}
	if got := kv.tokens(t, "aisa/state/batch-jobs/2026-10"); got != 100 {
		t.Errorf("persisted batch-jobs = %d, want 100", got)
	}
	puts := kv.puts
	mo.persist(ctx)
	if kv.puts != puts {
		t.Errorf("persist without a charge wrote %d times", kv.puts-puts)
	}

	// Redis loses its data: the persisted total stands, for the check and as the floor of
	// the next charge.
	mr.FlushAll()
	if v := mo.Check(ctx, "chat-ui", "cloud-large"); v.Outcome != Exhausted || v.Used != 1000 {
		t.Errorf("after a flush: %+v", v)
	}
	if err := mo.Charge(ctx, "batch-jobs", 1, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n, _ := mr.Get("aisa:budget:{batch-jobs}:2026-10"); n != "101" {
		t.Errorf("batch-jobs after a flush = %s, want 101 (the floor and the charge)", n)
	}

	// Redis down: the check uses the persisted total, the charge fails and is counted.
	mr.Close()
	if v := mo.Check(ctx, "chat-ui", "cloud-large"); v.Outcome != Exhausted || v.Used != 1000 {
		t.Errorf("redis down: %+v", v)
	}
	if err := mo.Charge(ctx, "chat-ui", 1, time.Time{}); err == nil {
		t.Error("a charge with Redis down succeeded")
	}
	if n := testutil.ToFloat64(m.BudgetErrors.WithLabelValues(metrics.BudgetOpCharge)); n != 1 {
		t.Errorf("aisa_budget_errors_total{charge} = %v, want 1", n)
	}
	if n := testutil.ToFloat64(m.BudgetErrors.WithLabelValues(metrics.BudgetOpCheck)); n != 1 {
		t.Errorf("aisa_budget_errors_total{check} = %v, want 1", n)
	}
}

func TestLoadAndObserve(t *testing.T) {
	ctx := context.Background()
	kv := &fakeKV{data: map[string][]byte{
		"aisa/state/chat-ui/2026-10":    []byte(`{"tokens": 900}`),
		"aisa/state/chat-ui/2026-09":    []byte(`{"tokens": 50}`),
		"aisa/state/batch-jobs/2026-10": []byte(`garbage`),
		"aisa/state/odd":                []byte(`{"tokens": 1}`),
	}}
	mo, _, m := newMonthly(t, kv)
	mo.load(ctx)
	if v := mo.Check(ctx, "chat-ui", "cloud-large"); v.Used != 900 || v.Outcome != Allow || !v.Soft {
		t.Errorf("after load: %+v", v)
	}
	if v := mo.Check(ctx, "batch-jobs", "cloud-large"); v.Used != 0 {
		t.Errorf("unreadable state counted: %+v", v)
	}
	// A charge starts the counter from the persisted total.
	if err := mo.Charge(ctx, "chat-ui", 100, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if v := mo.Check(ctx, "chat-ui", "cloud-large"); v.Used != 1000 || v.Outcome != Exhausted {
		t.Errorf("after a charge on the floor: %+v", v)
	}

	mo.observe(ctx)
	if n := testutil.ToFloat64(m.BudgetLimit.WithLabelValues("batch-jobs", "2026-10")); n != 100 {
		t.Errorf("aisa_budget_limit{batch-jobs} = %v, want 100", n)
	}
	if n := testutil.ToFloat64(m.BudgetSpent.WithLabelValues("batch-jobs", "2026-10")); n != 0 {
		t.Errorf("aisa_budget_spent{batch-jobs} = %v, want 0", n)
	}
	if n := testutil.ToFloat64(m.BudgetSpent.WithLabelValues("chat-ui", "2026-10")); n != 1000 {
		t.Errorf("aisa_budget_spent{chat-ui} = %v, want 1000", n)
	}

	// Consul down at the start: the totals are empty, and the error is counted.
	down := &fakeKV{fail: true}
	mo2, _, m2 := newMonthly(t, down)
	mo2.load(ctx)
	if v := mo2.Check(ctx, "chat-ui", "cloud-large"); v.Used != 0 {
		t.Errorf("after a failed load: %+v", v)
	}
	if n := testutil.ToFloat64(m2.BudgetErrors.WithLabelValues(metrics.BudgetOpPersist)); n != 1 {
		t.Errorf("aisa_budget_errors_total{persist} = %v, want 1", n)
	}
	// A persist that fails keeps the month dirty for the next one.
	if err := mo2.Charge(ctx, "chat-ui", 1, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mo2.persist(ctx)
	if len(mo2.dirty) != 1 {
		t.Errorf("dirty after a failed persist = %v, want chat-ui's month", mo2.dirty)
	}
	down.mu.Lock()
	down.fail = false
	down.mu.Unlock()
	mo2.persist(ctx)
	if got := down.tokens(t, "aisa/state/chat-ui/2026-10"); got != 1 || len(mo2.dirty) != 0 {
		t.Errorf("persisted after Consul is back = %d, dirty = %v", got, mo2.dirty)
	}
}

func TestRun(t *testing.T) {
	kv := &fakeKV{}
	mo, _, _ := newMonthly(t, kv)
	mo.opts.Persist = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { mo.Run(ctx); close(done) }()
	if err := mo.Charge(ctx, "chat-ui", 7, time.Time{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kv.mu.Lock()
		_, ok := kv.data["aisa/state/chat-ui/2026-10"]
		kv.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if got := kv.tokens(t, "aisa/state/chat-ui/2026-10"); got != 7 {
		t.Errorf("persisted by Run = %d, want 7", got)
	}
}

// A broken budget denies its consumer on priced models only; on a free model the request is
// allowed, with the error and without the budget's numbers.
func TestBrokenBudgetOnFreeModel(t *testing.T) {
	ctx := context.Background()
	mo, _, _ := newMonthly(t, &fakeKV{})
	if v := mo.Check(ctx, "broken", "cloud-large"); v.Outcome != Unavailable || !v.Applies || v.Err == nil {
		t.Errorf("priced: %+v", v)
	}
	if v := mo.Check(ctx, "broken", "qwen3"); v.Outcome != Allow || v.Applies || v.Err == nil || v.Limit != 0 {
		t.Errorf("free: %+v", v)
	}
	// Before the budgets are read, the same.
	unread := NewMonthly(NewBudgets(nil, discard, Options{}), mo.counter, &fakeKV{}, paidModels{"cloud-large": true}, metrics.New("test"), discard, MonthlyOptions{})
	if v := unread.Check(ctx, "chat-ui", "cloud-large"); v.Outcome != Unavailable {
		t.Errorf("unread, priced: %+v", v)
	}
	if v := unread.Check(ctx, "chat-ui", "qwen3"); v.Outcome != Allow || v.Err == nil {
		t.Errorf("unread, free: %+v", v)
	}
}

// After the counter has failed, the persisted total is used without asking for a second: a
// Redis that is down costs one timeout a second, not one per request.
func TestCounterBackoff(t *testing.T) {
	ctx := context.Background()
	mo, mr, m := newMonthly(t, &fakeKV{})
	now := mo.opts.Now()
	clock := now
	mo.opts.Now = func() time.Time { return clock }
	if err := mo.Charge(ctx, "chat-ui", 10, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mo.persist(ctx)
	mr.Close()
	for i := 0; i < 5; i++ {
		if v := mo.Check(ctx, "chat-ui", "cloud-large"); v.Used != 10 {
			t.Errorf("check %d with Redis down: %+v", i, v)
		}
	}
	if n := testutil.ToFloat64(m.BudgetErrors.WithLabelValues(metrics.BudgetOpCheck)); n != 1 {
		t.Errorf("aisa_budget_errors_total{check} after 5 checks = %v, want 1", n)
	}
	clock = now.Add(2 * time.Second)
	mo.Check(ctx, "chat-ui", "cloud-large")
	if n := testutil.ToFloat64(m.BudgetErrors.WithLabelValues(metrics.BudgetOpCheck)); n != 2 {
		t.Errorf("aisa_budget_errors_total{check} after the backoff = %v, want 2", n)
	}
}

// The gauges of a budget that is removed go with it.
func TestObserveRemovesGoneBudgets(t *testing.T) {
	ctx := context.Background()
	mo, _, m := newMonthly(t, &fakeKV{})
	mo.observe(ctx)
	if n := testutil.CollectAndCount(m.BudgetLimit); n != 2 {
		t.Fatalf("aisa_budget_limit series = %d, want 2", n)
	}
	mo.budgets.apply([]consul.Pair{{Key: "chat-ui", Value: []byte(`{"monthly_tokens": 1000}`)}})
	mo.observe(ctx)
	if n := testutil.CollectAndCount(m.BudgetLimit); n != 1 {
		t.Errorf("aisa_budget_limit series after batch-jobs is gone = %d, want 1", n)
	}
	if n := testutil.CollectAndCount(m.BudgetSpent); n != 1 {
		t.Errorf("aisa_budget_spent series after batch-jobs is gone = %d, want 1", n)
	}
	if n := testutil.ToFloat64(m.BudgetLimit.WithLabelValues("chat-ui", "2026-10")); n != 1000 {
		t.Errorf("chat-ui's limit = %v", n)
	}
}

func TestParseAndValue(t *testing.T) {
	for _, bud := range []Budget{
		{MonthlyTokens: 20000000, SoftRatio: 0.9, OnExhausted: Reject},
		{MonthlyTokens: 0, SoftRatio: DefaultSoftRatio, OnExhausted: Reject},
		{MonthlyTokens: 5000000, SoftRatio: 1, OnExhausted: Downgrade, DowngradeTo: "qwen3"},
	} {
		got, err := Parse(bud.Value())
		if err != nil || got != bud {
			t.Errorf("Parse(%s) = %+v, %v; want %+v", bud.Value(), got, err, bud)
		}
	}
	// Parse is the loader's: the same value is refused with the same message.
	value := []byte(`{"monthly_tokens": 1, "on_exhausted": "downgrade"}`)
	_, want := parseBudget(value)
	if _, err := Parse(value); err == nil || err.Error() != want.Error() {
		t.Errorf("Parse = %v, want %v", err, want)
	}
}

func TestUsage(t *testing.T) {
	ctx := context.Background()
	mo, _, _ := newMonthly(t, &fakeKV{})
	if period, used := mo.Usage(ctx, "chat-ui"); period != "2026-10" || used != 0 {
		t.Errorf("fresh: %s %d", period, used)
	}
	if err := mo.Charge(ctx, "chat-ui", 300, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, used := mo.Usage(ctx, "chat-ui"); used != 300 {
		t.Errorf("used = %d, want 300", used)
	}
	// A consumer without a budget still has its month counted, as Charge counts it.
	if _, used := mo.Usage(ctx, "nobody"); used != 0 {
		t.Errorf("nobody used = %d", used)
	}
}
