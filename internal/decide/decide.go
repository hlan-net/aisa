// Package decide is the decision API, contract 1 in docs/concepts/ADAPTER_CONTRACT.md: the
// gateway asks POST /v1/decide before it proxies a request, and applies the answer.
//
// This version authenticates the consumer and passes the requested model through. Quotas and
// budgets, which can deny a request or rewrite its model, come in later versions.
package decide

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
)

// Headers of the contract.
const (
	HeaderRequestedModel = "X-Aisa-Requested-Model"
	HeaderConsumer       = "X-Aisa-Consumer"
	HeaderModel          = "X-Aisa-Model"
	HeaderRequestID      = "X-Request-Id"
)

// maxBody bounds the request body aisa reads to find the model. The gateway may forward the
// whole client request, prompt included; the model is usually near the start.
const maxBody = 16 << 20

// Lookup finds the consumer of a key; consumers.Store implements it.
type Lookup interface {
	Lookup(ctx context.Context, key string) (consumers.Consumer, consumers.Result)
}

// Handler answers /v1/decide.
type Handler struct {
	consumers Lookup
	metrics   *metrics.Metrics
	log       *slog.Logger
}

// New returns the decision handler.
func New(c Lookup, m *metrics.Metrics, log *slog.Logger) *Handler {
	return &Handler{consumers: c, metrics: m, log: log}
}

// ServeHTTP answers POST and GET /v1/decide.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(HeaderRequestID)
	log := h.log.With("request_id", reqID)

	key, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		h.deny(w, log, http.StatusUnauthorized, metrics.ConsumerUnknown, metrics.ResultDenyAuth,
			"invalid_request_error", "invalid_api_key", "missing or malformed credential: want Authorization: Bearer <key>")
		return
	}
	consumer, res := h.consumers.Lookup(r.Context(), key)
	switch res {
	case consumers.Unavailable:
		// Fail closed: without current consumers aisa cannot tell a valid key from a revoked one.
		log.Error("cannot decide: consumers are not available")
		writeError(w, http.StatusServiceUnavailable, "server_error", "consumers_unavailable",
			"aisa cannot verify credentials at the moment")
		return
	case consumers.Unknown:
		h.deny(w, log, http.StatusUnauthorized, metrics.ConsumerUnknown, metrics.ResultDenyAuth,
			"invalid_request_error", "invalid_api_key", "unknown or invalid consumer key")
		return
	}

	model := requestedModel(r)
	if model == "" {
		h.deny(w, log, http.StatusBadRequest, consumer.Name, metrics.ResultInvalid,
			"invalid_request_error", "missing_model", "the requested model could not be determined")
		return
	}

	w.Header().Set(HeaderConsumer, consumer.Name)
	w.Header().Set(HeaderModel, model)
	w.WriteHeader(http.StatusOK)
	h.metrics.Decisions.WithLabelValues(consumer.Name, metrics.ResultAllow).Inc()
	log.Debug("allow", "consumer", consumer.Name, "model", model)
}

func (h *Handler) deny(w http.ResponseWriter, log *slog.Logger, status int, consumer, result, typ, code, msg string) {
	h.metrics.Decisions.WithLabelValues(consumer, result).Inc()
	log.Info("deny", "status", status, "consumer", consumer, "result", result, "reason", code)
	writeError(w, status, typ, code, msg)
}

// bearer extracts the key from an Authorization header. The scheme is case-insensitive.
func bearer(v string) (string, bool) {
	scheme, key, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	key = strings.TrimSpace(key)
	return key, key != ""
}

// requestedModel reads the model from X-Aisa-Requested-Model or, when absent, from the JSON
// request body. The header takes precedence: the adapter sets it from the body and overwrites
// any client value (adapter rule 2).
func requestedModel(r *http.Request) string {
	if m := strings.TrimSpace(r.Header.Get(HeaderRequestedModel)); m != "" {
		return m
	}
	if r.Body == nil {
		return ""
	}
	var body struct {
		Model string `json:"model"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(&body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Model)
}

// writeError writes an OpenAI-style error body, which the gateway passes to the client.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}
