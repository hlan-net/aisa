package budgets

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Always says every model is paid: for a provider budget, whatever goes through the key costs.
type Always struct{}

// Paid implements Pricer.
func (Always) Paid(string) bool { return true }

// Ceiling is the budgets of the provider accounts, aisa/budgets/providers/<key>: a token total
// per key above the consumers (#33). A model is bounded by the budgets of every key that
// serves it, and the tokens of a request are charged to the key of the backend that answered
// it, or to the keys of its model when the backend cannot be told.
type Ceiling struct {
	catalog *Catalog
	monthly *Monthly
	log     *slog.Logger

	mu    sync.Mutex
	noted map[string]bool
}

// NewCeiling returns the ceiling of the provider budgets in monthly, applied through catalog.
func NewCeiling(c *Catalog, m *Monthly, log *slog.Logger) *Ceiling {
	return &Ceiling{catalog: c, monthly: m, log: log, noted: map[string]bool{}}
}

// Check decides whether a request to a model may go on under the provider budgets. The first
// key of the model whose budget is exhausted, or cannot be known, answers; a model that no key
// serves has no provider budget.
func (ce *Ceiling) Check(ctx context.Context, model string) Verdict {
	out := Verdict{Outcome: NoBudget}
	for _, key := range ce.catalog.KeysOf(model) {
		v := ce.monthly.Check(ctx, key, model)
		v.Provider = key
		switch v.Outcome {
		case Exhausted, Unavailable:
			return v
		case Allow:
			if out.Outcome == NoBudget {
				out = v
			}
		}
	}
	return out
}

// Charge counts the tokens of a request to a model, answered by a backend, towards the budget
// of the backend's key. When the backend is not in the catalog, the keys of the model are
// charged: all of them when there are several, since the request went through one of them
// and a ceiling must not be lifted by a name the gateway did not report.
func (ce *Ceiling) Charge(ctx context.Context, model, backend string, tokens int64, at time.Time) error {
	if tokens <= 0 {
		return nil
	}
	keys := ce.catalog.KeysOf(model)
	if key, ok := ce.catalog.KeyOf(backend); ok {
		keys = []string{key}
	} else if len(keys) > 1 {
		ce.note(model, backend)
	}
	var first error
	for _, key := range keys {
		if err := ce.monthly.Charge(ctx, key, tokens, at); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// note logs once per model that its backend could not be told, so the charge went to every key.
func (ce *Ceiling) note(model, backend string) {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	if ce.noted[model] {
		return
	}
	ce.noted[model] = true
	ce.log.Warn("the backend of a usage event is not in the catalog; its tokens are charged to every provider key of the model",
		"model", model, "backend", backend)
}

// Ready returns nil once the provider budgets and the catalog have been read.
func (ce *Ceiling) Ready(ctx context.Context) error {
	if err := ce.monthly.Ready(ctx); err != nil {
		return err
	}
	if !ce.catalog.Loaded() {
		return errCatalogNotLoaded
	}
	return nil
}
