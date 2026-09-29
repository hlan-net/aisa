package ledger

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/metrics"
)

func TestHandlerServeHTTP(t *testing.T) {
	m := metrics.New("test")
	dedup := NewDedup(100, 5*time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(m, dedup, nil, log)

	body := []byte(`{
		"request_id": "r1",
		"consumer": "batch-jobs",
		"model": "qwen3",
		"backend": "ollama-1",
		"prompt_tokens": 15,
		"completion_tokens": 30,
		"status": 200,
		"latency_ms": 500,
		"ttft_ms": 100
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/usage", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted":1`) || !strings.Contains(rec.Body.String(), `"rejected":0`) {
		t.Errorf("body = %s, want accepted 1, rejected 0", rec.Body.String())
	}

	// Verify metrics
	if got := testutil.ToFloat64(m.UsageEvents.WithLabelValues(metrics.EventAccepted)); got != 1 {
		t.Errorf("UsageEvents{accepted} = %v, want 1", got)
	}
	reqs := testutil.ToFloat64(m.Requests.WithLabelValues("batch-jobs", "qwen3", "ollama-1", "200"))
	if reqs != 1 {
		t.Errorf("Requests = %v, want 1", reqs)
	}
	promptTokens := testutil.ToFloat64(m.Tokens.WithLabelValues("batch-jobs", "qwen3", "ollama-1", metrics.DirectionPrompt))
	if promptTokens != 15 {
		t.Errorf("Prompt tokens = %v, want 15", promptTokens)
	}
	complTokens := testutil.ToFloat64(m.Tokens.WithLabelValues("batch-jobs", "qwen3", "ollama-1", metrics.DirectionCompletion))
	if complTokens != 30 {
		t.Errorf("Completion tokens = %v, want 30", complTokens)
	}
}

func TestHandlerDedup(t *testing.T) {
	m := metrics.New("test")
	dedup := NewDedup(100, 5*time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(m, dedup, nil, log)

	body := []byte(`{
		"request_id": "r-dup",
		"consumer": "batch-jobs",
		"model": "qwen3",
		"backend": "ollama-1",
		"prompt_tokens": 10,
		"completion_tokens": 20,
		"status": 200
	}`)

	// Send first time
	req1 := httptest.NewRequest(http.MethodPost, "/v1/usage", bytes.NewReader(body))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec1.Code)
	}

	// Send second time (duplicate)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/usage", bytes.NewReader(body))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d", rec2.Code)
	}

	// Metrics must NOT double count
	if got := testutil.ToFloat64(m.UsageEvents.WithLabelValues(metrics.EventAccepted)); got != 1 {
		t.Errorf("UsageEvents{accepted} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.UsageEvents.WithLabelValues(metrics.EventDuplicate)); got != 1 {
		t.Errorf("UsageEvents{duplicate} = %v, want 1", got)
	}
	reqs := testutil.ToFloat64(m.Requests.WithLabelValues("batch-jobs", "qwen3", "ollama-1", "200"))
	if reqs != 1 {
		t.Errorf("Requests = %v, want 1 (deduplicated)", reqs)
	}
	tokens := testutil.ToFloat64(m.Tokens.WithLabelValues("batch-jobs", "qwen3", "ollama-1", metrics.DirectionPrompt))
	if tokens != 10 {
		t.Errorf("Prompt tokens = %v, want 10 (deduplicated)", tokens)
	}
}

func TestHandlerInvalidEventsSkipped(t *testing.T) {
	m := metrics.New("test")
	dedup := NewDedup(100, 5*time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(m, dedup, nil, log)

	body := []byte(`[
		{
			"request_id": "r-valid",
			"consumer": "batch-jobs",
			"model": "qwen3",
			"backend": "ollama-1",
			"status": 200,
			"prompt_tokens": 10,
			"completion_tokens": 20
		},
		{
			"request_id": "",
			"status": 200
		},
		{
			"request_id": "r-invalid-status",
			"status": 999
		},
		{
			"request_id": "r-neg-tokens",
			"status": 200,
			"prompt_tokens": -5
		}
	]`)

	req := httptest.NewRequest(http.MethodPost, "/v1/usage", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"accepted":1`) || !strings.Contains(rec.Body.String(), `"rejected":3`) {
		t.Errorf("body = %s, want accepted 1, rejected 3", rec.Body.String())
	}

	if got := testutil.ToFloat64(m.UsageEvents.WithLabelValues(metrics.EventAccepted)); got != 1 {
		t.Errorf("UsageEvents{accepted} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.UsageEvents.WithLabelValues(metrics.EventRejected)); got != 3 {
		t.Errorf("UsageEvents{rejected} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.Requests.WithLabelValues("batch-jobs", "qwen3", "ollama-1", "200")); got != 1 {
		t.Errorf("Requests = %v, want 1", got)
	}
}

func TestHandlerRejects(t *testing.T) {
	m := metrics.New("test")
	dedup := NewDedup(100, 5*time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(m, dedup, nil, log)

	// GET method not allowed
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	recGet := httptest.NewRecorder()
	h.ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", recGet.Code)
	}

	// Malformed JSON
	reqBad := httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader("bad-json"))
	recBad := httptest.NewRecorder()
	h.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("bad JSON status = %d, want 400", recBad.Code)
	}
}

func TestHandlerBodyTooLarge(t *testing.T) {
	m := metrics.New("test")
	dedup := NewDedup(100, 5*time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(m, dedup, nil, log)

	// Create body exceeding maxBody (16 MiB)
	largeBody := io.MultiReader(
		strings.NewReader(`[{"request_id":"large","comment":"`),
		strings.NewReader(strings.Repeat("a", (16<<20)+10)),
		strings.NewReader(`"}]`),
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/usage", largeBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// fakeCharger notes the charges.
type fakeCharger struct {
	charges map[string]int64 // "consumer@minute" → tokens
	calls   int
}

func (f *fakeCharger) Charge(_ context.Context, consumer string, tokens int64, at time.Time) error {
	f.calls++
	f.charges[consumer+"@"+at.UTC().Format("15:04:05")] += tokens
	return nil
}

func TestHandlerCharges(t *testing.T) {
	m := metrics.New("test")
	c := &fakeCharger{charges: map[string]int64{}}
	h := New(m, NewDedup(100, 5*time.Minute), c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.now = func() time.Time { return time.Date(2026, 9, 29, 12, 30, 20, 0, time.UTC) }

	body := `[
		{"request_id": "a", "consumer": "chat-ui", "status": 200, "prompt_tokens": 10, "completion_tokens": 5, "ts": "2026-09-29T12:00:10Z"},
		{"request_id": "b", "consumer": "chat-ui", "status": 200, "prompt_tokens": "1", "completion_tokens": 2, "ts": "2026-09-29T14:00:50.5+02:00"},
		{"request_id": "c", "consumer": "chat-ui", "status": 200, "prompt_tokens": 100, "ts": "2026-09-29T12:01:00Z"},
		{"request_id": "d", "consumer": "chat-ui", "status": 200, "prompt_tokens": 7, "ts": "not a time"},
		{"request_id": "a", "consumer": "chat-ui", "status": 200, "prompt_tokens": 10, "completion_tokens": 5, "ts": "2026-09-29T12:00:10Z"},
		{"request_id": "e", "consumer": "", "status": 200, "prompt_tokens": 1000},
		{"request_id": "f", "consumer": "batch-jobs", "status": 429},
		{"request_id": "", "consumer": "chat-ui", "status": 200, "prompt_tokens": 1000}
	]`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	// One charge per consumer and minute; not the duplicate, the event without a consumer, the
	// one without tokens or the invalid one. A ts that cannot be read is now.
	want := map[string]int64{"chat-ui@12:00:00": 18, "chat-ui@12:01:00": 100, "chat-ui@12:30:00": 7}
	// Equal minutes in other time zones are one charge.
	if len(c.charges) != len(want) || c.calls != len(want) {
		t.Errorf("charges = %v in %d calls, want %v", c.charges, c.calls, want)
	}
	for k, v := range want {
		if c.charges[k] != v {
			t.Errorf("charges[%s] = %d, want %d (all: %v)", k, c.charges[k], v, c.charges)
		}
	}
}

func TestHandlerChargesSaturate(t *testing.T) {
	c := &fakeCharger{charges: map[string]int64{}}
	h := New(metrics.New("test"), NewDedup(100, 5*time.Minute), c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `[
		{"request_id": "a", "consumer": "chat-ui", "status": 200, "prompt_tokens": 9223372036854775807, "completion_tokens": 1, "ts": "2026-09-29T12:00:10Z"},
		{"request_id": "b", "consumer": "chat-ui", "status": 200, "prompt_tokens": 9223372036854775807, "ts": "2026-09-29T12:00:20Z"}
	]`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body)))
	if got := c.charges["chat-ui@12:00:00"]; got != math.MaxInt64 {
		t.Errorf("charged %d, want the saturated %d", got, int64(math.MaxInt64))
	}
}
