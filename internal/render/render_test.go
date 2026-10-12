package render

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/metrics"
)

func TestHandler(t *testing.T) {
	m := metrics.New("test")
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	post := func(body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/render", strings.NewReader(body)))
		return rec.Code
	}
	renders := func(result string) float64 {
		return testutil.ToFloat64(m.ConfigRenders.WithLabelValues(result))
	}

	// Before the first report the gauges are absent, and every result series starts at zero.
	for _, name := range []string{"aisa_config_rendered_timestamp_seconds", "aisa_config_backends", "aisa_config_models"} {
		if n, err := testutil.GatherAndCount(m.Gatherer(), name); err != nil || n != 0 {
			t.Errorf("%s before the first report: %d series, %v; want none", name, n, err)
		}
	}
	for _, result := range []string{metrics.RenderPromoted, metrics.RenderRejected, metrics.RenderHeld} {
		if got := renders(result); got != 0 {
			t.Errorf("aisa_config_renders_total{%s} = %v before any report, want 0 and present", result, got)
		}
	}
	if n, _ := testutil.GatherAndCount(m.Gatherer(), "aisa_config_renders_total"); n != 3 {
		t.Errorf("aisa_config_renders_total has %d series before any report, want 3", n)
	}

	if got := post(`{"ts": "2026-10-12T11:59:00Z", "result": "promoted", "backends": 2, "models": 3}`); got != http.StatusNoContent {
		t.Fatalf("promoted: status = %d, want 204", got)
	}
	if got := renders(metrics.RenderPromoted); got != 1 {
		t.Errorf("aisa_config_renders_total{promoted} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ConfigRendered.WithLabelValues()); got != float64(now.Add(-time.Minute).Unix()) {
		t.Errorf("aisa_config_rendered_timestamp_seconds = %v, want the report's time", got)
	}
	if got := testutil.ToFloat64(m.ConfigBackends.WithLabelValues()); got != 2 {
		t.Errorf("aisa_config_backends = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ConfigModels.WithLabelValues()); got != 3 {
		t.Errorf("aisa_config_models = %v, want 3", got)
	}

	// A rejected render and a hold count, and leave the gauges of the running config alone.
	if got := post(`{"result": "rejected", "reason": "APISIX did not load these routes: model-x", "backends": 1, "models": 1}`); got != http.StatusNoContent {
		t.Fatalf("rejected: status = %d, want 204", got)
	}
	if got := post(`{"ts": "not a time", "result": "held", "reason": "no model route", "backends": 0, "models": 0}`); got != http.StatusNoContent {
		t.Fatalf("held: status = %d, want 204", got)
	}
	if got := renders(metrics.RenderRejected); got != 1 {
		t.Errorf("aisa_config_renders_total{rejected} = %v, want 1", got)
	}
	if got := renders(metrics.RenderHeld); got != 1 {
		t.Errorf("aisa_config_renders_total{held} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ConfigModels.WithLabelValues()); got != 3 {
		t.Errorf("aisa_config_models = %v after a rejected render, want 3 still", got)
	}

	// A promoted render in the future is dated now; one without counts reports an empty config.
	if got := post(`{"ts": "2030-01-01T00:00:00Z", "result": "promoted"}`); got != http.StatusNoContent {
		t.Fatalf("future: status = %d, want 204", got)
	}
	if got := testutil.ToFloat64(m.ConfigRendered.WithLabelValues()); got != float64(now.Unix()) {
		t.Errorf("aisa_config_rendered_timestamp_seconds = %v for a future ts, want now", got)
	}
	if got := testutil.ToFloat64(m.ConfigModels.WithLabelValues()); got != 0 {
		t.Errorf("aisa_config_models = %v, want 0", got)
	}

	// A report sent again with the same id counts once; reports without an id all count.
	if got := post(`{"id": "0000000007-abc", "result": "rejected", "reason": "x"}`); got != http.StatusNoContent {
		t.Fatalf("with an id: status = %d, want 204", got)
	}
	if got := post(`{"id": "0000000007-abc", "result": "rejected", "reason": "x"}`); got != http.StatusNoContent {
		t.Fatalf("sent again: status = %d, want 204", got)
	}
	if got := renders(metrics.RenderRejected); got != 2 {
		t.Errorf("aisa_config_renders_total{rejected} = %v after a report sent twice, want 2", got)
	}
	post(`{"result": "rejected", "reason": "no id"}`)
	post(`{"result": "rejected", "reason": "no id"}`)
	if got := renders(metrics.RenderRejected); got != 4 {
		t.Errorf("aisa_config_renders_total{rejected} = %v after two reports without an id, want 4", got)
	}

	for name, body := range map[string]string{
		"unknown result":    `{"result": "maybe"}`,
		"no result":         `{"backends": 1}`,
		"negative backends": `{"result": "promoted", "backends": -1}`,
		"not JSON":          `promoted`,
		"an array":          `[{"result": "promoted"}]`,
		"a long reason":     `{"result": "rejected", "reason": "` + strings.Repeat("x", maxReason+1) + `"}`,
		"a long id":         `{"result": "rejected", "id": "` + strings.Repeat("x", maxID+1) + `"}`,
	} {
		if got := post(body); got != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, got)
		}
	}
	if got := renders(metrics.RenderPromoted); got != 2 {
		t.Errorf("aisa_config_renders_total{promoted} = %v after the rejected reports, want 2", got)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/render", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET: status = %d, Allow = %q; want 405 and POST", rec.Code, rec.Header().Get("Allow"))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/render", strings.NewReader(`{"reason": "`+strings.Repeat("x", maxBody)+`"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized report: status = %d, want 413", rec.Code)
	}
}
