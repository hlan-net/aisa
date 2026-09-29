package ledger

import (
	"bytes"
	"context"
	"errors"
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
	if allow := recGet.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("GET Allow = %q, want POST", allow)
	}

	// Malformed JSON, and an array whose structure is broken after a valid event
	for _, body := range []string{"bad-json", `[{"request_id":"a","status":200}, invalid]`} {
		reqBad := httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body))
		recBad := httptest.NewRecorder()
		h.ServeHTTP(recBad, reqBad)
		if recBad.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, recBad.Code)
		}
	}

	for reason, want := range map[string]float64{metrics.UsageRequestMalformed: 2} {
		if got := testutil.ToFloat64(m.UsageRequestsRejected.WithLabelValues(reason)); got != want {
			t.Errorf("aisa_usage_requests_rejected_total{reason=%q} = %v, want %v", reason, got, want)
		}
	}
	if n := testutil.CollectAndCount(m.UsageEvents); n != 0 {
		t.Errorf("aisa_usage_events_total has %d series, want 0: no event was read", n)
	}
}

// An event with a value that cannot be parsed is rejected alone; the rest of its batch is
// recorded (#27).
func TestHandlerValueErrorRejectsOnlyItsEvent(t *testing.T) {
	m := metrics.New("test")
	h := New(m, NewDedup(100, 5*time.Minute), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	body := `[
		{"request_id":"a","consumer":"chat-ui","model":"qwen3","backend":"ollama-1","status":200,"prompt_tokens":7},
		{"request_id":"b","status":200,"prompt_tokens":"abc"},
		{"request_id":"c","status":200,"latency_ms":"NaN"},
		{"request_id":"d","status":200,"stream":"yes"},
		{"request_id":"e","status":200,"prompt_tokens":1.9}
	]`
	req := httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"accepted":1,"rejected":4}` {
		t.Fatalf("answer = %d %s, want 200 {\"accepted\":1,\"rejected\":4}", rec.Code, rec.Body)
	}
	if got := testutil.ToFloat64(m.Tokens.WithLabelValues("chat-ui", "qwen3", "ollama-1", metrics.DirectionPrompt)); got != 7 {
		t.Errorf("prompt tokens of the valid event = %v, want 7", got)
	}
	if got := testutil.ToFloat64(m.UsageEvents.WithLabelValues(metrics.EventRejected)); got != 4 {
		t.Errorf("aisa_usage_events_total{rejected} = %v, want 4", got)
	}
	for _, reason := range metrics.UsageRequestReasons {
		if got := testutil.ToFloat64(m.UsageRequestsRejected.WithLabelValues(reason)); got != 0 {
			t.Errorf("aisa_usage_requests_rejected_total{reason=%q} = %v, want 0", reason, got)
		}
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
	if got := testutil.ToFloat64(m.UsageRequestsRejected.WithLabelValues(metrics.UsageRequestTooLarge)); got != 1 {
		t.Errorf("aisa_usage_requests_rejected_total{too_large} = %v, want 1", got)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestHandlerUnreadableBody(t *testing.T) {
	m := metrics.New("test")
	h := New(m, NewDedup(100, 5*time.Minute), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/v1/usage", io.MultiReader(strings.NewReader(`[{"request_id":"a",`), failingReader{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got := testutil.ToFloat64(m.UsageRequestsRejected.WithLabelValues(metrics.UsageRequestUnreadable)); got != 1 {
		t.Errorf("aisa_usage_requests_rejected_total{reason=\"unreadable\"} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(m.UsageEvents); n != 0 {
		t.Errorf("aisa_usage_events_total has %d series, want 0: no event was read", n)
	}
}

// A batch whose events are all wrong writes one warn line, and no field value: a misconfigured
// sink may put a credential in any field.
func TestHandlerLogsRejectedEventsOncePerRequest(t *testing.T) {
	var buf bytes.Buffer
	h := New(metrics.New("test"), NewDedup(100, 5*time.Minute), nil, slog.New(slog.NewTextHandler(&buf, nil)))

	var body strings.Builder
	body.WriteString("[")
	for i := range 500 {
		if i > 0 {
			body.WriteString(",")
		}
		body.WriteString(`{"request_id":"r","status":200,"stream":"Bearer sk-secret"}`)
	}
	body.WriteString("]")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body.String())))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warn lines for one request, want 1:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "sk-secret") {
		t.Errorf("the log quotes a field value:\n%s", buf.String())
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
		{"request_id": "g", "consumer": "chat-ui", "status": 200, "prompt_tokens": 1, "ts": "2026-09-29T15:00:00Z"},
		{"request_id": "h", "consumer": "chat-ui", "status": 200, "prompt_tokens": 1, "ts": "2027-01-01T00:00:00Z"},
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
	// one without tokens or the invalid one. A ts that cannot be read or is in the future is now.
	want := map[string]int64{"chat-ui@12:00:00": 18, "chat-ui@12:01:00": 100, "chat-ui@12:30:00": 9}
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

func TestHandlerCountsMissingUsage(t *testing.T) {
	tests := []struct {
		name  string
		event string
		want  float64
	}{
		{"zeros as strings, as APISIX logs a lost stream", `"status":"200","prompt_tokens":"0","completion_tokens":"0"`, 1},
		{"zeros as numbers, as a backend without usage", `"status":200,"prompt_tokens":0,"completion_tokens":0`, 1},
		{"no counts at all", `"status":200`, 1},
		{"empty and null counts", `"status":201,"prompt_tokens":"","completion_tokens":null`, 1},
		{"counted usage", `"status":200,"prompt_tokens":5,"completion_tokens":7`, 0},
		{"only completion tokens", `"status":200,"completion_tokens":7`, 0},
		{"a denied request", `"status":429`, 0},
		{"a backend error", `"status":502,"prompt_tokens":"0","completion_tokens":"0"`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New("test")
			h := New(m, NewDedup(100, 5*time.Minute), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			body := `{"request_id":"r1","consumer":"chat-ui","model":"qwen3","backend":"ollama-1",` + tt.event + `}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body)))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted":1`) {
				t.Fatalf("answer = %d %s, want 200 with the event accepted", rec.Code, rec.Body.String())
			}
			if got := testutil.ToFloat64(m.UsageMissing.WithLabelValues("chat-ui", "qwen3", "ollama-1")); got != tt.want {
				t.Errorf("UsageMissing = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandlerCountsMissingUsageOnce(t *testing.T) {
	m := metrics.New("test")
	h := New(m, NewDedup(100, 5*time.Minute), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `[{"request_id":"r1","model":"qwen3","status":200},{"request_id":"r1","model":"qwen3","status":200}]`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/usage", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := testutil.ToFloat64(m.UsageMissing.WithLabelValues(metrics.ConsumerUnknown, "qwen3", "")); got != 1 {
		t.Errorf("UsageMissing = %v, want 1: a retried event is counted once", got)
	}
}
