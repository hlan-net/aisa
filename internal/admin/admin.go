// Package admin provides an HTTP administration API and Web UI for aisa, allowing the
// holder to inspect and manage consumers, quota profiles, backends, and models in Consul and Vault.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Delete(ctx context.Context, key string, recurse bool) error
	HealthService(ctx context.Context, service string, passingOnly bool) ([]consul.ServiceHealth, error)
}

// VaultClient is the subset of Vault operations needed by the admin interface.
type VaultClient interface {
	List(ctx context.Context, mount, path string) ([]string, error)
	Read(ctx context.Context, mount, path string) (map[string]any, error)
	Write(ctx context.Context, mount, path string, data map[string]any) error
	Delete(ctx context.Context, mount, path string) error
	Probe(ctx context.Context) error
}

// Options configure the admin handler.
type Options struct {
	ConsulPrefix string // Prefix in Consul KV, defaults to "aisa"
	VaultMount   string // KV v2 mount in Vault, defaults to "secret"
	VaultPrefix  string // Prefix in Vault KV v2 mount, defaults to "aisa"
}

// Handler serves both the admin JSON API and the embedded web dashboard.
type Handler struct {
	consul  ConsulClient
	vault   VaultClient
	metrics *metrics.Metrics
	opts    Options
	log     *slog.Logger
	checks  []server.Check
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
	h.mux.HandleFunc("POST /admin/api/consumers", h.createConsumer)
	h.mux.HandleFunc("DELETE /admin/api/consumers/{name}", h.deleteConsumer)
	h.mux.HandleFunc("POST /admin/api/consumers/{name}/rotate-key", h.rotateConsumerKey)
	h.mux.HandleFunc("GET /admin/api/quotas", h.listQuotas)
	h.mux.HandleFunc("POST /admin/api/quotas", h.createQuota)
	h.mux.HandleFunc("DELETE /admin/api/quotas/{name}", h.deleteQuota)
	h.mux.HandleFunc("GET /admin/api/backends", h.listBackends)
	h.mux.HandleFunc("GET /admin/api/models", h.listModels)

	// Web UI routes (embedded files)
	fileServer := http.FileServer(http.FS(ui.FS))
	h.mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	h.mux.Handle("GET /admin/", http.StripPrefix("/admin", fileServer))
}

// generateAPIKey generates a cryptographically secure random API key and its sha256 hex digest.
func generateAPIKey() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("read random bytes: %w", err)
	}
	plaintext = "aisa_sec_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	return plaintext, hash, nil
}

func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
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
