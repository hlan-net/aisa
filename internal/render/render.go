// Package render receives the proxy's reports of its config renders at POST /v1/render, so
// that a rejected render or an empty catalog shows in aisa's metrics and alerts and not only in
// the proxy's log (contract 3, PROXY_CONTRACT.md, Reporting a render).
package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hlan-net/aisa/internal/metrics"
)

// maxBody bounds a report (64 KiB): it holds a few numbers and a reason.
const maxBody = 64 << 10

// maxReason bounds the reason, which is logged.
const maxReason = 1024

// Report is what a proxy sends after each render of the gateway's config.
type Report struct {
	// Timestamp is when the render ended, ISO 8601; now when missing.
	Timestamp string `json:"ts"`
	// Result is promoted, rejected or held.
	Result string `json:"result"`
	// Reason says why a render was rejected or held, for the log.
	Reason string `json:"reason"`
	// Backends and Models are what the rendered config holds: the backends among its routes
	// and the models with a route.
	Backends int64 `json:"backends"`
	Models   int64 `json:"models"`
}

// Handler receives the reports.
type Handler struct {
	metrics *metrics.Metrics
	log     *slog.Logger
	now     func() time.Time
}

// New returns a handler for POST /v1/render.
func New(m *metrics.Metrics, log *slog.Logger) *Handler {
	return &Handler{metrics: m, log: log, now: time.Now}
}

// ServeHTTP answers POST /v1/render: 204 for a report it took, 400 for one it could not.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed: want POST"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var rep Report
	if err := json.Unmarshal(body, &rep); err != nil {
		h.log.Warn("render report rejected: malformed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON object"})
		return
	}
	if err := validate(rep); err != nil {
		h.log.Warn("render report rejected", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	h.record(rep)
	w.WriteHeader(http.StatusNoContent)
}

func validate(rep Report) error {
	switch rep.Result {
	case metrics.RenderPromoted, metrics.RenderRejected, metrics.RenderHeld:
	default:
		return fmt.Errorf("invalid result: want %s, %s or %s",
			metrics.RenderPromoted, metrics.RenderRejected, metrics.RenderHeld)
	}
	if rep.Backends < 0 || rep.Models < 0 {
		return errors.New("backends and models cannot be negative")
	}
	if len(rep.Reason) > maxReason {
		return fmt.Errorf("reason exceeds %d bytes", maxReason)
	}
	return nil
}

// record puts a valid report into the metrics and the log. The gauges describe the config
// the gateway runs, so only a promoted render sets them, and they stay absent until one.
func (h *Handler) record(rep Report) {
	h.metrics.ConfigRenders.WithLabelValues(rep.Result).Inc()
	at := h.when(rep.Timestamp)
	switch rep.Result {
	case metrics.RenderPromoted:
		h.metrics.ConfigRendered.WithLabelValues().Set(float64(at.Unix()))
		h.metrics.ConfigBackends.WithLabelValues().Set(float64(rep.Backends))
		h.metrics.ConfigModels.WithLabelValues().Set(float64(rep.Models))
		h.log.Info("config render promoted", "backends", rep.Backends, "models", rep.Models, "at", at)
	default:
		h.log.Warn("config render not promoted",
			"result", rep.Result, "reason", rep.Reason, "backends", rep.Backends, "models", rep.Models, "at", at)
	}
}

// when is the report's time, or now when it is missing, unreadable or in the future.
func (h *Handler) when(ts string) time.Time {
	now := h.now()
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(ts)); err == nil && !t.After(now) {
		return t
	}
	return now
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
