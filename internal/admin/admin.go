// Package admin provides an HTTP administration API and Web UI for aisa, allowing the
// holder to inspect and manage consumers, quota profiles, backends, and models in Consul and Vault.
package admin

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hlan-net/aisa/internal/admin/ui"
	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/server"
)

// ConsulClient is the subset of Consul operations needed by the admin interface.
type ConsulClient interface {
	List(ctx context.Context, prefix string, index uint64, wait time.Duration) ([]consul.Pair, uint64, error)
	Put(ctx context.Context, key string, value []byte) error
	PutCAS(ctx context.Context, key string, value []byte, cas uint64) error
	Delete(ctx context.Context, key string, recurse bool) error
	Health(ctx context.Context, service string) ([]consul.Instance, error)
}

// VaultClient is the subset of Vault operations needed by the admin interface.
type VaultClient interface {
	List(ctx context.Context, mount, path string) ([]string, error)
	Read(ctx context.Context, mount, path string) (map[string]any, error)
	ReadVersion(ctx context.Context, mount, path string) (map[string]any, int, error)
	Write(ctx context.Context, mount, path string, data map[string]any, cas int) error
	Delete(ctx context.Context, mount, path string) error
	Probe(ctx context.Context) error
}

// Options configure the admin handler.
type Options struct {
	ConsulPrefix string // Prefix in Consul KV, defaults to "aisa"
	VaultMount   string // KV v2 mount in Vault, defaults to "secret"
	VaultPrefix  string // Prefix in Vault KV v2 mount, defaults to "aisa"
	Docs         fs.FS  // The repository's Markdown documents (README.md, docs/…), nil for none
}

// Handler serves both the admin JSON API and the embedded web dashboard.
type Handler struct {
	consul  ConsulClient
	vault   VaultClient
	metrics *metrics.Metrics
	opts    Options
	log     *slog.Logger
	checks  []server.Check
	docs    *docs
	mux     *http.ServeMux
}

// New returns an admin Handler configured with Consul, Vault, metrics, and readiness checks.
func New(c ConsulClient, v VaultClient, m *metrics.Metrics, log *slog.Logger, opts Options, checks ...server.Check) *Handler {
	if opts.ConsulPrefix == "" {
		opts.ConsulPrefix = "aisa"
	}
	opts.ConsulPrefix = strings.Trim(opts.ConsulPrefix, "/")
	if opts.VaultMount == "" {
		opts.VaultMount = "secret"
	}
	if opts.VaultPrefix == "" {
		opts.VaultPrefix = "aisa"
	}
	opts.VaultPrefix = strings.Trim(opts.VaultPrefix, "/")

	h := &Handler{
		consul:  c,
		vault:   v,
		metrics: m,
		opts:    opts,
		log:     log,
		checks:  checks,
		mux:     http.NewServeMux(),
	}
	d, err := newDocs(opts.Docs)
	if err != nil {
		log.Warn("the admin interface shows no documents", "err", err)
		d, _ = newDocs(nil)
	}
	h.docs = d
	h.routes()
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	// API routes
	h.mux.HandleFunc("GET /admin/api/overview", h.overview)
	h.mux.HandleFunc("GET /admin/api/consumers", h.listConsumers)
	h.mux.HandleFunc("POST /admin/api/consumers", protectJSON(h.createConsumer))
	h.mux.HandleFunc("DELETE /admin/api/consumers/{name}", protectJSON(h.deleteConsumer))
	h.mux.HandleFunc("POST /admin/api/consumers/{name}/rotate-key", protectJSON(h.rotateConsumerKey))
	h.mux.HandleFunc("GET /admin/api/quotas", h.listQuotas)
	h.mux.HandleFunc("POST /admin/api/quotas", protectJSON(h.createQuota))
	h.mux.HandleFunc("DELETE /admin/api/quotas/{name}", protectJSON(h.deleteQuota))
	h.mux.HandleFunc("GET /admin/api/backends", h.listBackends)
	h.mux.HandleFunc("GET /admin/api/models", h.listModels)
	h.mux.HandleFunc("GET /admin/api/docs", h.listDocs)
	h.mux.HandleFunc("GET /admin/api/docs/{path...}", h.getDoc)

	// Web UI routes (embedded files)
	fileServer := http.FileServer(http.FS(ui.FS))
	h.mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	h.mux.Handle("GET /admin/", http.StripPrefix("/admin", fileServer))
}

// protectJSON checks Content-Type and rejects cross-site requests on mutating endpoints.
func protectJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "cross-site requests are forbidden")
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next(w, r)
	}
}

// writeJSON writes status and encodes v as JSON to w.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an error message as JSON.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
