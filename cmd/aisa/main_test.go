package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunRejectsBadInput(t *testing.T) {
	t.Setenv("AISA_ADDR", "no-port")
	if err := run(nil); err == nil {
		t.Error("want an error for an invalid AISA_ADDR")
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
