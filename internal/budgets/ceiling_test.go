package budgets

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
)

// fakeHealth is the catalog: the backends of aisa-backend.
type fakeHealth struct {
	mu        sync.Mutex
	instances []consul.Instance
	err       error
	calls     int
}

func (f *fakeHealth) Health(_ context.Context, service string) ([]consul.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if service != "aisa-backend" {
		return nil, errors.New("wrong service")
	}
	return f.instances, f.err
}

var devBackends = []consul.Instance{
	{ID: "mock-local", Address: "mock-local", Port: 8080, Passing: true,
		Meta: map[string]string{"provider": "openai-compatible", "models": "qwen3,llama3.2"}},
	{ID: "cloud-a", Address: "10.0.0.5", Port: 443, Passing: true,
		Meta: map[string]string{"provider": "openai-compatible", "models": "cloud-large, shared", "key": "openai"}},
	{ID: "cloud-b", Address: "10.0.0.6", Port: 443, Passing: false,
		Meta: map[string]string{"provider": "openai-compatible", "models": "shared,other", "key": "anthropic"}},
	{ID: "cloud-c", Address: "10.0.0.7", Port: 443, Passing: true,
		Meta: map[string]string{"provider": "openai-compatible", "models": "other", "key": "openai"}},
}

func TestCatalog(t *testing.T) {
	ctx := context.Background()
	h := &fakeHealth{err: errors.New("consul is down")}
	c := NewCatalog(h, discard, CatalogOptions{})
	if c.Loaded() || c.Refresh(ctx) {
		t.Error("loaded from a failed read")
	}
	if keys := c.KeysOf("cloud-large"); len(keys) != 0 {
		t.Errorf("keys before a load = %v", keys)
	}
	h.mu.Lock()
	h.err, h.instances = nil, devBackends
	h.mu.Unlock()
	if !c.Refresh(ctx) || !c.Loaded() {
		t.Fatal("not loaded after a read")
	}
	for model, want := range map[string][]string{
		"qwen3":       nil,
		"cloud-large": {"openai"},
		"shared":      {"anthropic", "openai"}, // sorted, the backend that is down included
		"other":       {"anthropic", "openai"},
		"nonesuch":    nil,
	} {
		if got := c.KeysOf(model); !slices.Equal(got, want) {
			t.Errorf("KeysOf(%s) = %v, want %v", model, got, want)
		}
	}
	for backend, want := range map[string]string{
		"cloud-a": "openai", "10.0.0.5": "openai", "10.0.0.5:443": "openai", " cloud-b ": "anthropic",
	} {
		if got, ok := c.KeyOf(backend); !ok || got != want {
			t.Errorf("KeyOf(%q) = %q, %v, want %q", backend, got, ok, want)
		}
	}
	for _, backend := range []string{"mock-local", "mock-local:8080", "", "nonesuch"} {
		if key, ok := c.KeyOf(backend); ok {
			t.Errorf("KeyOf(%q) = %q, want none", backend, key)
		}
	}
	// A read that fails later keeps the catalog.
	h.mu.Lock()
	h.err = errors.New("consul is down again")
	h.mu.Unlock()
	if c.Refresh(ctx) || len(c.KeysOf("cloud-large")) != 1 {
		t.Error("a failed refresh changed the catalog")
	}
}

func TestCatalogRun(t *testing.T) {
	h := &fakeHealth{instances: devBackends}
	c := NewCatalog(h, discard, CatalogOptions{Refresh: 2 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := h.calls
		h.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if !c.Loaded() {
		t.Error("Run did not load the catalog")
	}
}

// newCeiling returns a ceiling with an openai budget of 100 tokens (reject) and an anthropic one
// of 50 (downgrade to qwen3), over the dev backends.
func newCeiling(t *testing.T) (*Ceiling, *Monthly, *metrics.Metrics) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	b := NewBudgets(nil, discard, Options{})
	b.apply([]consul.Pair{
		{Key: "openai", Value: []byte(`{"monthly_tokens": 100}`)},
		{Key: "anthropic", Value: []byte(`{"monthly_tokens": 50, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}`)},
		{Key: "broken", Value: []byte(`{"monthly_tokens": -1}`)},
	})
	m := metrics.New("test")
	mo := NewMonthly(b, NewMonths(rdb, "aisa:provider-"), &fakeKV{}, Always{}, m, discard, MonthlyOptions{
		StatePrefix: "aisa/state/providers/",
		Limit:       m.ProviderBudgetLimit, Spent: m.ProviderBudgetSpent,
		Now: func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	})
	c := NewCatalog(&fakeHealth{instances: append(slices.Clone(devBackends), consul.Instance{
		ID: "cloud-d", Address: "10.0.0.8", Port: 443, Meta: map[string]string{"models": "odd", "key": "broken"},
	})}, discard, CatalogOptions{})
	c.Refresh(context.Background())
	return NewCeiling(c, mo, discard), mo, m
}

func TestCeiling(t *testing.T) {
	ctx := context.Background()
	ce, mo, m := newCeiling(t)

	// Before the catalog is read, the ceiling is not ready; the budgets are.
	notLoaded := NewCeiling(NewCatalog(&fakeHealth{}, discard, CatalogOptions{}), mo, discard)
	if err := notLoaded.Ready(ctx); !errors.Is(err, errCatalogNotLoaded) {
		t.Errorf("Ready before the catalog = %v", err)
	}
	if err := ce.Ready(ctx); err != nil {
		t.Errorf("Ready = %v", err)
	}

	// A local model has no provider budget; a priced one has its key's.
	if v := ce.Check(ctx, "qwen3"); v.Outcome != NoBudget {
		t.Errorf("qwen3: %+v", v)
	}
	if v := ce.Check(ctx, "cloud-large"); v.Outcome != Allow || v.Provider != "openai" || v.Remaining != 100 || !v.Applies {
		t.Errorf("cloud-large: %+v", v)
	}
	// A model whose key has a broken budget cannot be served.
	if v := ce.Check(ctx, "odd"); v.Outcome != Unavailable || v.Provider != "broken" {
		t.Errorf("odd: %+v", v)
	}

	// Charged by the backend's key when it is known: cloud-a is openai.
	if err := ce.Charge(ctx, "shared", "cloud-a", 100, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := testutil.ToFloat64(m.ProviderBudgetSpent.WithLabelValues("openai", "2026-10")); n != 100 {
		t.Errorf("aisa_provider_budget_spent{openai} = %v, want 100", n)
	}
	if n := testutil.ToFloat64(m.ProviderBudgetSpent.WithLabelValues("anthropic", "2026-10")); n != 0 {
		t.Errorf("aisa_provider_budget_spent{anthropic} = %v, want 0", n)
	}
	// openai is exhausted: every model it serves is, whatever the other keys say.
	if v := ce.Check(ctx, "cloud-large"); v.Outcome != Exhausted || v.Provider != "openai" || v.OnExhausted != Reject {
		t.Errorf("cloud-large exhausted: %+v", v)
	}
	if v := ce.Check(ctx, "shared"); v.Outcome != Exhausted {
		t.Errorf("shared with openai exhausted: %+v", v)
	}
	// anthropic alone: a downgrade.
	if err := ce.Charge(ctx, "other", "10.0.0.6:443", 50, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if v := ce.Check(ctx, "other"); v.Outcome != Exhausted || v.Provider != "anthropic" || v.DowngradeTo != "qwen3" {
		t.Errorf("other: %+v", v)
	}

	// A backend the catalog does not know: the model's keys are charged, every one of them.
	if err := ce.Charge(ctx, "shared", "203.0.113.9", 7, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := testutil.ToFloat64(m.ProviderBudgetSpent.WithLabelValues("openai", "2026-10")); n != 107 {
		t.Errorf("openai after an unknown backend = %v, want 107", n)
	}
	if n := testutil.ToFloat64(m.ProviderBudgetSpent.WithLabelValues("anthropic", "2026-10")); n != 57 {
		t.Errorf("anthropic after an unknown backend = %v, want 57", n)
	}
	// A model without a key: nothing to charge, nothing fails.
	if err := ce.Charge(ctx, "qwen3", "mock-local", 1000, time.Time{}); err != nil {
		t.Error(err)
	}
	if err := ce.Charge(ctx, "cloud-large", "cloud-a", 0, time.Time{}); err != nil {
		t.Error(err)
	}
}

// Two keyed backends on one address with different ports: the address names neither, the
// ID and the address:port still do.
func TestCatalogSharedAddress(t *testing.T) {
	c := NewCatalog(&fakeHealth{instances: []consul.Instance{
		{ID: "a", Address: "10.0.0.9", Port: 8001, Meta: map[string]string{"models": "m1", "key": "openai"}},
		{ID: "b", Address: "10.0.0.9", Port: 8002, Meta: map[string]string{"models": "m2", "key": "anthropic"}},
		{ID: "c", Address: "10.0.0.10", Port: 8001, Meta: map[string]string{"models": "m1", "key": "openai"}},
		{ID: "d", Address: "10.0.0.10", Port: 8002, Meta: map[string]string{"models": "m1", "key": "openai"}},
	}}, discard, CatalogOptions{})
	c.Refresh(context.Background())
	if key, ok := c.KeyOf("10.0.0.9"); ok {
		t.Errorf("KeyOf(shared address) = %q, want none", key)
	}
	for name, want := range map[string]string{"a": "openai", "b": "anthropic", "10.0.0.9:8001": "openai", "10.0.0.9:8002": "anthropic", "10.0.0.10": "openai"} {
		if key, ok := c.KeyOf(name); !ok || key != want {
			t.Errorf("KeyOf(%s) = %q, %v, want %q", name, key, ok, want)
		}
	}
}
