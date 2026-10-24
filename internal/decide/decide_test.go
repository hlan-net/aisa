package decide

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/budgets"
	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/models"
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
	return New(l, nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{}), m
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
			h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})

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
			checkDenial(t, rec, tc.wantCode)
			if got := rec.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
			if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", tc.wantResult)); n != 1 {
				t.Errorf("aisa_decisions_total{chat-ui,%s} = %v, want 1", tc.wantResult, n)
			}
		})
	}
}

// checkDenial checks the error body and the absence of X-Aisa-Consumer of a denial; wantCode ""
// is an allowed request.
func checkDenial(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	if wantCode == "" {
		return
	}
	checkErrorBody(t, rec.Body.Bytes(), wantCode)
	if rec.Header().Get(HeaderConsumer) != "" {
		t.Error("a denial must not carry X-Aisa-Consumer")
	}
}

func TestQuotaNotCheckedForInvalidRequests(t *testing.T) {
	q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Exhausted}}
	l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui"}}}
	h := New(l, q, metrics.New("test"), slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
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

// TestDecideDuration checks that each answer is timed once, under the result it counted, and
// that the stages it went through are timed too.
func TestDecideDuration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		key        string
		verdict    quotas.Verdict
		wantResult string
		wantStages map[string]uint64
	}{
		{"allow", "key-chat", quotas.Verdict{Outcome: quotas.Allow}, metrics.ResultAllow,
			map[string]uint64{metrics.StageConsumer: 1, metrics.StageQuota: 1}},
		{"unknown key", "nope", quotas.Verdict{Outcome: quotas.Allow}, metrics.ResultDenyAuth,
			map[string]uint64{metrics.StageConsumer: 1, metrics.StageQuota: 0}},
		{"quota exhausted", "key-chat", quotas.Verdict{Outcome: quotas.Exhausted, Limit: 1, Used: 1}, metrics.ResultDenyQuota,
			map[string]uint64{metrics.StageConsumer: 1, metrics.StageQuota: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
			h := New(l, &fakeQuota{verdict: tc.verdict}, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})

			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"qwen3"}`))
			req.Header.Set("Authorization", "Bearer "+tc.key)
			h.ServeHTTP(httptest.NewRecorder(), req)

			got := histogramCounts(t, m, "aisa_decide_duration_seconds", "result")
			if len(got) != 1 || got[tc.wantResult] != 1 {
				t.Errorf("aisa_decide_duration_seconds counts = %v, want 1 under %s only", got, tc.wantResult)
			}
			stages := histogramCounts(t, m, "aisa_decide_stage_duration_seconds", "stage")
			for stage, want := range tc.wantStages {
				if stages[stage] != want {
					t.Errorf("aisa_decide_stage_duration_seconds{stage=%q} count = %d, want %d", stage, stages[stage], want)
				}
			}
		})
	}
}

// histogramCounts returns the sample counts of a histogram by the value of one label.
func histogramCounts(t *testing.T, m *metrics.Metrics, name, label string) map[string]uint64 {
	t.Helper()
	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetName() == label {
					counts[l.GetValue()] += s.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return counts
}

func TestNoQuotaProfile(t *testing.T) {
	for _, tc := range []struct {
		name, defaultProfile string
		wantStatus           int
		wantCode, wantResult string
		wantChecked          string
	}{
		{"denied without a default", "", 403, "no_quota", metrics.ResultNoQuota, ""},
		{"the default applies", "batch", 200, "", metrics.ResultAllow, "batch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Allow}}
			l := fakeLookup{keys: map[string]consumers.Consumer{"key-open": {Name: "open-ended"}}}
			h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{DefaultQuotaProfile: tc.defaultProfile})

			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"qwen3"}`))
			req.Header.Set("Authorization", "Bearer key-open")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			checkDenial(t, rec, tc.wantCode)
			if n := testutil.ToFloat64(m.Decisions.WithLabelValues("open-ended", tc.wantResult)); n != 1 {
				t.Errorf("aisa_decisions_total{open-ended,%s} = %v, want 1", tc.wantResult, n)
			}
			switch {
			case tc.wantChecked == "" && len(q.checked) != 0:
				t.Errorf("quota checked for a denied consumer: %+v", q.checked)
			case tc.wantChecked != "" && (len(q.checked) != 1 || q.checked[0].QuotaProfile != tc.wantChecked):
				t.Errorf("checked = %+v, want the profile %s", q.checked, tc.wantChecked)
			}
		})
	}
}

type fakeBudget struct {
	verdict budgets.Verdict
	checked []string // "consumer/model"
}

func (f *fakeBudget) Check(_ context.Context, consumer, model string) budgets.Verdict {
	f.checked = append(f.checked, consumer+"/"+model)
	return f.verdict
}

func TestBudget(t *testing.T) {
	for _, tc := range []struct {
		name          string
		verdict       budgets.Verdict
		wantStatus    int
		wantModel     string
		wantCode      string
		wantResult    string
		wantRemaining string
	}{
		{"no budget", budgets.Verdict{Outcome: budgets.NoBudget}, 200, "cloud-large", "", metrics.ResultAllow, ""},
		{"tokens left", budgets.Verdict{Outcome: budgets.Allow, Applies: true, Limit: 100, Used: 40, Remaining: 60},
			200, "cloud-large", "", metrics.ResultAllow, "60"},
		{"soft limit", budgets.Verdict{Outcome: budgets.Allow, Applies: true, Limit: 100, Used: 90, Remaining: 10, Soft: true},
			200, "cloud-large", "", metrics.ResultAllow, "10"},
		{"exhausted, a free model", budgets.Verdict{Outcome: budgets.Allow, Applies: false, Limit: 100, Used: 100, Remaining: 0},
			200, "cloud-large", "", metrics.ResultAllow, "0"},
		{"exhausted, reject", budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, Limit: 100, Used: 100, OnExhausted: budgets.Reject},
			429, "", "budget_exhausted", metrics.ResultDenyBudget, ""},
		{"exhausted, downgrade", budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, Limit: 100, Used: 120, OnExhausted: budgets.Downgrade, DowngradeTo: "qwen3"},
			200, "qwen3", "", metrics.ResultDowngrade, "0"},
		{"exhausted, downgrade to itself", budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, Limit: 100, Used: 120, OnExhausted: budgets.Downgrade, DowngradeTo: "cloud-large"},
			429, "", "budget_exhausted", metrics.ResultDenyBudget, ""},
		{"broken budget", budgets.Verdict{Outcome: budgets.Unavailable, Err: errors.New("monthly_tokens is missing")},
			503, "", "budget_unavailable", metrics.ResultUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			b := &fakeBudget{verdict: tc.verdict}
			q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Allow}}
			l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
			h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Budget: b})

			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"cloud-large"}`))
			req.Header.Set("Authorization", "Bearer key-chat")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if got := rec.Header().Get(HeaderModel); got != tc.wantModel {
				t.Errorf("X-Aisa-Model = %q, want %q", got, tc.wantModel)
			}
			if got := rec.Header().Get(HeaderBudgetRemaining); got != tc.wantRemaining {
				t.Errorf("X-Aisa-Budget-Remaining = %q, want %q", got, tc.wantRemaining)
			}
			checkDenial(t, rec, tc.wantCode)
			if rec.Code == 429 && rec.Header().Get("Retry-After") != "" {
				t.Error("a budget denial carries no Retry-After: the month decides")
			}
			if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", tc.wantResult)); n != 1 {
				t.Errorf("aisa_decisions_total{chat-ui,%s} = %v, want 1", tc.wantResult, n)
			}
			if len(b.checked) != 1 || b.checked[0] != "chat-ui/cloud-large" {
				t.Errorf("budget checked for %v, want chat-ui/cloud-large once", b.checked)
			}
		})
	}
}

// The budget is checked after the quota: an exhausted quota is a 429 of its own, and the budget
// is not asked.
func TestBudgetAfterQuota(t *testing.T) {
	m := metrics.New("test")
	b := &fakeBudget{verdict: budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, OnExhausted: budgets.Reject}}
	q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Exhausted, Limit: 10, Used: 10, RetryAfter: time.Second}}
	l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
	h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Budget: b})
	req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"cloud-large"}`))
	req.Header.Set("Authorization", "Bearer key-chat")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || len(b.checked) != 0 {
		t.Errorf("status = %d, budget checked %v", rec.Code, b.checked)
	}
	checkDenial(t, rec, "rate_limit_exceeded")
}

type fakeProvider struct {
	verdict budgets.Verdict
	checked []string
}

// Check answers with the verdict for cloud-large; any other model has no provider budget.
func (f *fakeProvider) Check(_ context.Context, model string) budgets.Verdict {
	f.checked = append(f.checked, model)
	if model != "cloud-large" {
		return budgets.Verdict{Outcome: budgets.NoBudget}
	}
	return f.verdict
}

// The ceiling above the consumers: the budgets of the provider keys that serve the model.
func TestProviderBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		consumer   budgets.Verdict
		provider   budgets.Verdict
		wantStatus int
		wantModel  string
		wantCode   string
		wantResult string
		wantRemain string
		wantAsked  string
	}{
		{"no provider budget", budgets.Verdict{Outcome: budgets.NoBudget}, budgets.Verdict{Outcome: budgets.NoBudget},
			200, "cloud-large", "", metrics.ResultAllow, "", "cloud-large"},
		{"tokens left", budgets.Verdict{Outcome: budgets.NoBudget}, budgets.Verdict{Outcome: budgets.Allow, Applies: true, Provider: "cloud", Remaining: 5},
			200, "cloud-large", "", metrics.ResultAllow, "", "cloud-large"},
		{"exhausted, reject", budgets.Verdict{Outcome: budgets.Allow, Applies: true, Remaining: 60}, budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, Provider: "cloud", OnExhausted: budgets.Reject, Used: 10, Limit: 10},
			429, "", "provider_budget_exhausted", metrics.ResultDenyProviderBudget, "", "cloud-large"},
		{"exhausted, downgrade", budgets.Verdict{Outcome: budgets.Allow, Applies: true, Remaining: 60}, budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, Provider: "cloud", OnExhausted: budgets.Downgrade, DowngradeTo: "qwen3"},
			200, "qwen3", "", metrics.ResultDowngrade, "60", "cloud-large qwen3"},
		{"broken", budgets.Verdict{Outcome: budgets.NoBudget}, budgets.Verdict{Outcome: budgets.Unavailable, Provider: "cloud", Err: errors.New("monthly_tokens is missing")},
			503, "", "budget_unavailable", metrics.ResultUnavailable, "", "cloud-large"},
		// The consumer's downgrade comes first: the provider is asked about the model the
		// request goes to.
		{"after the consumer's downgrade", budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, OnExhausted: budgets.Downgrade, DowngradeTo: "qwen3"}, budgets.Verdict{Outcome: budgets.NoBudget},
			200, "qwen3", "", metrics.ResultDowngrade, "0", "qwen3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			p := &fakeProvider{verdict: tc.provider}
			b := &fakeBudget{verdict: tc.consumer}
			q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Allow}}
			l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
			h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Budget: b, Provider: p})

			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"cloud-large"}`))
			req.Header.Set("Authorization", "Bearer key-chat")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if got := rec.Header().Get(HeaderModel); got != tc.wantModel {
				t.Errorf("X-Aisa-Model = %q, want %q", got, tc.wantModel)
			}
			if got := rec.Header().Get(HeaderBudgetRemaining); got != tc.wantRemain {
				t.Errorf("X-Aisa-Budget-Remaining = %q, want %q", got, tc.wantRemain)
			}
			checkDenial(t, rec, tc.wantCode)
			if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", tc.wantResult)); n != 1 {
				t.Errorf("aisa_decisions_total{chat-ui,%s} = %v, want 1", tc.wantResult, n)
			}
			if got := strings.Join(p.checked, " "); got != tc.wantAsked {
				t.Errorf("provider asked about %q, want %q", got, tc.wantAsked)
			}
		})
	}
}

// A provider downgrade's model is checked against the provider budgets too, and not downgraded
// again; a broken consumer budget leaves a free model's request alone, without the header.
func TestProviderDowngradeTargetAndBrokenBudget(t *testing.T) {
	l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
	q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Allow}}
	ask := func(t *testing.T, opts Options, model string) (*httptest.ResponseRecorder, *metrics.Metrics) {
		t.Helper()
		m := metrics.New("test")
		h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
		req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"`+model+`"}`))
		req.Header.Set("Authorization", "Bearer key-chat")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, m
	}

	// Both keys exhausted, the first downgrading to the second's model: 429, after two checks.
	exhausted := &exhaustedProviders{to: map[string]string{"cloud-large": "other-cloud", "other-cloud": ""}}
	rec, m := ask(t, Options{Provider: exhausted}, "cloud-large")
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429; body %s", rec.Code, rec.Body)
	}
	checkDenial(t, rec, "provider_budget_exhausted")
	if !slices.Equal(exhausted.checked, []string{"cloud-large", "other-cloud"}) {
		t.Errorf("provider checked %v, want cloud-large then other-cloud", exhausted.checked)
	}
	if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", metrics.ResultDenyProviderBudget)); n != 1 {
		t.Errorf("deny_provider_budget = %v, want 1", n)
	}
	// The downgrade's model has tokens left: served there, checked twice.
	open := &exhaustedProviders{to: map[string]string{"cloud-large": "qwen3"}}
	rec, _ = ask(t, Options{Provider: open}, "cloud-large")
	if rec.Code != 200 || rec.Header().Get(HeaderModel) != "qwen3" || len(open.checked) != 2 {
		t.Errorf("status = %d, model = %q, checked %v", rec.Code, rec.Header().Get(HeaderModel), open.checked)
	}

	// A broken consumer budget on a free model: allowed, no header.
	b := &fakeBudget{verdict: budgets.Verdict{Outcome: budgets.Allow, Err: errors.New("monthly_tokens is missing")}}
	rec, _ = ask(t, Options{Budget: b}, "qwen3")
	if rec.Code != 200 || rec.Header().Get(HeaderBudgetRemaining) != "" {
		t.Errorf("status = %d, X-Aisa-Budget-Remaining = %q, want 200 and none", rec.Code, rec.Header().Get(HeaderBudgetRemaining))
	}
}

// exhaustedProviders is exhausted for every model in to, downgrading to its value when it is
// not empty; other models have no provider budget.
type exhaustedProviders struct {
	to      map[string]string
	checked []string
}

func (f *exhaustedProviders) Check(_ context.Context, model string) budgets.Verdict {
	f.checked = append(f.checked, model)
	to, ok := f.to[model]
	if !ok {
		return budgets.Verdict{Outcome: budgets.NoBudget}
	}
	v := budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, Provider: "p-" + model, OnExhausted: budgets.Reject}
	if to != "" {
		v.OnExhausted, v.DowngradeTo = budgets.Downgrade, to
	}
	return v
}

// fakeModels knows llama3.2 and qwen3, and llama3.2:3b and small as aliases.
type fakeModels struct{ outcome models.Outcome }

func (f fakeModels) Resolve(name string) (string, models.Outcome) {
	if f.outcome == models.NotLoaded {
		return name, models.NotLoaded
	}
	switch name {
	case "llama3.2", "qwen3":
		return name, models.Model
	case "llama3.2:3b":
		return "llama3.2", models.Alias
	case "small":
		return "qwen3", models.Alias
	}
	return name, models.Unknown
}

// An alias is decided as its model (#89): the budget sees the model, and X-Aisa-Model names it.
// A name that is neither is 404 model_not_found, before the quota is asked.
func TestModelNames(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolver   Resolver
		model      string
		wantStatus int
		wantModel  string
		wantCode   string
		wantResult string
		wantBudget []string
	}{
		{"a model", fakeModels{outcome: models.Model}, "llama3.2", 200, "llama3.2", "", metrics.ResultAllow, []string{"chat-ui/llama3.2"}},
		{"an alias", fakeModels{outcome: models.Model}, "llama3.2:3b", 200, "llama3.2", "", metrics.ResultAllow, []string{"chat-ui/llama3.2"}},
		{"unknown", fakeModels{outcome: models.Model}, "llama3.2:1b", 404, "", "model_not_found", metrics.ResultUnknownModel, nil},
		{"not loaded yet", fakeModels{outcome: models.NotLoaded}, "llama3.2:1b", 200, "llama3.2:1b", "", metrics.ResultAllow, []string{"chat-ui/llama3.2:1b"}},
		{"no resolver", nil, "anything", 200, "anything", "", metrics.ResultAllow, []string{"chat-ui/anything"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New("test")
			b := &fakeBudget{verdict: budgets.Verdict{Outcome: budgets.NoBudget}}
			q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Allow}}
			l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
			h := New(l, q, m, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Budget: b, Models: tc.resolver})

			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"`+tc.model+`"}`))
			req.Header.Set("Authorization", "Bearer key-chat")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			checkAnswer(t, rec, m, tc.wantStatus, tc.wantModel, tc.wantCode, tc.wantResult)
			if !slices.Equal(b.checked, tc.wantBudget) {
				t.Errorf("budget checked %v, want %v", b.checked, tc.wantBudget)
			}
			if rec.Code == 404 && len(q.checked) != 0 {
				t.Error("the quota was asked about an unknown model")
			}
		})
	}
}

// A downgrade named by an alias goes to the alias's model.
func TestDowngradeToAnAlias(t *testing.T) {
	b := &fakeBudget{verdict: budgets.Verdict{Outcome: budgets.Exhausted, Applies: true, OnExhausted: budgets.Downgrade, DowngradeTo: "small"}}
	p := &exhaustedProviders{to: map[string]string{"llama3.2": "small"}}
	q := &fakeQuota{verdict: quotas.Verdict{Outcome: quotas.Allow}}
	l := fakeLookup{keys: map[string]consumers.Consumer{"key-chat": {Name: "chat-ui", QuotaProfile: "interactive"}}}
	for name, opts := range map[string]Options{
		"consumer budget": {Budget: b, Models: fakeModels{outcome: models.Model}},
		"provider budget": {Provider: p, Models: fakeModels{outcome: models.Model}},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(l, q, metrics.New("test"), slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
			req := httptest.NewRequest(http.MethodPost, "/v1/decide", strings.NewReader(`{"model":"llama3.2:3b"}`))
			req.Header.Set("Authorization", "Bearer key-chat")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 || rec.Header().Get(HeaderModel) != "qwen3" {
				t.Errorf("status = %d, X-Aisa-Model = %q, want 200 and qwen3; body %s", rec.Code, rec.Header().Get(HeaderModel), rec.Body)
			}
		})
	}
}

// checkAnswer checks a decision's status, X-Aisa-Model, error code and its count under chat-ui.
func checkAnswer(t *testing.T, rec *httptest.ResponseRecorder, m *metrics.Metrics, status int, model, code, result string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body)
	}
	if got := rec.Header().Get(HeaderModel); got != model {
		t.Errorf("X-Aisa-Model = %q, want %q", got, model)
	}
	checkDenial(t, rec, code)
	if n := testutil.ToFloat64(m.Decisions.WithLabelValues("chat-ui", result)); n != 1 {
		t.Errorf("aisa_decisions_total{chat-ui,%s} = %v, want 1", result, n)
	}
}
