package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/hlan-net/aisa/internal/metrics"
)

// maxBody bounds the request body aisa reads for usage events (16 MiB).
const maxBody = 16 << 20

// maxLabel bounds the length of string labels (request_id, consumer, model, backend) to prevent oversized metric labels.
const maxLabel = 256

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
		// Not counted: a log sink always POSTs, so this is a stray request (a probe, a scanner),
		// not usage that is being lost.
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed: want POST"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			h.log.Warn("usage request rejected: body too large", "limit_bytes", maxBody)
			h.rejectRequest(w, http.StatusRequestEntityTooLarge, metrics.UsageRequestTooLarge, "request body too large")
			return
		}
		h.log.Warn("usage request rejected: unreadable body", "error", err)
		h.rejectRequest(w, http.StatusBadRequest, metrics.UsageRequestUnreadable, err.Error())
		return
	}

	events, err := ParseEvents(body)
	if err != nil {
		h.log.Warn("usage request rejected: malformed events", "error", err)
		h.rejectRequest(w, http.StatusBadRequest, metrics.UsageRequestMalformed, err.Error())
		return
	}

	accepted, rejected := 0, 0
	var firstErr error
	firstIndex := -1
	for i, p := range events {
		ev := p.Event
		err := p.Err
		if err == nil {
			err = validateEvent(ev)
		}
		if err != nil {
			h.metrics.UsageEvents.WithLabelValues(metrics.EventRejected).Inc()
			if firstErr == nil {
				firstErr, firstIndex = err, i
			}
			rejected++
			continue
		}
		if h.dedup.SeenOrAdd(ev.RequestID) {
			h.metrics.UsageEvents.WithLabelValues(metrics.EventDuplicate).Inc()
			h.log.Debug("duplicate usage event skipped", "request_id", ev.RequestID)
			accepted++
			continue
		}

		h.metrics.UsageEvents.WithLabelValues(metrics.EventAccepted).Inc()
		h.recordEvent(ev)
		accepted++
	}

	if rejected > 0 {
		// One line per request, not per event: a sink with a systematic mistake sends every event
		// of every batch wrong, up to 10 000 per request.
		h.log.Warn("invalid usage events skipped",
			"rejected", rejected, "events", len(events), "first_index", firstIndex, "first_error", firstErr)
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": accepted, "rejected": rejected})
}

// rejectRequest answers a usage request that is rejected as a whole, and counts it: none of its
// events reach aisa_usage_events_total.
func (h *Handler) rejectRequest(w http.ResponseWriter, status int, reason, msg string) {
	h.metrics.UsageRequestsRejected.WithLabelValues(reason).Inc()
	writeJSON(w, status, map[string]string{"error": msg})
}

// validateEvent verifies that required fields and numeric bounds conform to the usage contract.
func validateEvent(ev Event) error {
	if strings.TrimSpace(ev.RequestID) == "" {
		return errors.New("missing request_id")
	}
	if len(ev.RequestID) > maxLabel {
		return fmt.Errorf("request_id exceeds %d bytes", maxLabel)
	}
	if ev.Status < 100 || ev.Status > 599 {
		return fmt.Errorf("invalid status %d: must be an HTTP status code (100-599)", ev.Status)
	}
	if ev.PromptTokens < 0 {
		return errors.New("prompt_tokens cannot be negative")
	}
	if ev.CompletionTokens < 0 {
		return errors.New("completion_tokens cannot be negative")
	}
	if ev.LatencyMS < 0 {
		return errors.New("latency_ms cannot be negative")
	}
	if ev.TTFTMS < 0 {
		return errors.New("ttft_ms cannot be negative")
	}
	if len(ev.Consumer) > maxLabel {
		return fmt.Errorf("consumer exceeds %d bytes", maxLabel)
	}
	if len(ev.Model) > maxLabel {
		return fmt.Errorf("model exceeds %d bytes", maxLabel)
	}
	if len(ev.Backend) > maxLabel {
		return fmt.Errorf("backend exceeds %d bytes", maxLabel)
	}
	return nil
}

// recordEvent updates the metrics for a valid, non-duplicate event.
func (h *Handler) recordEvent(ev Event) {
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
