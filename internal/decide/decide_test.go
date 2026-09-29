package decide

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
)

type fakeLookup struct {
	keys        map[string]consumers.Consumer
	unavailable bool
}

func (f fakeLookup) Lookup(_ context.Context, key string) (consumers.Consumer, consumers.Result) {
	if f.unavailable {
		return consumers.Consumer{}, consumers.Unavailable
	}
	if c, ok := f.keys[key]; ok {
		return c, consumers.Found
	}
	return consumers.Consumer{}, consumers.Unknown
}

func newHandler(unavailable bool) (*Handler, *metrics.Metrics) {
	m := metrics.New("test")
	l := fakeLookup{
		keys:        map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}},
		unavailable: unavailable,
	}
	return New(l, m, slog.New(slog.NewTextHandler(io.Discard, nil))), m
}

type decideCase struct {
	name         string
	method       string
	header       map[string]string
	body         string
	wantStatus   int
	wantConsumer string
	wantModel    string
	wantCode     string
	wantResult   [2]string // consumer, result label of aisa_decisions_total
}

func TestDecide(t *testing.T) {
	tests := []decideCase{
		{"model from the body", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"model":"qwen3","messages":[{"role":"user","content":"hi"}]}`, 200, "chat-ui", "qwen3", "",
			[2]string{"chat-ui", metrics.ResultAllow}},
		{"header wins over the body", http.MethodPost,
			map[string]string{"Authorization": "Bearer key-chat", "X-Aisa-Requested-Model": "llama3.2"},
			`{"model":"qwen3"}`, 200, "chat-ui", "llama3.2", "", [2]string{"chat-ui", metrics.ResultAllow}},
		{"GET with the header", http.MethodGet,
			map[string]string{"Authorization": "bearer  key-chat ", "X-Aisa-Requested-Model": "qwen3"},
			``, 200, "chat-ui", "qwen3", "", [2]string{"chat-ui", metrics.ResultAllow}},
		{"unknown key", http.MethodPost, map[string]string{"Authorization": "Bearer nope"},
			`{"model":"qwen3"}`, 401, "", "", "invalid_api_key", [2]string{metrics.ConsumerUnknown, metrics.ResultDenyAuth}},
		{"no credential", http.MethodPost, nil,
			`{"model":"qwen3"}`, 401, "", "", "invalid_api_key", [2]string{metrics.ConsumerUnknown, metrics.ResultDenyAuth}},
		{"not a bearer credential", http.MethodPost, map[string]string{"Authorization": "Basic a2V5LWNoYXQ="},
			`{"model":"qwen3"}`, 401, "", "", "invalid_api_key", [2]string{metrics.ConsumerUnknown, metrics.ResultDenyAuth}},
		{"no model", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"messages":[]}`, 400, "", "", "missing_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"body is not JSON", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`not json`, 400, "", "", "missing_model", [2]string{"chat-ui", metrics.ResultInvalid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { runDecide(t, tt) })
	}
}

func runDecide(t *testing.T, tt decideCase) {
	h, m := newHandler(false)
	req := httptest.NewRequest(tt.method, "/v1/decide", strings.NewReader(tt.body))
	for k, v := range tt.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != tt.wantStatus {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
	}
	if got := rec.Header().Get(HeaderConsumer); got != tt.wantConsumer {
		t.Errorf("X-Aisa-Consumer = %q, want %q", got, tt.wantConsumer)
	}
	if got := rec.Header().Get(HeaderModel); got != tt.wantModel {
		t.Errorf("X-Aisa-Model = %q, want %q", got, tt.wantModel)
	}
	if tt.wantCode != "" {
		checkErrorBody(t, rec.Body.Bytes(), tt.wantCode)
	}
	if n := testutil.ToFloat64(m.Decisions.WithLabelValues(tt.wantResult[0], tt.wantResult[1])); n != 1 {
		t.Errorf("aisa_decisions_total%v = %v, want 1", tt.wantResult, n)
	}
}

func checkErrorBody(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var e struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Code != wantCode || e.Error.Message == "" {
		t.Errorf("error body = %s, want code %s", body, wantCode)
	}
}

func TestUnavailableFailsClosed(t *testing.T) {
	h, m := newHandler(true)
	req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"qwen3"}`))
	req.Header.Set("Authorization", "Bearer key-chat")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get(HeaderConsumer) != "" {
		t.Error("a 503 must not carry X-Aisa-Consumer")
	}
	if n := testutil.CollectAndCount(m.Decisions); n != 0 {
		t.Errorf("aisa_decisions_total has %d series, want 0: no decision was made", n)
	}
}
