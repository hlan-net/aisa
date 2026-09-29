package decide

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/quotas"
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
	return New(l, nil, m, slog.New(slog.NewTextHandler(io.Discard, nil))), m
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
		{"model in the header too long", http.MethodPost,
			map[string]string{"Authorization": "Bearer key-chat", "X-Aisa-Requested-Model": strings.Repeat("m", maxModel+1)},
			``, 400, "", "", "invalid_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"model in the body too long", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"model":"` + strings.Repeat("m", maxModel+1) + `"}`, 400, "", "", "invalid_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"model at the limit", http.MethodPost,
			map[string]string{"Authorization": "Bearer key-chat", "X-Aisa-Requested-Model": strings.Repeat("m", maxModel)},
			``, 200, "chat-ui", strings.Repeat("m", maxModel), "", [2]string{"chat-ui", metrics.ResultAllow}},
		{"body is not JSON", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`not json`, 400, "", "", "missing_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"body is not an object", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`["model","qwen3"]`, 400, "", "", "missing_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"body cut short", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"model":"qwen3","messages":[{"role":"user"`, 400, "", "", "missing_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"model is not a string", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"model":{"name":"qwen3"},"messages":[]}`, 400, "", "", "missing_model", [2]string{"chat-ui", metrics.ResultInvalid}},
		{"model only inside a message", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"messages":[{"role":"user","model":"qwen3","content":{"model":"qwen3"}}]}`, 400, "", "", "missing_model",
			[2]string{"chat-ui", metrics.ResultInvalid}},
		{"model after the messages", http.MethodPost, map[string]string{"Authorization": "Bearer key-chat"},
			`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true,"model":" qwen3 "}`,
			200, "chat-ui", "qwen3", "", [2]string{"chat-ui", metrics.ResultAllow}},
		{"model given twice: the last one, as the backend reads it", http.MethodPost,
			map[string]string{"Authorization": "Bearer key-chat"},
			`{"model":"llama3.2","messages":[],"model":"qwen3"}`, 200, "chat-ui", "qwen3", "",
			[2]string{"chat-ui", metrics.ResultAllow}},
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
	checkErrorBody(t, rec.Body.Bytes(), "consumers_unavailable")
	// Counted, so an outage of Vault shows in aisa's metrics as denials and not as less traffic.
	if n := testutil.ToFloat64(m.Decisions.WithLabelValues(metrics.ConsumerUnknown, metrics.ResultUnavailable)); n != 1 {
		t.Errorf("aisa_decisions_total{unknown,unavailable} = %v, want 1", n)
	}
}

func TestBodySize(t *testing.T) {
	// A prompt that fills the body up to the limit, with the model after it.
	body := func(size int) string {
		const head, tail = `{"messages":[{"role":"user","content":"`, `"}],"model":"qwen3"}`
		return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	}
	for name, tc := range map[string]struct {
		size       int
		wantStatus int
		wantCode   string
	}{
		"as large as the limit":     {maxBody, 200, ""},
		"one byte over the limit":   {maxBody + 1, 413, "request_too_large"},
		"twice as large as allowed": {2 * maxBody, 413, "request_too_large"},
	} {
		h, m := newHandler(false)
		req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(body(tc.size)))
		req.Header.Set("Authorization", "Bearer key-chat")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, tc.wantStatus)
			continue
		}
		if tc.wantCode != "" {
			checkErrorBody(t, rec.Body.Bytes(), tc.wantCode)
			if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", metrics.ResultInvalid)); n != 1 {
				t.Errorf("%s: aisa_decisions_total{chat-ui,invalid} = %v, want 1", name, n)
			}
		}
	}
}

func TestBodySizeCountsWhatFollowsTheObject(t *testing.T) {
	// The model comes first; the scan could stop right after it.
	const obj = `{"model":"qwen3"}`
	for name, tc := range map[string]struct {
		body       string
		wantStatus int
	}{
		"white space up to the limit": {obj + strings.Repeat(" ", maxBody-len(obj)), 200},
		"white space over the limit":  {obj + strings.Repeat(" ", maxBody), 413},
		"not JSON over the limit":     {`[` + strings.Repeat("x", maxBody), 413},
	} {
		h, _ := newHandler(false)
		req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer key-chat")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, tc.wantStatus)
		}
	}
}

func TestLargeBodyWithTheHeaderIsNotRead(t *testing.T) {
	h, _ := newHandler(false)
	body := &countingReader{r: strings.NewReader(strings.Repeat("x", 1<<20))}
	req := httptest.NewRequest(http.MethodPost, "/v1/decide", body)
	req.Header.Set("Authorization", "Bearer key-chat")
	req.Header.Set(HeaderRequestedModel, "qwen3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || body.n != 0 {
		t.Errorf("status = %d, %d bytes of the body read; want 200 and none", rec.Code, body.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// BenchmarkModelAfterLargePrompt shows what finding the model costs when a gateway forwards a
// body with a large prompt: the memory per request must stay far below the size of the body.
func BenchmarkModelAfterLargePrompt(b *testing.B) {
	messages := strings.Repeat(`{"role":"user","content":"`+strings.Repeat("x", 1000)+`"},`, 8000) // 8 MiB
	body := `{"messages":[` + strings.TrimSuffix(messages, ",") + `],"model":"qwen3"}`
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(body))
		if m, err := requestedModel(req); err != nil || m != "qwen3" {
			b.Fatalf("model = %q, %v", m, err)
		}
	}
}

// TestModelOfAgreesWithEncodingJSON compares the scanner with the standard library on bodies
// that are valid JSON: what aisa takes for the model must be what a backend reads.
func TestModelOfAgreesWithEncodingJSON(t *testing.T) {
	for _, body := range modelBodies {
		// Into a map: a struct would also take "Model", which Go matches without regard to case
		// and a backend does not.
		var want map[string]any
		if err := json.Unmarshal([]byte(body), &want); err != nil {
			t.Fatalf("the test's own body is not valid JSON: %s: %v", body, err)
		}
		wantModel, _ := want["model"].(string)
		got, err := modelOf(strings.NewReader(body))
		if len(strings.TrimSpace(wantModel)) > maxModel {
			if !errors.Is(err, errModelTooLong) || got != "" {
				t.Errorf("%s: model = %q, %v, want errModelTooLong", body[:40], got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", body, err)
		}
		if got != wantModel {
			t.Errorf("%s: model = %q, encoding/json reads %q", body, got, wantModel)
		}
	}
}

var modelBodies = []string{
	`{}`,
	`{"model":"qwen3"}`,
	` { "model" : "qwen3" } `,
	"{\n\t\"model\":\r\n\"qwen3\"\n}",
	`{"model":""}`,
	`{"model":null}`,
	`{"model":42}`,
	`{"model":-1.5e3,"x":1}`,
	`{"model":true}`,
	`{"model":["qwen3"]}`,
	`{"model":{"model":"inner"}}`,
	`{"model":{"model":"inner"},"model":"outer"}`,
	`{"model":"first","model":"last"}`,
	`{"model":"first","model":7}`,
	`{"Model":"upper"}`,
	`{"models":"plural","mode":"short"}`,
	`{"\u006dodel":"escaped key"}`,
	`{"model":"qw\u0065n3"}`,
	`{"model":"with \"quotes\" and \\ backslash"}`,
	`{"model":"ends with a backslash \\"}`,
	`{"model":"snow \u2603 and clef \ud834\udd1e"}`,
	`{"model":"ünïcödé ☃"}`,
	`{"a":"}","b":"]","c":"{\"model\":\"in a string\"}","model":"qwen3"}`,
	`{"a":"\\","model":"after a string that ends with a backslash"}`,
	`{"a":"\\\"","model":"after an escaped backslash and quote"}`,
	`{"messages":[{"role":"user","content":"hi","model":"in a message"}],"model":"qwen3"}`,
	`{"messages":[[[{"model":"deep"}]],{"a":{"b":{"model":"deeper"}}}],"model":"qwen3"}`,
	`{"messages":[{"model":"only in a message"}]}`,
	`{"n":0,"f":false,"z":null,"e":1e-9,"model":"after literals"}`,
	`{"a":[],"b":{},"c":[{}],"d":"","model":"after empty values"}`,
	`{"model":"qwen3","stream":true,"max_tokens":32,"temperature":0}`,
	`{"model":"` + strings.Repeat("m", maxModel) + `"}`,
	`{"model":"` + strings.Repeat("m", maxModel+1) + `"}`,
	`{"model":"` + strings.Repeat(`\u006d`, maxModel) + `"}`,
	`{"model":"` + strings.Repeat("m", maxString-2) + `"}`,
	`{"model":"` + strings.Repeat("m", 4*maxString) + `","model":"qwen3"}`,
	`{"model":"qwen3","model":"` + strings.Repeat("m", 4*maxString) + `"}`,
	`{"model":"` + strings.Repeat("m", 4*maxString) + `"}`,
	`{"` + strings.Repeat("k", 4*maxString) + `":"long key","model":"qwen3"}`,
	`{"model":"qwen3","content":"` + strings.Repeat("x", 100_000) + `"}`,
	`{"content":"` + strings.Repeat(`\"`, 50_000) + `","model":"qwen3"}`,
}

func TestModelOfRejects(t *testing.T) {
	for _, body := range []string{
		``, ` `, `null`, `"model"`, `42`, `[{"model":"qwen3"}]`,
		`{`, `{"model"`, `{"model":`, `{"model":"qwen3"`, `{"model":"qwen3`, `{"model":"qwen3",`,
		`{"model" "qwen3"}`, `{"model":"qwen3" "x":1}`, `{model:"qwen3"}`, `{"model":,}`,
		`{"messages":[{"role":"user"`, `{"a":"unterminated`, `{"a":"\`,
	} {
		if m, err := modelOf(strings.NewReader(body)); err == nil {
			t.Errorf("%q: model = %q, want an error", body, m)
		}
	}
}

// FuzzModelOf looks for bodies that are valid JSON and on which the scanner and the standard
// library disagree. `go test` runs it on the bodies above only; `go test -fuzz=FuzzModelOf
// ./internal/decide` searches.
func FuzzModelOf(f *testing.F) {
	for _, body := range modelBodies {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		got, err := modelOf(strings.NewReader(body))
		var want map[string]any
		if json.Unmarshal([]byte(body), &want) != nil {
			return // not valid JSON, or not an object: the backend rejects it
		}
		wantModel, _ := want["model"].(string)
		if len(strings.TrimSpace(wantModel)) > maxModel {
			if !errors.Is(err, errModelTooLong) {
				t.Fatalf("a model of %d bytes: %v, want errModelTooLong\n%s", len(wantModel), err, body)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid JSON, and modelOf fails: %v\n%s", err, body)
		}
		if got != wantModel {
			t.Fatalf("model = %q, encoding/json reads %q\n%s", got, wantModel, body)
		}
	})
}

// fakeQuota answers every check with a fixed verdict and notes who was checked.
type fakeQuota struct {
	verdict quotas.Verdict
	checked []consumers.Consumer
}

func (f *fakeQuota) Check(_ context.Context, c consumers.Consumer) quotas.Verdict {
	f.checked = append(f.checked, c)
	return f.verdict
}

func TestQuota(t *testing.T) {
	for _, tc := range []struct {
		name           string
		verdict        quotas.Verdict
		wantStatus     int
		wantCode       string
		wantResult     string
		wantRetryAfter string
	}{
		{"tokens left", quotas.Verdict{Outcome: quotas.Allow, Limit: 100, Used: 99}, 200, "", metrics.ResultAllow, ""},
		{"exhausted", quotas.Verdict{Outcome: quotas.Exhausted, Limit: 100, Used: 120, RetryAfter: 1500 * time.Millisecond},
			429, "rate_limit_exceeded", metrics.ResultDenyQuota, "2"},
		{"profile unknown", quotas.Verdict{Outcome: quotas.Unavailable, Err: quotas.ErrUnknownProfile},
			503, "quota_unavailable", metrics.ResultUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			q := &fakeQuota{verdict: tc.verdict}
			l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
			h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)))

			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"qwen3"}`))
			req.Header.Set("Authorization", "Bearer key-chat")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if len(q.checked) != 1 || q.checked[0].QuotaProfile != "interactive" {
				t.Errorf("checked = %+v, want chat-ui with its profile", q.checked)
			}
			if tc.wantCode != "" {
				checkErrorBody(t, rec.Body.Bytes(), tc.wantCode)
				if rec.Header().Get(HeaderConsumer) != "" {
					t.Error("a denial must not carry X-Aisa-Consumer")
				}
			}
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
			if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", tc.wantResult)); n != 1 {
				t.Errorf("aisa_decisions_total{chat-ui,%s} = %v, want 1", tc.wantResult, n)
			}
		})
	}
}

func TestQuotaNotCheckedForInvalidRequests(t *testing.T) {
	q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Exhausted}}
	l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui"}}}
	h := New(l, q, metrics.New("test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, auth := range []string{"Bearer nope", "Bearer key-chat"} {
		// The second has a known key but no model.
		req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{}`))
		req.Header.Set("Authorization", auth)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(q.checked) != 0 {
		t.Errorf("quota checked for %d invalid requests", len(q.checked))
	}
}
