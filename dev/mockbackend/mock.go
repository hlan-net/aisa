package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hlan-net/aisa/dev/internal/devutil"
)

// maxCompletionTokens caps generated output so a typo cannot stream forever.
const maxCompletionTokens = 4096

// Stream usage modes: whether a streamed response ends with a usage chunk.
const (
	usageAuto   = "auto"   // only when the client sets stream_options.include_usage (OpenAI behaviour)
	usageAlways = "always" // always, as some OpenAI-compatible servers do
	usageNever  = "never"  // never, to test gateways against servers without streamed usage
)

// Config controls the mock backend.
type Config struct {
	// Name identifies this backend instance in the X-Mock-Backend response header and in logs.
	Name string
	// Models lists the served models. An empty list accepts any model.
	Models []string
	// DefaultCompletionTokens is used when the request sets no token limit.
	DefaultCompletionTokens int
	// TTFT is the delay before the first token.
	TTFT time.Duration
	// TokenDelay is the delay between tokens in a streamed response.
	TokenDelay time.Duration
	// StreamUsage is one of usageAuto, usageAlways or usageNever.
	StreamUsage string
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

type server struct {
	cfg Config
	log *slog.Logger
}

func newServer(cfg Config, log *slog.Logger) http.Handler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StreamUsage == "" {
		cfg.StreamUsage = usageAuto
	}
	s := &server{cfg: cfg, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		devutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	return mux
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Stream              bool          `json:"stream"`
	MaxTokens           int           `json:"max_tokens"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(s.cfg.Models))
	for _, m := range s.cfg.Models {
		data = append(data, map[string]any{
			"id":       m,
			"object":   "model",
			"created":  0,
			"owned_by": s.cfg.Name,
		})
	}
	devutil.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Mock-Backend", s.cfg.Name)

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		devutil.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json",
			fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if req.Model == "" {
		devutil.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model is required")
		return
	}
	if len(s.cfg.Models) > 0 && !slices.Contains(s.cfg.Models, req.Model) {
		devutil.WriteOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("model %q is not served by %s", req.Model, s.cfg.Name))
		return
	}
	if len(req.Messages) == 0 {
		devutil.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_messages", "messages must not be empty")
		return
	}

	n, limited, err := s.completionTokens(r, req)
	if err != nil {
		devutil.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_count", err.Error())
		return
	}
	u := usage{PromptTokens: promptTokens(req.Messages), CompletionTokens: n}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	finish := "stop"
	if limited {
		finish = "length"
	}

	includeUsage := s.cfg.StreamUsage == usageAlways ||
		(s.cfg.StreamUsage == usageAuto && req.StreamOptions != nil && req.StreamOptions.IncludeUsage)

	s.log.Info("chat completion",
		"backend", s.cfg.Name,
		"model", req.Model,
		"stream", req.Stream,
		"stream_usage", req.Stream && includeUsage,
		"prompt_tokens", u.PromptTokens,
		"completion_tokens", u.CompletionTokens,
		"authorization_present", r.Header.Get("Authorization") != "",
	)

	id := fmt.Sprintf("chatcmpl-mock-%d", s.cfg.Now().UnixNano())
	created := s.cfg.Now().Unix()

	if !req.Stream {
		sleep(r, s.cfg.TTFT)
		devutil.WriteJSON(w, http.StatusOK, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   req.Model,
			// The backend name also goes in the body, because gateways may drop response headers.
			"system_fingerprint": s.cfg.Name,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": completionText(n)},
				"finish_reason": finish,
			}},
			"usage": u,
		})
		return
	}

	s.stream(w, r, id, created, req.Model, u, finish, includeUsage)
}

// stream writes an OpenAI-style server-sent event stream with one chunk per token.
func (s *server) stream(w http.ResponseWriter, r *http.Request, id string, created int64, model string,
	u usage, finish string, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		devutil.WriteOpenAIError(w, http.StatusInternalServerError, "server_error", "no_flush", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	chunk := func(choices []map[string]any, withUsage bool) bool {
		c := map[string]any{
			"id":                 id,
			"object":             "chat.completion.chunk",
			"created":            created,
			"model":              model,
			"system_fingerprint": s.cfg.Name,
			"choices":            choices,
		}
		if withUsage {
			c["usage"] = u
		} else if includeUsage {
			// OpenAI sends "usage": null on every chunk when include_usage is set.
			c["usage"] = nil
		}
		b, err := json.Marshal(c)
		if err != nil {
			s.log.Error("marshal chunk", "err", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	sleep(r, s.cfg.TTFT)
	if !chunk([]map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}, false) {
		return
	}
	for i := range u.CompletionTokens {
		if i > 0 && !sleep(r, s.cfg.TokenDelay) {
			return
		}
		if !chunk([]map[string]any{{"index": 0, "delta": map[string]any{"content": tokenText(i)}, "finish_reason": nil}}, false) {
			return
		}
	}
	if !chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, false) {
		return
	}
	if includeUsage && !chunk([]map[string]any{}, true) {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// completionTokens returns the number of tokens to generate and whether a limit was applied.
// Precedence: the X-Mock-Completion-Tokens header, max_completion_tokens, max_tokens, the default.
func (s *server) completionTokens(r *http.Request, req chatRequest) (int, bool, error) {
	if h := r.Header.Get("X-Mock-Completion-Tokens"); h != "" {
		n, err := strconv.Atoi(h)
		if err != nil || n < 0 || n > maxCompletionTokens {
			return 0, false, fmt.Errorf("X-Mock-Completion-Tokens must be an integer in 0..%d", maxCompletionTokens)
		}
		return n, false, nil
	}
	limit := req.MaxCompletionTokens
	if limit == 0 {
		limit = req.MaxTokens
	}
	if limit < 0 {
		return 0, false, fmt.Errorf("token limit must not be negative")
	}
	if limit > 0 && limit < s.cfg.DefaultCompletionTokens {
		return limit, true, nil
	}
	return min(s.cfg.DefaultCompletionTokens, maxCompletionTokens), false, nil
}

// promptTokens counts whitespace-separated words in all message contents, so the count is
// deterministic and easy to compute by hand in tests. Content may be a string or an array of
// parts; only text parts are counted.
func promptTokens(msgs []chatMessage) int {
	n := 0
	for _, m := range msgs {
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			n += len(strings.Fields(text))
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if p.Type == "text" {
					n += len(strings.Fields(p.Text))
				}
			}
		}
	}
	return n
}

var words = []string{"lorem", "ipsum", "dolor", "sit", "amet", "consectetur", "adipiscing", "elit"}

// tokenText returns the i-th generated token. Every token after the first starts with a space.
func tokenText(i int) string {
	if i == 0 {
		return words[0]
	}
	return " " + words[i%len(words)]
}

func completionText(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(tokenText(i))
	}
	return b.String()
}

// sleep waits for d or until the request is cancelled. It reports whether the request is still alive.
func sleep(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return r.Context().Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}
