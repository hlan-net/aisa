// Package budgets enforces monthly token budgets (docs/features/quotas-and-budgets.md,
// Budgets): a consumer's budget in Consul KV gives the tokens it may use in a calendar month
// on the models that have a price, a counter in Redis holds what it has used, and Consul KV
// keeps a copy of the month's total so that a Redis that loses its data does not lift the
// budget.
package budgets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

// Values of on_exhausted.
const (
	// Reject: an exhausted consumer's requests to priced models are denied with 429.
	Reject = "reject"
	// Downgrade: they are allowed, on the model of downgrade_to instead.
	Downgrade = "downgrade"
)

// maxModel bounds downgrade_to, as the decision bounds a model name.
const maxModel = 256

// DefaultSoftRatio is the share of the budget at which it is soft-exhausted when the budget
// names none.
const DefaultSoftRatio = 0.8

// Budget is a consumer's monthly budget, aisa/budgets/<consumer> in Consul KV.
type Budget struct {
	// MonthlyTokens is how many tokens the consumer may use in a calendar month on priced
	// models. 0 allows none.
	MonthlyTokens int64
	// SoftRatio is the share of the budget at which the budget is soft-exhausted: the request
	// is allowed, and the holder is alerted.
	SoftRatio float64
	// OnExhausted is Reject or Downgrade.
	OnExhausted string
	// DowngradeTo is the model an exhausted consumer's requests are sent to, with Downgrade.
	DowngradeTo string
}

// LoadStats describe a load of the budgets, for metrics.
type LoadStats struct {
	// Err is nil when the load replaced the budgets.
	Err error
	// Valid and Invalid count the budgets that could and could not be read.
	Valid, Invalid int
}

// Options configure Budgets.
type Options struct {
	// RetryMin and RetryMax are the waits after a failed read; see consul.FollowOptions.
	RetryMin, RetryMax time.Duration
	// OnLoad is called after every load, when set.
	OnLoad func(LoadStats)
}

// Budgets holds the consumers' budgets, kept up to date with the source.
type Budgets struct {
	src  consul.Lister
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	loaded  bool
	valid   map[string]Budget
	invalid map[string]error
}

// NewBudgets returns an empty set of budgets. Call Run to fill it.
func NewBudgets(src consul.Lister, log *slog.Logger, opts Options) *Budgets {
	return &Budgets{src: src, opts: opts, log: log}
}

// Errors of Budgets.Get.
var (
	// ErrNotLoaded: the budgets have not been read yet.
	ErrNotLoaded = errors.New("budgets not loaded yet")
	// ErrNoBudget: the consumer has no budget.
	ErrNoBudget = errors.New("consumer has no budget")
)

// Get returns a consumer's budget, ErrNoBudget when it has none, and the error of a budget
// that exists but cannot be read, so a broken budget does not pass for none.
func (b *Budgets) Get(consumer string) (Budget, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.loaded {
		return Budget{}, ErrNotLoaded
	}
	if bud, ok := b.valid[consumer]; ok {
		return bud, nil
	}
	if err, ok := b.invalid[consumer]; ok {
		return Budget{}, fmt.Errorf("budget of consumer %q: %w", consumer, err)
	}
	return Budget{}, fmt.Errorf("%w: %q", ErrNoBudget, consumer)
}

// Names returns the consumers that have a valid budget.
func (b *Budgets) Names() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.valid))
	for name := range b.valid {
		names = append(names, name)
	}
	return names
}

// Ready returns nil once the budgets have been read, for aisa's readiness.
func (b *Budgets) Ready(context.Context) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.loaded {
		return ErrNotLoaded
	}
	return nil
}

// Run reads the budgets, then follows their changes until ctx is done. When the source cannot
// be read, the budgets read before stay in use.
func (b *Budgets) Run(ctx context.Context) {
	consul.Follow(ctx, b.src, consul.FollowOptions{RetryMin: b.opts.RetryMin, RetryMax: b.opts.RetryMax}, b.apply, func(err error) {
		b.log.Error("reading budgets failed; keeping the ones read before", "error", err)
		if b.opts.OnLoad != nil {
			b.opts.OnLoad(LoadStats{Err: err})
		}
	})
}

// apply replaces the budgets with those read. An entry below a folder, such as the providers'
// budgets under the consumers' prefix, is not one of these.
func (b *Budgets) apply(pairs []consul.Pair) {
	pairs = slices.DeleteFunc(slices.Clone(pairs), func(p consul.Pair) bool {
		return strings.Contains(strings.TrimSuffix(p.Key, "/"), "/")
	})
	valid, invalid, names := consul.ParseEntries(pairs, parseBudget)
	for _, name := range names {
		// The consumer is denied with 503 on priced models until the budget is fixed.
		b.log.Error("invalid budget", "consumer", name, "error", invalid[name])
	}
	b.mu.Lock()
	b.loaded, b.valid, b.invalid = true, valid, invalid
	b.mu.Unlock()
	b.log.Info("budgets loaded", "valid", len(valid), "invalid", len(invalid))
	if b.opts.OnLoad != nil {
		b.opts.OnLoad(LoadStats{Valid: len(valid), Invalid: len(invalid)})
	}
}

// Parse reads a budget's JSON value as the loader does, so that a budget written elsewhere,
// such as by the admin interface, is refused with the same message as a load would log.
func Parse(value []byte) (Budget, error) { return parseBudget(value) }

// Value is the budget's JSON value in Consul KV, with every field named, which Parse reads
// back as the same budget.
func (b Budget) Value() []byte {
	v := struct {
		MonthlyTokens int64   `json:"monthly_tokens"`
		SoftRatio     float64 `json:"soft_ratio"`
		OnExhausted   string  `json:"on_exhausted"`
		DowngradeTo   string  `json:"downgrade_to,omitempty"`
	}{b.MonthlyTokens, b.SoftRatio, b.OnExhausted, b.DowngradeTo}
	out, _ := json.Marshal(v)
	return out
}

// parseBudget reads a budget's JSON value, such as {"monthly_tokens": 5000000, "soft_ratio":
// 0.8, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}. monthly_tokens must be there;
// the others have defaults, and downgrade_to is required with on_exhausted "downgrade". Fields
// it does not know are ignored, so a later version can add some.
func parseBudget(value []byte) (Budget, error) {
	var raw struct {
		MonthlyTokens *int64   `json:"monthly_tokens"`
		SoftRatio     *float64 `json:"soft_ratio"`
		OnExhausted   *string  `json:"on_exhausted"`
		DowngradeTo   *string  `json:"downgrade_to"`
	}
	if err := json.Unmarshal(value, &raw); err != nil {
		return Budget{}, fmt.Errorf("want a JSON object with monthly_tokens: %w", err)
	}
	if raw.MonthlyTokens == nil {
		return Budget{}, errors.New("monthly_tokens is missing")
	}
	if *raw.MonthlyTokens < 0 {
		return Budget{}, fmt.Errorf("monthly_tokens is negative: %d", *raw.MonthlyTokens)
	}
	bud := Budget{MonthlyTokens: *raw.MonthlyTokens, SoftRatio: DefaultSoftRatio, OnExhausted: Reject}
	if raw.SoftRatio != nil {
		if *raw.SoftRatio <= 0 || *raw.SoftRatio > 1 {
			return Budget{}, fmt.Errorf("soft_ratio must be above 0 and at most 1, got %v", *raw.SoftRatio)
		}
		bud.SoftRatio = *raw.SoftRatio
	}
	if raw.OnExhausted != nil {
		switch strings.TrimSpace(*raw.OnExhausted) {
		case Reject:
			bud.OnExhausted = Reject
		case Downgrade:
			bud.OnExhausted = Downgrade
		default:
			return Budget{}, fmt.Errorf("on_exhausted must be %q or %q, got %q", Reject, Downgrade, *raw.OnExhausted)
		}
	}
	if raw.DowngradeTo != nil {
		bud.DowngradeTo = strings.TrimSpace(*raw.DowngradeTo)
		if len(bud.DowngradeTo) > maxModel {
			return Budget{}, fmt.Errorf("downgrade_to is longer than %d bytes", maxModel)
		}
	}
	if bud.OnExhausted == Downgrade && bud.DowngradeTo == "" {
		return Budget{}, errors.New("on_exhausted is downgrade, but downgrade_to names no model")
	}
	return bud, nil
}
