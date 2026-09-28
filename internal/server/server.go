// Package server is aisa's HTTP server: health endpoints and metrics. The decision API and the
// usage ingestion register their handlers here.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Check is a dependency that aisa needs before it can serve, such as Vault or Redis.
type Check struct {
	// Name identifies the dependency in the answer of /readyz and in logs.
	Name string
	// Probe returns nil when the dependency can be used.
	Probe func(ctx context.Context) error
}

// probeTimeout is the time one check gets; /readyz is asked every few seconds.
const probeTimeout = 2 * time.Second

// Server is aisa's HTTP server.
type Server struct {
	log             *slog.Logger
	mux             *http.ServeMux
	checks          []Check
	shutdownTimeout time.Duration
}

// New returns a server with /healthz, /readyz and /metrics. metrics serves the last one.
func New(log *slog.Logger, metrics http.Handler, shutdownTimeout time.Duration, checks ...Check) *Server {
	s := &Server{log: log, mux: http.NewServeMux(), checks: checks, shutdownTimeout: shutdownTimeout}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.Handle("GET /metrics", metrics)
	return s
}

// Handle registers a handler, with a pattern of http.ServeMux such as "POST /v1/decide".
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Handler returns the server's routes, for tests.
func (s *Server) Handler() http.Handler { return s.mux }

// healthz answers while the process runs. Kubernetes restarts the pod when it stops answering.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz answers 200 when every dependency can be used, else 503. The answer names the
// dependencies that failed; why they failed is in the log only.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	checks := make(map[string]string, len(s.checks))
	for _, c := range s.checks {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		err := c.Probe(ctx)
		cancel()
		if err != nil {
			s.log.Warn("not ready", "check", c.Name, "error", err)
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

// Run listens on addr and serves until ctx is done, then lets the requests in flight finish.
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on a listener that is already open.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", ln.Addr().String())
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down")
	// The parent context is done already; the requests in flight get their own time.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
