package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
)

func TestRunRejectsBadInput(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:1")
	t.Setenv("AISA_ADDR", "no-port")
	if err := run(nil); err == nil {
		t.Error("want an error for an invalid AISA_ADDR")
	}
	t.Setenv("AISA_ADDR", "")
	t.Setenv("VAULT_ADDR", "")
	if err := run(nil); err == nil {
		t.Error("want an error without VAULT_ADDR")
	}
	if err := run([]string{"-no-such-flag"}); err == nil {
		t.Error("want an error for an unknown flag")
	}
}

func TestRunVersion(t *testing.T) {
	if err := run([]string{"-version"}); err != nil {
		t.Errorf("-version: %v", err)
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probed %s, want /healthz", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	// The probe goes to the address of AISA_ADDR, not to a fixed port.
	t.Setenv("AISA_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	if err := run([]string{"-healthcheck"}); err != nil {
		t.Errorf("a healthy server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := run([]string{"-healthcheck"}); err == nil {
		t.Error("want an error for status 503")
	}
	srv.Close()
	if err := run([]string{"-healthcheck"}); err == nil {
		t.Error("want an error for a server that is gone")
	}
}

func TestHealthURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		":9000":          "http://127.0.0.1:9000/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"127.0.0.1:9000": "http://127.0.0.1:9000/healthz",
		"10.1.2.3:9000":  "http://10.1.2.3:9000/healthz",
		"[::1]:9000":     "http://[::1]:9000/healthz",
		"localhost:9000": "http://localhost:9000/healthz",
	} {
		if got := healthURL(addr); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestObserveLoad(t *testing.T) {
	m := metrics.New("test")
	now := time.Unix(1_700_000_000, 0)
	observeLoad(m, consumers.Stats{Consumers: 3, Keys: 4, Unreadable: 1}, now)
	observeLoad(m, consumers.Stats{Err: errors.New("vault down"), Consumers: 3, Keys: 4, Unreadable: 1}, now.Add(time.Minute))

	for name, tc := range map[string]struct{ got, want float64 }{
		"aisa_consumers":                          {testutil.ToFloat64(m.Consumers), 3},
		"aisa_consumer_keys":                      {testutil.ToFloat64(m.ConsumerKeys), 4},
		"aisa_consumers_unreadable":               {testutil.ToFloat64(m.ConsumersUnreadable), 1},
		"aisa_consumer_loads_total{result=ok}":    {testutil.ToFloat64(m.ConsumerLoads.WithLabelValues(metrics.LoadOK)), 1},
		"aisa_consumer_loads_total{result=error}": {testutil.ToFloat64(m.ConsumerLoads.WithLabelValues(metrics.LoadError)), 1},
		"aisa_consumers_loaded_timestamp_seconds": {testutil.ToFloat64(m.ConsumersLoaded), 1_700_000_000},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", name, tc.got, tc.want)
		}
	}
}
