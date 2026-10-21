package budgets

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
	"github.com/hlan-net/aisa/internal/metrics"
)

// Counter counts a consumer's tokens per month; Months (Redis) implements it.
type Counter interface {
	// Add counts tokens in a period and returns its total, starting a missing counter at floor.
	Add(ctx context.Context, consumer, period string, tokens, floor int64, end time.Time) (int64, error)
	// Total returns the period's tokens, 0 when there is no counter.
	Total(ctx context.Context, consumer, period string) (int64, error)
}

// KV keeps the persisted monthly totals; consul.Client implements it.
type KV interface {
	List(ctx context.Context, prefix string, index uint64, wait time.Duration) ([]consul.Pair, uint64, error)
	Put(ctx context.Context, key string, value []byte) error
}

// Outcome of a budget check.
type Outcome int

const (
	// NoBudget: the consumer has no budget, so nothing applies.
	NoBudget Outcome = iota
	// Allow: the consumer has tokens left, or the model has no price, so the budget does not
	// apply to the request.
	Allow
	// Exhausted: the consumer has used its month's tokens, and the model has a price.
	Exhausted
	// Unavailable: the consumer's budget is broken, or the budgets have not been read yet.
	// aisa fails closed.
	Unavailable
)

// Verdict is the answer of a budget check.
type Verdict struct {
	Outcome Outcome
	// Applies: the model has a price, so the budget bounds this request. A request to a model
	// without a price is allowed whatever the budget, but still carries Remaining.
	Applies bool
	// Limit, Used and Remaining are the month's tokens, when the consumer has a budget.
	Limit, Used, Remaining int64
	// Soft: Used is at or above the budget's soft ratio.
	Soft bool
	// OnExhausted and DowngradeTo are the budget's, for an Exhausted verdict.
	OnExhausted, DowngradeTo string
	// Err says why the budget is Unavailable.
	Err error
}

// Pricer says whether a model has a price; pricing.Prices does.
type Pricer interface {
	Paid(model string) bool
}

// MonthlyOptions configure Monthly.
type MonthlyOptions struct {
	// Timeout bounds each call to the counter, so a slow Redis does not stall the gateway;
	// 250 ms when zero.
	Timeout time.Duration
	// Location is the time zone of the calendar month; UTC when nil.
	Location *time.Location
	// Persist is how often the month's totals are copied to Consul KV; a minute when zero.
	Persist time.Duration
	// StatePrefix is where the totals are kept, such as "aisa/state/".
	StatePrefix string
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// Monthly checks and charges the budgets.
type Monthly struct {
	budgets *Budgets
	counter Counter
	kv      KV
	pricer  Pricer
	metrics *metrics.Metrics
	log     *slog.Logger
	opts    MonthlyOptions

	mu sync.Mutex
	// persisted is the total last written to, or read from, Consul KV, by consumer and period.
	persisted map[string]map[string]int64
	// dirty are the consumers and periods charged since the last persist.
	dirty map[periodKey]bool
}

type periodKey struct{ consumer, period string }

// NewMonthly returns the budget checks of the budgets, counted with counter and persisted
// with kv.
func NewMonthly(b *Budgets, c Counter, kv KV, p Pricer, m *metrics.Metrics, log *slog.Logger, opts MonthlyOptions) *Monthly {
	if opts.Timeout <= 0 {
		opts.Timeout = 250 * time.Millisecond
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.Persist <= 0 {
		opts.Persist = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.StatePrefix = strings.TrimSuffix(opts.StatePrefix, "/") + "/"
	return &Monthly{
		budgets: b, counter: c, kv: kv, pricer: p, metrics: m, log: log, opts: opts,
		persisted: make(map[string]map[string]int64), dirty: make(map[periodKey]bool),
	}
}

// Period is the calendar month of t in the configured time zone, as "2006-01", and when it ends.
func (mo *Monthly) Period(t time.Time) (string, time.Time) {
	t = t.In(mo.opts.Location)
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, mo.opts.Location)
	return start.Format("2006-01"), start.AddDate(0, 1, 0)
}

// Check decides whether a consumer may send a request to a model. The budget bounds the
// requests to models with a price; a request's own tokens are known only after the response,
// so the last request of a month can take a consumer past the budget. When the usage cannot
// be read from Redis, the total last persisted to Consul stands in for it: a budget is a
// bound on spend, and the persisted total is a lower bound on it.
func (mo *Monthly) Check(ctx context.Context, consumer, model string) Verdict {
	bud, err := mo.budgets.Get(consumer)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoBudget):
		return Verdict{Outcome: NoBudget}
	default:
		return Verdict{Outcome: Unavailable, Err: err}
	}
	period, _ := mo.Period(mo.opts.Now())
	used := mo.used(ctx, consumer, period)
	v := Verdict{
		Outcome: Allow, Applies: mo.pricer != nil && mo.pricer.Paid(model),
		Limit: bud.MonthlyTokens, Used: used, Remaining: max(bud.MonthlyTokens-used, 0),
		OnExhausted: bud.OnExhausted, DowngradeTo: bud.DowngradeTo,
	}
	v.Soft = float64(used) >= float64(bud.MonthlyTokens)*bud.SoftRatio
	if used >= bud.MonthlyTokens && v.Applies {
		v.Outcome = Exhausted
	}
	return v
}

// used is the greater of the counter's total and the persisted one: a counter Redis lost
// since the last persist reads 0, and must not lift the budget.
func (mo *Monthly) used(ctx context.Context, consumer, period string) int64 {
	ctx, cancel := context.WithTimeout(ctx, mo.opts.Timeout)
	defer cancel()
	total, err := mo.counter.Total(ctx, consumer, period)
	if err != nil {
		mo.metrics.BudgetErrors.WithLabelValues(metrics.BudgetOpCheck).Inc()
		mo.log.Warn("cannot read budget usage; using the persisted total", "consumer", consumer, "period", period, "error", err)
	}
	return max(total, mo.floor(consumer, period))
}

// Charge counts tokens that a consumer used on priced models at a time. A time in the future is
// taken as now. An error is logged and counted in aisa_budget_errors_total, and returned.
func (mo *Monthly) Charge(ctx context.Context, consumer string, tokens int64, at time.Time) error {
	if consumer == "" || consumer == metrics.ConsumerUnknown || tokens <= 0 {
		return nil
	}
	now := mo.opts.Now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	period, end := mo.Period(at)
	ctx, cancel := context.WithTimeout(ctx, mo.opts.Timeout)
	defer cancel()
	total, err := mo.counter.Add(ctx, consumer, period, tokens, mo.floor(consumer, period), end)
	if err != nil {
		mo.metrics.BudgetErrors.WithLabelValues(metrics.BudgetOpCharge).Inc()
		mo.log.Warn("cannot charge tokens to the budget", "consumer", consumer, "tokens", tokens, "error", err)
		return err
	}
	mo.mu.Lock()
	mo.dirty[periodKey{consumer, period}] = true
	mo.mu.Unlock()
	mo.metrics.BudgetSpent.WithLabelValues(consumer, period).Set(float64(total))
	return nil
}

// floor is the total last persisted for a consumer's period, 0 when none.
func (mo *Monthly) floor(consumer, period string) int64 {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	return mo.persisted[consumer][period]
}

// Ready returns nil once the budgets have been read, for aisa's readiness.
func (mo *Monthly) Ready(ctx context.Context) error { return mo.budgets.Ready(ctx) }

// Run reads the persisted totals from Consul KV, then copies the charged totals there every
// Persist until ctx is done, and keeps the budget gauges of the current month current.
func (mo *Monthly) Run(ctx context.Context) {
	mo.load(ctx)
	t := time.NewTicker(mo.opts.Persist)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mo.persist(ctx)
			mo.observe(ctx)
		}
	}
}

// state is the value of aisa/state/<consumer>/<period>.
type state struct {
	Tokens int64 `json:"tokens"`
}

// load reads the persisted totals once. A read that fails leaves them empty: the counters in
// Redis are the first source, and the next persist writes them again.
func (mo *Monthly) load(ctx context.Context) {
	pairs, _, err := mo.kv.List(ctx, mo.opts.StatePrefix, 0, 0)
	if err != nil {
		mo.metrics.BudgetErrors.WithLabelValues(metrics.BudgetOpPersist).Inc()
		mo.log.Error("cannot read the persisted budget totals; starting from the counters in Redis", "error", err)
		return
	}
	mo.mu.Lock()
	defer mo.mu.Unlock()
	n := 0
	for _, p := range pairs {
		consumer, period, ok := strings.Cut(strings.TrimPrefix(p.Key, mo.opts.StatePrefix), "/")
		if !ok || consumer == "" || period == "" {
			continue
		}
		var s state
		if err := json.Unmarshal(p.Value, &s); err != nil || s.Tokens < 0 {
			mo.log.Warn("ignoring an unreadable persisted budget total", "key", p.Key)
			continue
		}
		if mo.persisted[consumer] == nil {
			mo.persisted[consumer] = make(map[string]int64)
		}
		mo.persisted[consumer][period] = s.Tokens
		n++
	}
	mo.log.Info("persisted budget totals read", "totals", n)
}

// persist copies the totals of the consumers charged since the last call to Consul KV, so
// that a Redis that loses its data does not lift a budget by more than a minute's use.
func (mo *Monthly) persist(ctx context.Context) {
	mo.mu.Lock()
	keys := make([]periodKey, 0, len(mo.dirty))
	for k := range mo.dirty {
		keys = append(keys, k)
	}
	mo.dirty = make(map[periodKey]bool)
	mo.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].consumer < keys[j].consumer || keys[i].consumer == keys[j].consumer && keys[i].period < keys[j].period
	})
	for _, k := range keys {
		total, err := mo.counter.Total(ctx, k.consumer, k.period)
		if err != nil {
			mo.metrics.BudgetErrors.WithLabelValues(metrics.BudgetOpPersist).Inc()
			mo.log.Warn("cannot read a budget total to persist it", "consumer", k.consumer, "period", k.period, "error", err)
			mo.markDirty(k)
			continue
		}
		total = max(total, mo.floor(k.consumer, k.period))
		value, _ := json.Marshal(state{Tokens: total})
		if err := mo.kv.Put(ctx, mo.opts.StatePrefix+k.consumer+"/"+k.period, value); err != nil {
			mo.metrics.BudgetErrors.WithLabelValues(metrics.BudgetOpPersist).Inc()
			mo.log.Warn("cannot persist a budget total", "consumer", k.consumer, "period", k.period, "error", err)
			mo.markDirty(k)
			continue
		}
		mo.mu.Lock()
		if mo.persisted[k.consumer] == nil {
			mo.persisted[k.consumer] = make(map[string]int64)
		}
		mo.persisted[k.consumer][k.period] = total
		mo.mu.Unlock()
	}
}

func (mo *Monthly) markDirty(k periodKey) {
	mo.mu.Lock()
	mo.dirty[k] = true
	mo.mu.Unlock()
}

// observe sets aisa_budget_limit and aisa_budget_spent for the current month of every consumer
// with a budget, so the gauges exist before the first charge of the month and after a restart.
func (mo *Monthly) observe(ctx context.Context) {
	period, _ := mo.Period(mo.opts.Now())
	for _, consumer := range mo.budgets.Names() {
		bud, err := mo.budgets.Get(consumer)
		if err != nil {
			continue
		}
		mo.metrics.BudgetLimit.WithLabelValues(consumer, period).Set(float64(bud.MonthlyTokens))
		mo.metrics.BudgetSpent.WithLabelValues(consumer, period).Set(float64(mo.used(ctx, consumer, period)))
	}
}

// Observe is observe for callers outside Run, such as a load of the budgets.
func (mo *Monthly) Observe(ctx context.Context) { mo.observe(ctx) }

// String names an outcome in logs and tests.
func (o Outcome) String() string {
	switch o {
	case NoBudget:
		return "no_budget"
	case Allow:
		return "allow"
	case Exhausted:
		return "exhausted"
	case Unavailable:
		return "unavailable"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}
