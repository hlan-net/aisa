package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/quotas"
)

// maxBody bounds the request body aisa reads for usage events (16 MiB).
const maxBody = 16 << 20

// maxLabel bounds the length of string labels (request_id, consumer, model, backend) to prevent oversized metric labels.
const maxLabel = 256

// Charger counts the tokens of a consumer towards its quota; quotas.Quota implements it. It
// logs and counts its own errors.
type Charger interface {
	Charge(ctx context.Context, consumer string, tokens int64, at time.Time) error
}

// Handler ingests usage events from gateways at POST /v1/usage.
type Handler struct {
	metrics *metrics.Metrics
	dedup   *Dedup
	charger Charger
	log     *slog.Logger
	now     func() time.Time
}

// New returns a usage handler. With a nil charger, tokens are not counted towards quotas.
func New(m *metrics.Metrics, dedup *Dedup, charger Charger, log *slog.Logger) *Handler {
	return &Handler{
		metrics: m,
		dedup:   dedup,
		charger: charger,
		log:     log,
		now:     time.Now,
	}
}

// chargeTimeout bounds the charges of one batch together, so a Redis that is down cannot keep
// a request busy for one timeout per consumer and minute.
const chargeTimeout = 5 * time.Second

// charge is the tokens of one consumer in one minute of a batch, charged together.
type charge struct {
	consumer string
	minute   time.Time
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
			h.log.Warn("usage request rejected: body too large", "limit_bytes", maxBody)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		h.log.Warn("usage request rejected: unreadable body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	events, err := ParseEvents(body)
	if err != nil {
		h.log.Warn("usage request rejected: malformed events", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	accepted, rejected := 0, 0
	charges := map[charge]int64{}
	for _, ev := range events {
		if err := validateEvent(ev); err != nil {
			h.metrics.UsageEvents.WithLabelValues(metrics.EventRejected).Inc()
			h.log.Warn("invalid usage event skipped", "error", err, "request_id", ev.RequestID)
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
		if tokens := quotas.AddTokens(int64(ev.PromptTokens), int64(ev.CompletionTokens)); tokens > 0 && ev.Consumer != "" {
			key := charge{ev.Consumer, h.eventTime(ev).UTC().Truncate(time.Minute)}
			charges[key] = quotas.AddTokens(charges[key], tokens)
		}
		accepted++
	}
	h.chargeAll(r.Context(), charges)

	writeJSON(w, http.StatusOK, map[string]int{"accepted": accepted, "rejected": rejected})
}

// eventTime is when the gateway logged the event, or now when its ts is missing or unreadable.
func (h *Handler) eventTime(ev Event) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(ev.Timestamp)); err == nil {
		return t
	}
	return h.now()
}

// chargeAll counts the tokens of a batch towards the quotas. It is not cancelled when the log
// sink goes away: the events are accepted, so their tokens count.
func (h *Handler) chargeAll(ctx context.Context, charges map[charge]int64) {
	if h.charger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chargeTimeout)
	defer cancel()
	done := 0
	for c, tokens := range charges {
		if ctx.Err() != nil {
			h.log.Warn("quota charges of a batch timed out; the rest are not counted",
				"charged", done, "skipped", len(charges)-done)
			return
		}
		// The charger logs and counts its errors; the events stay accepted, since a retry
		// would be dropped as a duplicate anyway.
		_ = h.charger.Charge(ctx, c.consumer, tokens, c.minute)
		done++
	}
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
