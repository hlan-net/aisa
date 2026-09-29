package ledger

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hlan-net/aisa/internal/metrics"
)

// maxBody bounds the request body aisa reads for usage events (16 MiB).
const maxBody = 16 << 20

// Handler ingests usage events from gateways at POST /v1/usage.
type Handler struct {
	metrics *metrics.Metrics
	dedup   *Dedup
	log     *slog.Logger
}

// New returns a usage handler.
func New(m *metrics.Metrics, dedup *Dedup, log *slog.Logger) *Handler {
	return &Handler{
		metrics: m,
		dedup:   dedup,
		log:     log,
	}
}

// ServeHTTP answers POST /v1/usage.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed: want POST"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	events, err := ParseEvents(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	for _, ev := range events {
		h.recordEvent(ev)
	}

	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(events)})
}

// recordEvent deduplicates by request id and updates the metrics.
func (h *Handler) recordEvent(ev Event) {
	if ev.RequestID != "" && h.dedup.SeenOrAdd(ev.RequestID) {
		h.log.Debug("duplicate usage event skipped", "request_id", ev.RequestID)
		return
	}

	consumer := ev.Consumer
	if consumer == "" {
		consumer = metrics.ConsumerUnknown
	}
	statusStr := strconv.Itoa(int(ev.Status))

	h.metrics.Requests.WithLabelValues(consumer, ev.Model, ev.Backend, statusStr).Inc()

	if ev.PromptTokens > 0 {
		h.metrics.Tokens.WithLabelValues(consumer, ev.Model, ev.Backend, metrics.DirectionPrompt).Add(float64(ev.PromptTokens))
	}
	if ev.CompletionTokens > 0 {
		h.metrics.Tokens.WithLabelValues(consumer, ev.Model, ev.Backend, metrics.DirectionCompletion).Add(float64(ev.CompletionTokens))
	}

	if ev.LatencyMS > 0 {
		h.metrics.Latency.WithLabelValues(ev.Model, ev.Backend).Observe(float64(ev.LatencyMS) / 1000.0)
	}
	if ev.TTFTMS > 0 {
		h.metrics.TTFT.WithLabelValues(ev.Model, ev.Backend).Observe(float64(ev.TTFTMS) / 1000.0)
	}

	h.log.Debug("usage event recorded",
		"consumer", consumer,
		"model", ev.Model,
		"backend", ev.Backend,
		"status", ev.Status,
		"prompt_tokens", ev.PromptTokens,
		"completion_tokens", ev.CompletionTokens,
		"request_id", ev.RequestID,
	)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
