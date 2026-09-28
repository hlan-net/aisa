package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestStub(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := Config{
		Keys:     map[string]string{"key-chat": "chat-ui", "key-batch": "batch-jobs", "key-blocked": "blocked"},
		Rewrites: map[string]map[string]string{"batch-jobs": {"cloud-large": "qwen3"}},
		Deny:     []string{"blocked"},
		Capacity: 3,
		Now:      func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	srv := httptest.NewServer(newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func records(t *testing.T, srv *httptest.Server, kind string) []Record {
	t.Helper()
	resp := do(t, srv, http.MethodGet, "/debug/requests?kind="+kind, "", nil)
	var out []Record
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		body         string
		header       map[string]string
		wantStatus   int
		wantConsumer string
		wantModel    string
		wantSource   string
	}{
		{"model from body", http.MethodPost, `{"model":"qwen3","messages":[]}`,
			map[string]string{"Authorization": "Bearer key-chat"}, 200, "chat-ui", "qwen3", "body"},
		{"header wins over body", http.MethodPost, `{"model":"qwen3"}`,
			map[string]string{"Authorization": "Bearer key-chat", "X-Aisa-Requested-Model": "llama3.2"}, 200, "chat-ui", "llama3.2", "header"},
		{"GET with header", http.MethodGet, ``,
			map[string]string{"Authorization": "Bearer key-chat", "X-Aisa-Requested-Model": "qwen3"}, 200, "chat-ui", "qwen3", "header"},
		{"downgrade rewrite", http.MethodPost, `{"model":"cloud-large"}`,
			map[string]string{"Authorization": "Bearer key-batch"}, 200, "batch-jobs", "qwen3", "body"},
		{"rewrite is per consumer", http.MethodPost, `{"model":"cloud-large"}`,
			map[string]string{"Authorization": "Bearer key-chat"}, 200, "chat-ui", "cloud-large", "body"},
		{"unknown key", http.MethodPost, `{"model":"qwen3"}`,
			map[string]string{"Authorization": "Bearer nope"}, 401, "", "", ""},
		{"no credential", http.MethodPost, `{"model":"qwen3"}`, nil, 401, "", "", ""},
		{"denied consumer", http.MethodPost, `{"model":"qwen3"}`,
			map[string]string{"Authorization": "Bearer key-blocked"}, 429, "", "", "body"},
		{"no model", http.MethodPost, `not json`,
			map[string]string{"Authorization": "Bearer key-chat"}, 400, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestStub(t)
			resp := do(t, srv, tt.method, "/v1/decide", tt.body, tt.header)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Header.Get("X-Aisa-Consumer"); got != tt.wantConsumer {
				t.Errorf("X-Aisa-Consumer = %q, want %q", got, tt.wantConsumer)
			}
			if got := resp.Header.Get("X-Aisa-Model"); got != tt.wantModel {
				t.Errorf("X-Aisa-Model = %q, want %q", got, tt.wantModel)
			}
			if tt.wantStatus != http.StatusOK {
				var e struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error.Message == "" {
					t.Errorf("want an OpenAI-style error body, err=%v", err)
				}
			}
			recs := records(t, srv, "decide")
			if len(recs) != 1 {
				t.Fatalf("recorded %d decide requests, want 1", len(recs))
			}
			if recs[0].Status != tt.wantStatus || recs[0].ModelSource != tt.wantSource {
				t.Errorf("record = %+v", recs[0])
			}
			if a := recs[0].Headers["Authorization"]; strings.Contains(a, "key-") {
				t.Errorf("Authorization not redacted: %q", a)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	srv := newTestStub(t)

	resp := do(t, srv, http.MethodPost, "/v1/usage", `{"request_id":"a","prompt_tokens":1}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single event: status = %d", resp.StatusCode)
	}
	resp = do(t, srv, http.MethodPost, "/v1/usage", `[{"request_id":"b"},{"request_id":"c"}]`, nil)
	var out map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out["accepted"] != 2 {
		t.Fatalf("batch: accepted = %v, err = %v", out, err)
	}
	resp = do(t, srv, http.MethodPost, "/v1/usage", `nope`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid body: status = %d", resp.StatusCode)
	}

	recs := records(t, srv, "usage")
	if len(recs) != 3 {
		t.Fatalf("recorded %d usage requests, want 3", len(recs))
	}
	if recs[0].Events != 1 || recs[1].Events != 2 || recs[2].RawBody != "nope" {
		t.Errorf("records = %+v", recs)
	}
}

func TestRecordCapacityAndClear(t *testing.T) {
	srv := newTestStub(t)
	for _, id := range []string{"1", "2", "3", "4"} {
		do(t, srv, http.MethodPost, "/v1/usage", `{"request_id":"`+id+`"}`, nil)
	}
	recs := records(t, srv, "")
	if len(recs) != 3 {
		t.Fatalf("kept %d records, want capacity 3", len(recs))
	}
	if string(recs[0].Body) != `{"request_id":"2"}` {
		t.Errorf("oldest kept = %s, want request 2", recs[0].Body)
	}

	resp := do(t, srv, http.MethodDelete, "/debug/requests", "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: status = %d", resp.StatusCode)
	}
	if n := len(records(t, srv, "")); n != 0 {
		t.Errorf("%d records after clear", n)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("STUB_KEYS", "k1=chat-ui, k2=batch-jobs")
	t.Setenv("STUB_REWRITES", "batch-jobs:cloud-large=qwen3")
	t.Setenv("STUB_DENY", "blocked")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keys["k2"] != "batch-jobs" || cfg.Rewrites["batch-jobs"]["cloud-large"] != "qwen3" || cfg.Deny[0] != "blocked" {
		t.Errorf("cfg = %+v", cfg)
	}

	t.Setenv("STUB_REWRITES", "batch-jobs=qwen3")
	if _, err := configFromEnv(); err == nil {
		t.Error("want an error for a malformed rewrite")
	}
}
