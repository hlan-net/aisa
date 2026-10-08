// Package server is aisa's HTTP server: health endpoints and metrics. The decision API and the
// usage ingestion register their handlers here.
//
// It listens on one address, or on two: the API (the handlers registered with Handle) on one,
// and the operations endpoints (/metrics) on the other. /healthz and /readyz answer on both. Two
// addresses let a network policy give the proxy the API and a scraper the metrics only (#46).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Check is a dependency that aisa needs before it can serve, such as Vault or Redis.
type Check struct {
	// Name identifies the dependency in the answer of /readyz and in logs.
	Name string
	// Probe returns nil when the dependency can be used.
	Probe func(ctx context.Context) error
}

// probeTimeout is the time all checks get together: they run at the same time. /readyz is asked
// every few seconds, and an answer must not take longer than the probe waits for it.
const probeTimeout = 2 * time.Second

// Server is aisa's HTTP server.
type Server struct {
	log             *slog.Logger
	api             *http.ServeMux // the probes and the handlers registered with Handle
	ops             *http.ServeMux // the probes and /metrics
	all             *http.ServeMux // both, for a single address
	checks          []Check
	shutdownTimeout time.Duration
}

// New returns a server with /healthz, /readyz and /metrics. metrics serves the last one.
func New(log *slog.Logger, metrics http.Handler, shutdownTimeout time.Duration, checks ...Check) *Server {
	s := &Server{
		log: log, api: http.NewServeMux(), ops: http.NewServeMux(), all: http.NewServeMux(),
		checks: checks, shutdownTimeout: shutdownTimeout,
	}
	for _, mux := range []*http.ServeMux{s.api, s.ops} {
		mux.HandleFunc("GET /healthz", s.healthz)
		mux.HandleFunc("GET /readyz", s.readyz)
	}
	s.ops.Handle("GET /metrics", metrics)
	s.all.Handle("GET /metrics", metrics)
	s.all.Handle("/", s.api)
	return s
}

// Handle registers a handler of the API, with a pattern of http.ServeMux such as
// "POST /v1/decide".
func (s *Server) Handle(pattern string, h http.Handler) { s.api.Handle(pattern, h) }

// Handler returns all routes, as served on a single address.
func (s *Server) Handler() http.Handler { return s.all }

// APIHandler and OpsHandler return the routes of each address when there are two.
func (s *Server) APIHandler() http.Handler { return s.api }

// OpsHandler returns the routes of the operations address: the probes and /metrics.
func (s *Server) OpsHandler() http.Handler { return s.ops }

// healthz answers while the process runs. Kubernetes restarts the pod when it stops answering.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz answers 200 when every dependency can be used, else 503. The answer names the
// dependencies that failed; why they failed is in the log only.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	errs := make([]error, len(s.checks))
	var wg sync.WaitGroup
	for i, c := range s.checks {
		wg.Go(func() { errs[i] = probeWithin(ctx, c) })
	}
	wg.Wait()

	status := http.StatusOK
	checks := make(map[string]string, len(s.checks))
	for i, c := range s.checks {
		if errs[i] != nil {
			s.log.Warn("not ready", "check", c.Name, "error", errs[i])
			checks[c.Name] = "failed"
			status = http.StatusServiceUnavailable
			continue
		}
		checks[c.Name] = "ok"
	}
	answer := "ready"
	if status != http.StatusOK {
		answer = "not ready"
	}
	writeJSON(w, status, map[string]any{"status": answer, "checks": checks})
}

// probeWithin runs a check and gives up when ctx is done, also when the check ignores it.
func probeWithin(ctx context.Context, c Check) error {
	done := make(chan error, 1)
	go func() { done <- c.Probe(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("no answer in time: %w", ctx.Err())
	}
}

// Run listens on addr and serves until ctx is done, then lets the requests in flight finish.
// With an opsAddr, the API is served on addr and /metrics on opsAddr; without one, all on addr.
func (s *Server) Run(ctx context.Context, addr, opsAddr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	if opsAddr == "" {
		return s.Serve(ctx, ln)
	}
	opsLn, err := net.Listen("tcp", opsAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("listen on %s: %w", opsAddr, err)
	}
	return s.ServeSplit(ctx, ln, opsLn)
}

// Serve is Run on a listener that is already open, with all routes on it.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, listener{ln, s.all, "all"})
}

// ServeSplit is Run on two listeners that are already open: the API on ln, the operations
// endpoints on opsLn.
func (s *Server) ServeSplit(ctx context.Context, ln, opsLn net.Listener) error {
	return s.serve(ctx, listener{ln, s.api, "api"}, listener{opsLn, s.ops, "ops"})
}

type listener struct {
	ln      net.Listener
	handler http.Handler
	serves  string
}

// serve serves each listener until ctx is done or one of them fails, then shuts all down and
// lets the requests in flight finish.
func (s *Server) serve(ctx context.Context, lns ...listener) error {
	srvs := make([]*http.Server, len(lns))
	errc := make(chan error, len(lns))
	for i, l := range lns {
		srvs[i] = &http.Server{
			Handler:           l.handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			s.log.Info("listening", "addr", l.ln.Addr().String(), "serves", l.serves)
			if err := srvs[i].Serve(l.ln); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve on %s: %w", l.ln.Addr(), err)
				return
			}
			errc <- nil
		}()
	}

	var failed error
	select {
	case failed = <-errc:
	case <-ctx.Done():
	}

	s.log.Info("shutting down")
	// The parent context may be done already; the requests in flight get their own time.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()
	for _, srv := range srvs {
		if err := srv.Shutdown(shutdownCtx); err != nil && failed == nil {
			failed = fmt.Errorf("shutdown: %w", err)
		}
	}
	return failed
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
