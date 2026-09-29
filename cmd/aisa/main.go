// Command aisa is the governance layer for LLM traffic: identities, token quotas, money
// budgets and usage metrics next to an AI gateway. See docs/concepts/ARCHITECTURE.md.
//
// It is configured with environment variables:
//
//	AISA_ADDR                   address to listen on                              :8080
//	AISA_LOG_LEVEL              debug, info, warn or error                        info
//	AISA_SHUTDOWN_TIMEOUT       time for requests in flight at the end            10s
//	VAULT_ADDR                  Vault's address (required)
//	VAULT_CACERT                CA certificate of Vault, PEM                      system CAs
//	VAULT_TOKEN                 a fixed token, for development                    Kubernetes auth
//	AISA_VAULT_K8S_MOUNT        Kubernetes auth method's mount path               kubernetes
//	AISA_VAULT_K8S_ROLE         Vault role aisa logs in as                        aisa
//	AISA_VAULT_K8S_TOKEN_PATH   service account token file                        the pod's
//	AISA_VAULT_KV_MOUNT         KV v2 mount with aisa's secrets                   secret
//	AISA_VAULT_PREFIX           path of aisa's secrets in that mount              aisa
//	AISA_CONSUMER_REFRESH       interval of reloading the consumers               1m
//	AISA_CONSUMER_MISS_REFRESH  least time between reloads for unknown keys       5s
//	AISA_CONSUMER_MAX_STALE     age of the consumers after which aisa fails closed 15m
//	AISA_LEDGER_DEDUP_CAPACITY  request IDs kept for dedup                        100000
//	AISA_LEDGER_DEDUP_TTL       how long request IDs are kept for dedup           15m
//
// With -healthcheck it asks the aisa that runs on AISA_ADDR for /healthz and exits, for the
// health check of a container.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hlan-net/aisa/internal/config"
	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/decide"
	"github.com/hlan-net/aisa/internal/ledger"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/server"
	"github.com/hlan-net/aisa/internal/vault"
	"github.com/hlan-net/aisa/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aisa:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("aisa", flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print the version and exit")
	healthcheck := flags.Bool("healthcheck", false, "probe /healthz of the running aisa and exit (for container health checks)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}

	if *healthcheck {
		addr, err := config.AddrFromEnv(os.Getenv)
		if err != nil {
			return fmt.Errorf("configuration: %w", err)
		}
		return probe(healthURL(addr))
	}
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting aisa", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	vc, err := vault.New(vault.Options{Addr: cfg.Vault.Addr, CACert: cfg.Vault.CACert, Auth: vaultAuth(cfg.Vault)})
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	m := metrics.New(version.Version)
	store := consumers.New(
		consumers.Vault{Client: vc, Mount: cfg.Vault.KVMount, Prefix: cfg.Vault.Prefix},
		log.With("component", "consumers"),
		consumers.Options{
			Refresh:     cfg.Consumers.Refresh,
			MissRefresh: cfg.Consumers.MissRefresh,
			MaxStale:    cfg.Consumers.MaxStale,
			OnLoad:      func(s consumers.Stats) { observeLoad(m, s, time.Now()) },
		})
	go store.Run(ctx)

	srv := server.New(log, m.Handler(), cfg.ShutdownTimeout,
		server.Check{Name: "consumers", Probe: store.Ready})
	decision := decide.New(store, m, log.With("component", "decide"))
	srv.Handle("POST /v1/decide", decision)
	srv.Handle("GET /v1/decide", decision)
	ledg := ledger.New(m, ledger.NewDedup(cfg.Ledger.DedupCapacity, cfg.Ledger.DedupTTL), log.With("component", "ledger"))
	srv.Handle("POST /v1/usage", ledg)
	if err := srv.Run(ctx, cfg.Addr); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info("stopped")
	return nil
}

// observeLoad puts the result of a load of the consumers into the metrics.
func observeLoad(m *metrics.Metrics, s consumers.Stats, now time.Time) {
	if s.Err != nil {
		m.ConsumerLoads.WithLabelValues(metrics.LoadError).Inc()
		return
	}
	m.ConsumerLoads.WithLabelValues(metrics.LoadOK).Inc()
	m.Consumers.Set(float64(s.Consumers))
	m.ConsumerKeys.Set(float64(s.Keys))
	m.ConsumersUnreadable.Set(float64(s.Unreadable))
	m.ConsumersLoaded.Set(float64(now.Unix()))
}

// vaultAuth logs in with a fixed token when one is configured, as in development, and with the
// pod's Kubernetes service account otherwise.
func vaultAuth(v config.Vault) vault.Auth {
	if v.Token != "" {
		return vault.TokenAuth{Token: v.Token}
	}
	return vault.KubernetesAuth{Mount: v.KubernetesMount, Role: v.KubernetesRole, TokenPath: v.KubernetesTokenPath}
}

// healthURL is the URL of /healthz of an aisa that listens on addr, seen from the same host.
func healthURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// config.FromEnv has checked the address.
		return "http://" + addr + "/healthz"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

// probe performs an HTTP GET and returns an error unless the answer is 2xx. The image has no
// shell or curl, so Docker's health check runs aisa itself.
func probe(url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}
