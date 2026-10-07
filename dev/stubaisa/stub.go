package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/dev/internal/devutil"
)

// Config controls the stub's decisions. It stands in for identities in Vault and budgets in
// Consul; the stub has no quota or budget logic of its own.
type Config struct {
	// Keys maps a plaintext consumer key to a consumer name. Dev-only: real aisa stores hashes.
	Keys map[string]string
	// Rewrites maps consumer → requested model → returned model, to simulate a budget downgrade.
	Rewrites map[string]map[string]string
	// Deny lists consumers that get 429, to simulate an exhausted quota or budget.
	Deny []string
	// Capacity is the number of recorded requests kept for /debug/requests.
	Capacity int
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// Record is one request received by the stub, as returned by /debug/requests.
type Record struct {
	Time    time.Time         `json:"time"`
	Kind    string            `json:"kind"` // "decide" or "usage"
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body,omitempty"`
	// RawBody describes a body that is not valid JSON. The content is not kept, since it cannot be
	// redacted reliably.
	RawBody string `json:"raw_body,omitempty"`
	// Decide only.
	Status      int    `json:"status,omitempty"`
	Consumer    string `json:"consumer,omitempty"`
	Model       string `json:"model,omitempty"`
	ModelSource string `json:"model_source,omitempty"` // "header", "body" or ""
	ReturnModel string `json:"return_model,omitempty"`
	// Usage only: number of events in the body.
	Events int `json:"events,omitempty"`
}

// requestedModelHeader lets a gateway pass the model without forwarding the body.
const requestedModelHeader = "X-Aisa-Requested-Model"

type server struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	records []Record
}

func newServer(cfg Config, log *slog.Logger) *server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1000
	}
	return &server{cfg: cfg, log: log}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		devutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// forward-auth implementations differ in method; accept both.
	mux.HandleFunc("POST /v1/decide", s.decide)
	mux.HandleFunc("GET /v1/decide", s.decide)
	mux.HandleFunc("POST /v1/usage", s.usage)
	mux.HandleFunc("GET /debug/requests", s.listRecords)
	mux.HandleFunc("DELETE /debug/requests", s.clearRecords)
	return mux
}

func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	rec, body, err := s.newRecord(r, "decide")
	if err != nil {
		devutil.WriteOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unreadable_body", err.Error())
		return
	}
	status := s.decideInto(&rec, r, body, w.Header())
	rec.Status = status
	s.store(rec)
	s.log.Info("decide",
		"status", status, "consumer", rec.Consumer, "model", rec.Model, "model_source", rec.ModelSource,
		"return_model", rec.ReturnModel, "forwarded_uri", r.Header.Get("X-Forwarded-Uri"),
		"headers", rec.Headers, "body_bytes", len(body))

	switch status {
	case http.StatusOK:
		w.WriteHeader(http.StatusOK)
	case http.StatusUnauthorized:
		devutil.WriteOpenAIError(w, status, "invalid_request_error", "invalid_api_key", "unknown or invalid consumer key")
	case http.StatusTooManyRequests:
		devutil.WriteOpenAIError(w, status, "insufficient_quota", "quota_exceeded", "quota or budget exhausted for "+rec.Consumer)
	default:
		devutil.WriteOpenAIError(w, status, "invalid_request_error", "missing_model", "the requested model could not be determined")
	}
}

// decideInto fills in rec and the response headers and returns the status code.
func (s *server) decideInto(rec *Record, r *http.Request, body []byte, h http.Header) int {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	consumer, known := s.cfg.Keys[strings.TrimSpace(key)]
	if !ok || !known {
		return http.StatusUnauthorized
	}
	rec.Consumer = consumer

	rec.Model, rec.ModelSource = requestedModel(r, body)
	if rec.Model == "" {
		return http.StatusBadRequest
	}
	if slices.Contains(s.cfg.Deny, consumer) {
		return http.StatusTooManyRequests
	}

	rec.ReturnModel = rec.Model
	if to, ok := s.cfg.Rewrites[consumer][rec.Model]; ok {
		rec.ReturnModel = to
	}
	h.Set("X-Aisa-Consumer", consumer)
	h.Set("X-Aisa-Model", rec.ReturnModel)
	h.Set("X-Aisa-Budget-Remaining", "1000000")
	if id := r.Header.Get("X-Request-Id"); id != "" {
		h.Set("X-Aisa-Request-Id", id)
	}
	return http.StatusOK
}

// requestedModel finds the model name in the requested-model header or in a JSON request body.
func requestedModel(r *http.Request, body []byte) (model, source string) {
	if m := strings.TrimSpace(r.Header.Get(requestedModelHeader)); m != "" {
		return m, "header"
	}
	var b struct {
		Model string `json:"model"`
	}
	if len(body) > 0 && json.Unmarshal(body, &b) == nil && b.Model != "" {
		return b.Model, "body"
	}
	return "", ""
}

func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	rec, body, err := s.newRecord(r, "usage")
	if err != nil {
		devutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	events, err := parseEvents(body)
	if err != nil {
		s.store(rec)
		devutil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	rec.Events = len(events)
	s.store(rec)
	for _, ev := range events {
		s.log.Info("usage event", "event", redactJSON(ev))
	}
	devutil.WriteJSON(w, http.StatusOK, map[string]int{"accepted": len(events)})
}

// parseEvents accepts one JSON object or an array of objects, as http-logger style sinks send
// them, and rejects anything else.
func parseEvents(body []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var events []json.RawMessage
		if err := json.Unmarshal(trimmed, &events); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %w", err)
		}
		for i, ev := range events {
			if !isObject(ev) {
				return nil, fmt.Errorf("event %d is not a JSON object", i)
			}
		}
		return events, nil
	}
	if !isObject(trimmed) {
		return nil, fmt.Errorf("body is not a JSON object or an array of objects")
	}
	return []json.RawMessage{trimmed}, nil
}

func isObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}

// sensitiveKeys are JSON keys whose values are credentials. Gateways' default log formats can
// include the client's request headers, so received bodies are redacted before they are logged
// or kept for /debug/requests.
var sensitiveKeys = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"api-key":             true,
	"api_key":             true,
	"cookie":              true,
}

// redactJSON replaces the values of sensitive keys at any depth. Invalid JSON is returned as is.
func redactJSON(raw []byte) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return raw
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKeys[strings.ToLower(k)] {
				t[k] = "<redacted>"
			} else {
				t[k] = redactValue(val)
			}
		}
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
	}
	return v
}

func (s *server) newRecord(r *http.Request, kind string) (Record, []byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 8<<20))
	if err != nil {
		return Record{}, nil, fmt.Errorf("read body: %w", err)
	}
	rec := Record{
		Time:    s.cfg.Now().UTC(),
		Kind:    kind,
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Headers: flattenHeaders(r.Header),
	}
	if len(body) > 0 {
		if json.Valid(body) {
			rec.Body = redactJSON(body)
		} else {
			rec.RawBody = fmt.Sprintf("<%d bytes, not JSON>", len(body))
		}
	}
	return rec, body, nil
}

// flattenHeaders joins repeated headers and redacts credentials, keeping enough to debug with.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		val := strings.Join(v, ", ")
		switch {
		case strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Proxy-Authorization"):
			// Keep the scheme: whether a client sent Bearer or Basic is useful when debugging.
			scheme, _, _ := strings.Cut(val, " ")
			val = fmt.Sprintf("%s <redacted, %d bytes>", scheme, len(val))
		case sensitiveKeys[strings.ToLower(k)]:
			val = fmt.Sprintf("<redacted, %d bytes>", len(val))
		}
		out[k] = val
	}
	return out
}

func (s *server) store(rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	if over := len(s.records) - s.cfg.Capacity; over > 0 {
		s.records = slices.Delete(s.records, 0, over)
	}
}

func (s *server) listRecords(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	s.mu.Lock()
	out := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		if kind == "" || rec.Kind == kind {
			out = append(out, rec)
		}
	}
	s.mu.Unlock()
	devutil.WriteJSON(w, http.StatusOK, out)
}

func (s *server) clearRecords(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.records = nil
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
