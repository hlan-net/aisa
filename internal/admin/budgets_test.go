package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hlan-net/aisa/internal/budgets"
)

// fakeUsage reports fixed tokens used per consumer in October 2026.
type fakeUsage map[string]int64

func (f fakeUsage) Usage(_ context.Context, consumer string) (string, int64) {
	return "2026-10", f[consumer]
}

func serve(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newJSONRequest(method, target, body))
	return w
}

func decodeBudget(t *testing.T, w *httptest.ResponseRecorder) BudgetItem {
	t.Helper()
	var item BudgetItem
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatalf("unmarshal %s: %v", w.Body.String(), err)
	}
	return item
}

func TestBudgetsCRUD(t *testing.T) {
	h, mc, _ := setupTestHandler()
	h.opts.Budgets = fakeUsage{"chat-ui": 900}
	_ = mc.Put(context.Background(), "aisa/consumers/chat-ui", []byte(`{}`))

	// Set: the defaults are filled in and stored with every field named.
	w := serve(h, http.MethodPut, "/admin/api/budgets/chat-ui", `{"monthly_tokens": 1000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put status = %d: %s", w.Code, w.Body.String())
	}
	stored := mc.pairs["aisa/budgets/chat-ui"]
	if bud, err := budgets.Parse(stored); err != nil || bud != (budgets.Budget{MonthlyTokens: 1000, SoftRatio: budgets.DefaultSoftRatio, OnExhausted: budgets.Reject}) {
		t.Fatalf("stored %s: %+v, %v", stored, bud, err)
	}
	item := decodeBudget(t, w)
	if item.Status != "ready" || item.SoftLimit != 800 || item.Usage == nil ||
		*item.Usage != (BudgetUsageItem{Period: "2026-10", Used: 900, Remaining: 100, Soft: true}) {
		t.Errorf("put response = %+v, usage %+v", item, item.Usage)
	}

	// Change, and read back.
	w = serve(h, http.MethodPut, "/admin/api/budgets/chat-ui",
		`{"monthly_tokens": 900, "soft_ratio": 0.5, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change status = %d: %s", w.Code, w.Body.String())
	}
	w = serve(h, http.MethodGet, "/admin/api/budgets/chat-ui", "")
	item = decodeBudget(t, w)
	if w.Code != http.StatusOK || item.MonthlyTokens != 900 || item.SoftRatio != 0.5 || item.OnExhausted != "downgrade" ||
		item.DowngradeTo != "qwen3" || item.SoftLimit != 450 || !item.Usage.Exhausted || item.Usage.Remaining != 0 {
		t.Errorf("get = %d %+v, usage %+v", w.Code, item, item.Usage)
	}

	checkBudgetList(t, h, mc)

	// Remove.
	if w = serve(h, http.MethodDelete, "/admin/api/budgets/chat-ui", ""); w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	if _, ok := mc.pairs["aisa/budgets/chat-ui"]; ok {
		t.Error("budget still in Consul")
	}
	if w = serve(h, http.MethodGet, "/admin/api/budgets/chat-ui", ""); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d", w.Code)
	}
	if w = serve(h, http.MethodDelete, "/admin/api/budgets/chat-ui", ""); w.Code != http.StatusNotFound {
		t.Errorf("second delete = %d", w.Code)
	}
}

func TestBudgetRejectedAsTheLoaderRejectsIt(t *testing.T) {
	h, mc, _ := setupTestHandler()
	_ = mc.Put(context.Background(), "aisa/consumers/chat-ui", []byte(`{}`))
	for _, body := range []string{
		`{"soft_ratio": 0.5}`,
		`{"monthly_tokens": -1}`,
		`{"monthly_tokens": 1, "soft_ratio": 1.5}`,
		`{"monthly_tokens": 1, "on_exhausted": "ignore"}`,
		`{"monthly_tokens": 1, "on_exhausted": "downgrade"}`,
		`5000000`,
	} {
		_, want := budgets.Parse([]byte(body))
		w := serve(h, http.MethodPut, "/admin/api/budgets/chat-ui", body)
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusBadRequest || resp["error"] != "invalid budget: "+want.Error() {
			t.Errorf("%s: %d %q, want 400 %q", body, w.Code, resp["error"], want)
		}
	}
	if _, ok := mc.pairs["aisa/budgets/chat-ui"]; ok {
		t.Error("an invalid budget was saved")
	}
}

func TestBudgetErrors(t *testing.T) {
	h, mc, _ := setupTestHandler()

	// Only a declared consumer gets a budget.
	if w := serve(h, http.MethodPut, "/admin/api/budgets/nobody", `{"monthly_tokens": 1}`); w.Code != http.StatusNotFound {
		t.Errorf("undeclared consumer: %d", w.Code)
	}
	for _, name := range []string{"unknown", "-x", "a%20b"} {
		if w := serve(h, http.MethodPut, "/admin/api/budgets/"+name, `{"monthly_tokens": 1}`); w.Code != http.StatusBadRequest {
			t.Errorf("name %q: %d", name, w.Code)
		}
	}
	// Mutations need JSON, as the other endpoints do.
	req := httptest.NewRequest(http.MethodPut, "/admin/api/budgets/chat-ui", strings.NewReader(`{"monthly_tokens": 1}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no content type: %d", w.Code)
	}
	// Without usage, the budget is shown without it.
	_ = mc.Put(context.Background(), "aisa/budgets/chat-ui", []byte(`{"monthly_tokens": 10}`))
	if item := decodeBudget(t, serve(h, http.MethodGet, "/admin/api/budgets/chat-ui", "")); item.Usage != nil || item.MonthlyTokens != 10 {
		t.Errorf("without usage: %+v", item)
	}

	mc.listErr = errors.New("consul down")
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/admin/api/budgets", ""},
		{http.MethodGet, "/admin/api/budgets/chat-ui", ""},
		{http.MethodPut, "/admin/api/budgets/chat-ui", `{"monthly_tokens": 1}`},
		{http.MethodDelete, "/admin/api/budgets/chat-ui", ""},
	} {
		if w := serve(h, tc.method, tc.target, tc.body); w.Code != http.StatusInternalServerError {
			t.Errorf("%s %s with Consul down: %d", tc.method, tc.target, w.Code)
		}
	}
	mc.listErr = nil
	_ = mc.Put(context.Background(), "aisa/consumers/chat-ui", []byte(`{}`))
	mc.putErr = errors.New("denied")
	if w := serve(h, http.MethodPut, "/admin/api/budgets/chat-ui", `{"monthly_tokens": 1}`); w.Code != http.StatusInternalServerError {
		t.Errorf("put denied: %d", w.Code)
	}
	mc.delErr = errors.New("denied")
	if w := serve(h, http.MethodDelete, "/admin/api/budgets/chat-ui", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("delete denied: %d", w.Code)
	}
}

// checkBudgetList checks that the list holds the consumers' budgets, not the providers' below them.
func checkBudgetList(t *testing.T, h http.Handler, mc *mockConsul) {
	t.Helper()
	_ = mc.Put(context.Background(), "aisa/budgets/providers/gemini", []byte(`{"monthly_tokens": 1}`))
	_ = mc.Put(context.Background(), "aisa/budgets/broken", []byte(`{"soft_ratio": 0.5}`))
	w := serve(h, http.MethodGet, "/admin/api/budgets", "")
	var list []BudgetItem
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != http.StatusOK || len(list) != 2 || list[0].Consumer != "broken" || list[1].Consumer != "chat-ui" {
		t.Fatalf("list = %d %+v", w.Code, list)
	}
	if list[0].Status != "invalid" || list[0].Error != "monthly_tokens is missing" || list[0].Usage != nil {
		t.Errorf("broken budget = %+v", list[0])
	}
}
