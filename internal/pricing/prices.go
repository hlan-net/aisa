// Package pricing reads the prices of the models from Consul KV (aisa/pricing/<model>,
// docs/concepts/CONSUL.md) so the usage ledger can compute aisa_cost_total and budgets can
// tell which models are paid.
package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

// maxCurrency bounds the currency label of aisa_cost_total.
const maxCurrency = 256

// Price is the price of a model, aisa/pricing/<model> in Consul KV.
type Price struct {
	// InputPer1K and OutputPer1K are the prices of 1 000 prompt and completion tokens.
	InputPer1K  float64
	OutputPer1K float64
	// Currency is the currency label of aisa_cost_total, such as "EUR".
	Currency string
}

// Cost is the price of promptTokens and completionTokens in p.Currency.
func (p Price) Cost(promptTokens, completionTokens int64) float64 {
	return (float64(max(promptTokens, 0))*p.InputPer1K + float64(max(completionTokens, 0))*p.OutputPer1K) / 1000.0
}

// Paid reports whether the model costs money (either rate > 0).
func (p Price) Paid() bool {
	return p.InputPer1K > 0 || p.OutputPer1K > 0
}

// Errors of Prices.Get.
var (
	// ErrNotLoaded: the prices have not been read yet.
	ErrNotLoaded = errors.New("model prices not loaded yet")
	// ErrNoPrice: no price is configured for the model.
	ErrNoPrice = errors.New("model has no price")
)

// LoadStats describe a load of the model prices, for metrics.
type LoadStats struct {
	// Err is nil when the load replaced the prices.
	Err error
	// Valid and Invalid count the prices that could and could not be read.
	Valid, Invalid int
}

// Options configure Prices.
type Options struct {
	// RetryMin and RetryMax are the waits after a failed read; see consul.FollowOptions.
	RetryMin, RetryMax time.Duration
	// OnLoad is called after every load, when set.
	OnLoad func(LoadStats)
}

// Prices holds the model prices in Consul KV, kept up to date with the source.
type Prices struct {
	src  consul.Lister
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	loaded  bool
	valid   map[string]Price
	invalid map[string]error
}

// New returns an empty set of model prices. Call Run to fill it.
func New(src consul.Lister, log *slog.Logger, opts Options) *Prices {
	return &Prices{src: src, opts: opts, log: log}
}

// Get returns the price of a model, ErrNotLoaded before the first read, ErrNoPrice when no
// entry exists for the model, and another error when the model's entry cannot be read.
func (p *Prices) Get(model string) (Price, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return Price{}, ErrNotLoaded
	}
	if pr, ok := p.valid[model]; ok {
		return pr, nil
	}
	if err, ok := p.invalid[model]; ok {
		return Price{}, fmt.Errorf("price of model %q: %w", model, err)
	}
	return Price{}, fmt.Errorf("%w: %q", ErrNoPrice, model)
}

// Lookup returns the valid price of a model if one is loaded.
func (p *Prices) Lookup(model string) (Price, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pr, ok := p.valid[model]
	return pr, ok
}

// Ready returns nil once the prices have been read, for aisa's readiness.
func (p *Prices) Ready(context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return ErrNotLoaded
	}
	return nil
}

// Run reads the prices, then follows their changes until ctx is done. When the source cannot
// be read, the prices read before stay in use.
func (p *Prices) Run(ctx context.Context) {
	consul.Follow(ctx, p.src, consul.FollowOptions{RetryMin: p.opts.RetryMin, RetryMax: p.opts.RetryMax}, p.apply, func(err error) {
		p.log.Error("reading model prices failed; keeping the ones read before", "error", err)
		if p.opts.OnLoad != nil {
			p.opts.OnLoad(LoadStats{Err: err})
		}
	})
}

// apply replaces the prices with those read.
func (p *Prices) apply(pairs []consul.Pair) {
	valid := map[string]Price{}
	invalid := map[string]error{}
	for _, kv := range pairs {
		// The prefix itself, or a folder under it.
		if kv.Key == "" || strings.HasSuffix(kv.Key, "/") {
			continue
		}
		pr, err := parsePrice(kv.Value)
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
		p.log.Error("invalid model price", "model", name, "error", invalid[name])
	}

	p.mu.Lock()
	p.loaded, p.valid, p.invalid = true, valid, invalid
	p.mu.Unlock()
	p.log.Info("model prices loaded", "valid", len(valid), "invalid", len(invalid))
	if p.opts.OnLoad != nil {
		p.opts.OnLoad(LoadStats{Valid: len(valid), Invalid: len(invalid)})
	}
}

// parsePrice reads a model price's JSON value, such as
// {"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "EUR"}. Fields it does not know
// are ignored, so a later version can add some.
func parsePrice(value []byte) (Price, error) {
	var raw struct {
		InputPer1K  *float64 `json:"input_per_1k"`
		OutputPer1K *float64 `json:"output_per_1k"`
		Currency    *string  `json:"currency"`
	}
	if trimmed := bytes.TrimSpace(value); len(trimmed) == 0 || trimmed[0] != '{' {
		return Price{}, errors.New("want a JSON object")
	}
	if err := json.Unmarshal(value, &raw); err != nil {
		return Price{}, fmt.Errorf("want a JSON object: %w", err)
	}
	if raw.InputPer1K == nil {
		return Price{}, errors.New("input_per_1k is missing")
	}
	if math.IsNaN(*raw.InputPer1K) || math.IsInf(*raw.InputPer1K, 0) || *raw.InputPer1K < 0 {
		return Price{}, fmt.Errorf("input_per_1k is invalid: %v", *raw.InputPer1K)
	}
	if raw.OutputPer1K == nil {
		return Price{}, errors.New("output_per_1k is missing")
	}
	if math.IsNaN(*raw.OutputPer1K) || math.IsInf(*raw.OutputPer1K, 0) || *raw.OutputPer1K < 0 {
		return Price{}, fmt.Errorf("output_per_1k is invalid: %v", *raw.OutputPer1K)
	}
	if raw.Currency == nil {
		return Price{}, errors.New("currency is missing")
	}
	currency := strings.TrimSpace(*raw.Currency)
	if currency == "" {
		return Price{}, errors.New("currency is empty")
	}
	if len(currency) > maxCurrency {
		return Price{}, fmt.Errorf("currency exceeds %d bytes", maxCurrency)
	}
	return Price{
		InputPer1K:  *raw.InputPer1K,
		OutputPer1K: *raw.OutputPer1K,
		Currency:    currency,
	}, nil
}
