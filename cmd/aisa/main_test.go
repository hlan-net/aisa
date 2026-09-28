package main

import (
	"net/http"
	"net/http/httptest"
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

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	if err := run([]string{"-healthcheck", srv.URL + "/healthz"}); err != nil {
		t.Errorf("a healthy server: %v", err)
	}
	if err := run([]string{"-healthcheck", srv.URL + "/readyz"}); err == nil {
		t.Error("want an error for status 503")
	}
	srv.Close()
	if err := run([]string{"-healthcheck", srv.URL + "/healthz"}); err == nil {
		t.Error("want an error for a server that is gone")
	}
}
