package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "test-backend"
	}
	if cfg.DefaultCompletionTokens == 0 {
		cfg.DefaultCompletionTokens = 5
	}
	cfg.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	srv := httptest.NewServer(newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
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

func TestNonStreamingUsage(t *testing.T) {
	srv := newTestServer(t, Config{})
	resp := post(t, srv, `{"model":"qwen3","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello there world"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Mock-Backend"); got != "test-backend" {
		t.Errorf("X-Mock-Backend = %q", got)
	}
	var out struct {
		Model             string `json:"model"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	want := usage{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10}
	if out.Usage != want {
		t.Errorf("usage = %+v, want %+v", out.Usage, want)
	}
	if out.SystemFingerprint != "test-backend" {
		t.Errorf("system_fingerprint = %q", out.SystemFingerprint)
	}
	if out.Model != "qwen3" {
		t.Errorf("model = %q", out.Model)
	}
	if got := out.Choices[0].Message.Content; got != "lorem ipsum dolor sit amet" {
		t.Errorf("content = %q", got)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
}

func TestCompletionTokenLimits(t *testing.T) {
	srv := newTestServer(t, Config{DefaultCompletionTokens: 10})
	tests := []struct {
		name       string
		body       string
		header     map[string]string
		wantTokens int
		wantFinish string
	}{
		{"default", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil, 10, "stop"},
		{"max_tokens lowers", `{"model":"m","max_tokens":3,"messages":[{"role":"user","content":"hi"}]}`, nil, 3, "length"},
		{"max_completion_tokens wins", `{"model":"m","max_tokens":3,"max_completion_tokens":2,"messages":[{"role":"user","content":"hi"}]}`, nil, 2, "length"},
		{"limit above default", `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, nil, 10, "stop"},
		{"header overrides", `{"model":"m","max_tokens":3,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Mock-Completion-Tokens": "42"}, 42, "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, srv, tt.body, tt.header)
			var out struct {
				Choices []struct {
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage usage `json:"usage"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if out.Usage.CompletionTokens != tt.wantTokens {
				t.Errorf("completion_tokens = %d, want %d", out.Usage.CompletionTokens, tt.wantTokens)
			}
			if out.Choices[0].FinishReason != tt.wantFinish {
				t.Errorf("finish_reason = %q, want %q", out.Choices[0].FinishReason, tt.wantFinish)
			}
		})
	}
}

// readStream returns the decoded data chunks of an SSE response and whether it ended with [DONE].
func readStream(t *testing.T, r io.Reader) ([]map[string]any, bool) {
	t.Helper()
	var chunks []map[string]any
	done := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			t.Fatalf("unexpected line %q", line)
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("chunk %q: %v", data, err)
		}
		chunks = append(chunks, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return chunks, done
}

func TestStreaming(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		opts      string
		wantUsage bool
	}{
		{"auto without include_usage", usageAuto, ``, false},
		{"auto with include_usage", usageAuto, `,"stream_options":{"include_usage":true}`, true},
		{"always", usageAlways, ``, true},
		{"never ignores include_usage", usageNever, `,"stream_options":{"include_usage":true}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, Config{DefaultCompletionTokens: 3, StreamUsage: tt.mode})
			resp := post(t, srv, `{"model":"m","stream":true,"messages":[{"role":"user","content":"one two"}]`+tt.opts+`}`, nil)
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("Content-Type = %q", ct)
			}
			chunks, done := readStream(t, resp.Body)
			if !done {
				t.Error("stream did not end with [DONE]")
			}
			// role chunk + 3 token chunks + finish chunk (+ usage chunk)
			want := 5
			if tt.wantUsage {
				want++
			}
			if len(chunks) != want {
				t.Fatalf("got %d chunks, want %d", len(chunks), want)
			}
			var text strings.Builder
			for _, c := range chunks {
				for _, ch := range c["choices"].([]any) {
					delta := ch.(map[string]any)["delta"].(map[string]any)
					if s, ok := delta["content"].(string); ok {
						text.WriteString(s)
					}
				}
			}
			if text.String() != "lorem ipsum dolor" {
				t.Errorf("streamed text = %q", text.String())
			}
			last := chunks[len(chunks)-1]
			u, hasUsage := last["usage"].(map[string]any)
			if hasUsage != tt.wantUsage {
				t.Fatalf("usage chunk present = %v, want %v", hasUsage, tt.wantUsage)
			}
			if hasUsage {
				if u["prompt_tokens"] != 2.0 || u["completion_tokens"] != 3.0 || u["total_tokens"] != 5.0 {
					t.Errorf("usage = %v", u)
				}
				if n := len(last["choices"].([]any)); n != 0 {
					t.Errorf("usage chunk has %d choices, want 0", n)
				}
			}
		})
	}
}

func TestContentParts(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"a b c"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"d"}]`)},
		{Role: "assistant", Content: json.RawMessage(`null`)},
	}
	if got := promptTokens(msgs); got != 4 {
		t.Errorf("promptTokens = %d, want 4", got)
	}
}

func TestErrors(t *testing.T) {
	srv := newTestServer(t, Config{Models: []string{"qwen3"}})
	tests := []struct {
		name   string
		body   string
		header map[string]string
		status int
		code   string
	}{
		{"invalid json", `{`, nil, http.StatusBadRequest, "invalid_json"},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, nil, http.StatusBadRequest, "missing_model"},
		{"unknown model", `{"model":"other","messages":[{"role":"user","content":"hi"}]}`, nil, http.StatusNotFound, "model_not_found"},
		{"no messages", `{"model":"qwen3","messages":[]}`, nil, http.StatusBadRequest, "missing_messages"},
		{"bad header", `{"model":"qwen3","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Mock-Completion-Tokens": "lots"}, http.StatusBadRequest, "invalid_token_count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, srv, tt.body, tt.header)
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			var out struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if out.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", out.Error.Code, tt.code)
			}
		})
	}
}

func TestModels(t *testing.T) {
	srv := newTestServer(t, Config{Models: []string{"qwen3", "llama3.2"}})
	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 || out.Data[0].ID != "qwen3" || out.Data[1].ID != "llama3.2" {
		t.Errorf("models = %+v", out.Data)
	}
}
