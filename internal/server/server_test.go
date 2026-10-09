package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newTestServer(checks ...Check) *Server {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "aisa_build_info 1\n")
	})
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), metrics, time.Second, checks...)
}

func get(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	rec := get(t, newTestServer(), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestHealthzIgnoresChecks(t *testing.T) {
	down := Check{Name: "vault", Probe: func(context.Context) error { return errors.New("sealed") }}
	if rec := get(t, newTestServer(down), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: a failed dependency must not restart the pod", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	ok := Check{Name: "redis", Probe: func(context.Context) error { return nil }}
	down := Check{Name: "vault", Probe: func(context.Context) error { return errors.New("sealed at 10.0.0.1") }}

	type answer struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	for name, tc := range map[string]struct {
		checks []Check
		status int
		want   answer
	}{
		"no dependencies": {nil, http.StatusOK, answer{"ready", map[string]string{}}},
		"all ready":       {[]Check{ok}, http.StatusOK, answer{"ready", map[string]string{"redis": "ok"}}},
		"one failed": {[]Check{ok, down}, http.StatusServiceUnavailable,
			answer{"not ready", map[string]string{"redis": "ok", "vault": "failed"}}},
	} {
		rec := get(t, newTestServer(tc.checks...), http.MethodGet, "/readyz")
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, tc.status)
		}
		var got answer
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Status != tc.want.Status || len(got.Checks) != len(tc.want.Checks) {
			t.Errorf("%s: answer = %+v, want %+v", name, got, tc.want)
		}
		for k, v := range tc.want.Checks {
			if got.Checks[k] != v {
				t.Errorf("%s: check %s = %q, want %q", name, k, got.Checks[k], v)
			}
		}
	}
}

func TestReadyzGivesAProbeLimitedTime(t *testing.T) {
	slow := Check{Name: "consul", Probe: func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("no deadline")
		}
		return nil
	}}
	if rec := get(t, newTestServer(slow), http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("status = %d: the probe's context has no deadline", rec.Code)
	}
}

func TestReadyzRunsTheChecksAtTheSameTime(t *testing.T) {
	// Each check waits for the other one to have started: run one after the other, the first
	// would wait until its time is up.
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	check := func(name string) Check {
		return Check{Name: name, Probe: func(ctx context.Context) error {
			started.Done()
			select {
			case <-both:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	}
	if rec := get(t, newTestServer(check("vault"), check("consul")), http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the checks did not run at the same time", rec.Code)
	}
}

func TestReadyzDoesNotWaitForACheckThatHangs(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	hangs := Check{Name: "redis", Probe: func(context.Context) error {
		<-release // ignores its context
		return nil
	}}
	began := time.Now()
	rec := get(t, newTestServer(hangs), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if took := time.Since(began); took > probeTimeout+time.Second {
		t.Errorf("/readyz took %v, want about %v", took, probeTimeout)
	}
}

func TestMetricsAndMethods(t *testing.T) {
	s := newTestServer()
	if rec := get(t, s, http.MethodGet, "/metrics"); rec.Code != http.StatusOK || rec.Body.String() == "" {
		t.Errorf("GET /metrics: status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec := get(t, s, http.MethodPost, "/healthz"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: status = %d, want 405", rec.Code)
	}
	if rec := get(t, s, http.MethodGet, "/nothing"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nothing: status = %d, want 404", rec.Code)
	}
}

func TestHandle(t *testing.T) {
	s := newTestServer()
	s.Handle("POST /v1/decide", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if rec := get(t, s, http.MethodPost, "/v1/decide"); rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	s := newTestServer()
	started := make(chan struct{})
	release := make(chan struct{})
	s.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	status := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			status <- 0
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()

	<-started
	cancel() // SIGTERM while a request is in flight
	close(release)

	if got := <-status; got != http.StatusOK {
		t.Errorf("the request in flight got status %d, want 200", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was done")
	}
}

func TestRunReportsAnAddressInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if err := newTestServer().Run(context.Background(), ln.Addr().String(), ""); err == nil {
		t.Error("want an error for an address that is in use")
	}
	// The operations address in use: the API's listener is closed again.
	if err := newTestServer().Run(context.Background(), "127.0.0.1:0", ln.Addr().String()); err == nil {
		t.Error("want an error for an operations address that is in use")
	}
}

// With two addresses, the API's address does not serve /metrics and the operations address
// serves nothing of the API; the probes answer on both (#46).
func TestSplitRoutes(t *testing.T) {
	s := newTestServer()
	s.Handle("POST /v1/usage", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name    string
		handler http.Handler
		method  string
		path    string
		want    int
	}{
		{"api: usage", s.APIHandler(), http.MethodPost, "/v1/usage", http.StatusNoContent},
		{"api: no metrics", s.APIHandler(), http.MethodGet, "/metrics", http.StatusNotFound},
		{"api: healthz", s.APIHandler(), http.MethodGet, "/healthz", http.StatusOK},
		{"api: readyz", s.APIHandler(), http.MethodGet, "/readyz", http.StatusOK},
		{"ops: metrics", s.OpsHandler(), http.MethodGet, "/metrics", http.StatusOK},
		{"ops: no usage", s.OpsHandler(), http.MethodPost, "/v1/usage", http.StatusNotFound},
		{"ops: healthz", s.OpsHandler(), http.MethodGet, "/healthz", http.StatusOK},
		{"ops: readyz", s.OpsHandler(), http.MethodGet, "/readyz", http.StatusOK},
		{"one address: usage", s.Handler(), http.MethodPost, "/v1/usage", http.StatusNoContent},
		{"one address: metrics", s.Handler(), http.MethodGet, "/metrics", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		tc.handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s: %s %s = %d, want %d", tc.name, tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

func TestServeSplit(t *testing.T) {
	s := newTestServer()
	s.Handle("POST /v1/usage", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeSplit(ctx, ln, opsLn) }()

	status := func(method, addr, path string) int {
		req, _ := http.NewRequest(method, "http://"+addr+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(http.MethodPost, ln.Addr().String(), "/v1/usage"); got != http.StatusNoContent {
		t.Errorf("usage on the API's address: %d, want 204", got)
	}
	if got := status(http.MethodGet, ln.Addr().String(), "/metrics"); got != http.StatusNotFound {
		t.Errorf("metrics on the API's address: %d, want 404", got)
	}
	if got := status(http.MethodGet, opsLn.Addr().String(), "/metrics"); got != http.StatusOK {
		t.Errorf("metrics on the operations address: %d, want 200", got)
	}
	if got := status(http.MethodPost, opsLn.Addr().String(), "/v1/usage"); got != http.StatusNotFound {
		t.Errorf("usage on the operations address: %d, want 404", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeSplit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeSplit did not return after the context was done")
	}
}
