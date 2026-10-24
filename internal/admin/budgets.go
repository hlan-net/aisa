package admin

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/hlan-net/aisa/internal/budgets"
	"github.com/hlan-net/aisa/internal/consul"
)

const (
	pathBudgets = "/budgets/"
	// maxBudgetBody bounds the body of PUT /admin/api/budgets/{name}; a budget is a few fields.
	maxBudgetBody = 64 << 10
)

// BudgetUsage reports a consumer's tokens in the current month; budgets.Monthly implements it.
type BudgetUsage interface {
	Usage(ctx context.Context, consumer string) (period string, used int64)
}

// BudgetItem is a consumer's monthly budget, aisa/budgets/<consumer> in Consul KV, with the
// month's use when aisa counts it.
type BudgetItem struct {
	Consumer      string  `json:"consumer"`
	Status        string  `json:"status"` // ready, invalid
	Error         string  `json:"error,omitempty"`
	MonthlyTokens int64   `json:"monthly_tokens"`
	SoftRatio     float64 `json:"soft_ratio"`
	OnExhausted   string  `json:"on_exhausted,omitempty"`
	DowngradeTo   string  `json:"downgrade_to,omitempty"`
	// SoftLimit is the tokens at which the budget is soft-exhausted.
	SoftLimit int64 `json:"soft_limit"`
	// Usage is absent when aisa counts no budgets (no Redis) or the budget cannot be read.
	Usage *BudgetUsageItem `json:"usage,omitempty"`
}

// BudgetUsageItem is a consumer's use of its budget in the current month.
type BudgetUsageItem struct {
	Period    string `json:"period"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	// Soft: Used is at or above the soft limit. Exhausted: nothing remains, so requests to
	// priced models are rejected or downgraded.
	Soft      bool `json:"soft"`
	Exhausted bool `json:"exhausted"`
}

// budgetItem reads a budget's value as the budgets loader does, and adds the month's use.
func (h *Handler) budgetItem(ctx context.Context, consumer string, value []byte) BudgetItem {
	bud, err := budgets.Parse(value)
	if err != nil {
		return BudgetItem{Consumer: consumer, Status: "invalid", Error: err.Error()}
	}
	item := BudgetItem{
		Consumer:      consumer,
		Status:        "ready",
		MonthlyTokens: bud.MonthlyTokens,
		SoftRatio:     bud.SoftRatio,
		OnExhausted:   bud.OnExhausted,
		DowngradeTo:   bud.DowngradeTo,
		SoftLimit:     int64(math.Ceil(float64(bud.MonthlyTokens) * bud.SoftRatio)),
	}
	if h.opts.Budgets != nil {
		period, used := h.opts.Budgets.Usage(ctx, consumer)
		item.Usage = &BudgetUsageItem{
			Period:    period,
			Used:      used,
			Remaining: max(bud.MonthlyTokens-used, 0),
			// As budgets.Monthly.Check decides them.
			Soft:      float64(used) >= float64(bud.MonthlyTokens)*bud.SoftRatio,
			Exhausted: used >= bud.MonthlyTokens,
		}
	}
	return item
}

// fetchBudgets lists the consumers' budgets. A key below a folder, such as a provider's budget
// under aisa/budgets/providers/, is not a consumer's.
func (h *Handler) fetchBudgets(ctx context.Context) ([]BudgetItem, error) {
	prefix := h.opts.ConsulPrefix + pathBudgets
	pairs, _, err := h.consul.List(ctx, prefix, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	items := make([]BudgetItem, 0, len(pairs))
	for _, p := range pairs {
		name := strings.TrimPrefix(p.Key, prefix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		items = append(items, h.budgetItem(ctx, name, p.Value))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Consumer < items[j].Consumer })
	return items, nil
}

func (h *Handler) listBudgets(w http.ResponseWriter, r *http.Request) {
	items, err := h.fetchBudgets(r.Context())
	if err != nil {
		h.log.Error("list budgets failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list budgets: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// budgetName returns the consumer named in the path, or writes the error and returns false.
func budgetName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, errConsumerNameRequired)
		return "", false
	}
	if !isValidName(name) {
		writeError(w, http.StatusBadRequest, errInvalidConsumerName)
		return "", false
	}
	return name, true
}

// lookup returns the pair at key, nil when there is none.
func (h *Handler) lookup(ctx context.Context, key string) (*consul.Pair, error) {
	pairs, _, err := h.consul.List(ctx, key, 0, 0)
	if err != nil {
		return nil, err
	}
	return findExactKVPair(pairs, key), nil
}

func (h *Handler) getBudget(w http.ResponseWriter, r *http.Request) {
	name, ok := budgetName(w, r)
	if !ok {
		return
	}
	pair, err := h.lookup(r.Context(), h.opts.ConsulPrefix+pathBudgets+name)
	if err != nil {
		h.log.Error("consul list budget failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, errConsulListPrefix+err.Error())
		return
	}
	if pair == nil {
		writeError(w, http.StatusNotFound, "budget not found")
		return
	}
	writeJSON(w, http.StatusOK, h.budgetItem(r.Context(), name, pair.Value))
}

// putBudget sets a declared consumer's budget. The body is the budget's value in Consul KV
// (docs/concepts/CONSUL.md), validated as aisa validates it when it reads the budgets, so a
// budget that aisa would refuse cannot be saved; it is stored with every field named.
func (h *Handler) putBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, ok := budgetName(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBudgetBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	bud, err := budgets.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid budget: "+err.Error())
		return
	}
	declared, err := h.lookup(ctx, h.opts.ConsulPrefix+pathConsumers+name)
	if err != nil {
		h.log.Error("consul list consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, errConsulListPrefix+err.Error())
		return
	}
	if declared == nil {
		writeError(w, http.StatusNotFound, "consumer not found")
		return
	}
	value := bud.Value()
	if err := h.consul.Put(ctx, h.opts.ConsulPrefix+pathBudgets+name, value); err != nil {
		h.log.Error("consul put budget failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul put: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.budgetItem(ctx, name, value))
}

func (h *Handler) deleteBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, ok := budgetName(w, r)
	if !ok {
		return
	}
	key := h.opts.ConsulPrefix + pathBudgets + name
	pair, err := h.lookup(ctx, key)
	if err != nil {
		h.log.Error("consul list budget failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, errConsulListPrefix+err.Error())
		return
	}
	if pair == nil {
		writeError(w, http.StatusNotFound, "budget not found")
		return
	}
	if err := h.consul.Delete(ctx, key, false); err != nil {
		h.log.Error("consul delete budget failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul delete: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}
