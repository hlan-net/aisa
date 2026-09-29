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
//	CONSUL_HTTP_ADDR            Consul's address; with AISA_REDIS_ADDR, turns token quotas on
//	CONSUL_CACERT               CA certificate of Consul, PEM                     system CAs
//	CONSUL_HTTP_TOKEN           a fixed Consul ACL token, for development
//	CONSUL_HTTP_TOKEN_FILE      file with the Consul ACL token, read for each request
//	AISA_CONSUL_PREFIX          prefix of aisa's keys in Consul KV                aisa
//	AISA_REDIS_ADDR             Redis host:port for the quota counters
//	AISA_REDIS_USERNAME         Redis ACL user
//	AISA_REDIS_PASSWORD         Redis password
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

	"github.com/redis/go-redis/v9"

	"github.com/hlan-net/aisa/internal/config"
	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/decide"
	"github.com/hlan-net/aisa/internal/ledger"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/quotas"
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

	checks := []server.Check{{Name: "consumers", Probe: store.Ready}}
	// Interfaces stay nil without quotas, not a nil *quotas.Quota inside them.
	var (
		quota   decide.QuotaChecker
		charger ledger.Charger
	)
	if cfg.QuotasEnabled() {
		q, profiles, closeQuotas, err := newQuotas(cfg, m, log.With("component", "quotas"))
		if err != nil {
			return err
		}
		defer closeQuotas()
		go profiles.Run(ctx)
		quota, charger = q, q
		checks = append(checks, server.Check{Name: "quota_profiles", Probe: profiles.Ready})
	} else {
		log.Warn("token quotas are off: set CONSUL_HTTP_ADDR and AISA_REDIS_ADDR to turn them on")
	}

	srv := server.New(log, m.Handler(), cfg.ShutdownTimeout, checks...)
	routes(srv,
		decide.New(store, quota, m, log.With("component", "decide")),
		ledger.New(m, ledger.NewDedup(cfg.Ledger.DedupCapacity, cfg.Ledger.DedupTTL), charger, log.With("component", "ledger")))
	if err := srv.Run(ctx, cfg.Addr); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info("stopped")
	return nil
}

// newQuotas returns the token quotas with their profiles from Consul and counters in Redis,
// and a function that closes the connections to Redis.
func newQuotas(cfg config.Config, m *metrics.Metrics, log *slog.Logger) (*quotas.Quota, *quotas.Profiles, func(), error) {
	cc, err := consul.New(consul.Options{
		Addr: cfg.Consul.Addr, CACert: cfg.Consul.CACert, Token: cfg.Consul.Token, TokenFile: cfg.Consul.TokenFile,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("consul: %w", err)
	}
	profiles := quotas.NewProfiles(
		quotas.ConsulSource{Client: cc, Prefix: cfg.Consul.Prefix + "/quotas/"},
		log,
		quotas.ProfileOptions{OnLoad: func(s quotas.LoadStats) { observeProfiles(m, s) }})
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Redis.Addr, Username: cfg.Redis.Username, Password: cfg.Redis.Password,
	})
	q := quotas.New(profiles, quotas.NewWindow(rdb, "aisa:"), m, log, quotas.Options{})
	return q, profiles, func() { _ = rdb.Close() }, nil
}

// observeProfiles puts the result of a load of the quota profiles into the metrics.
func observeProfiles(m *metrics.Metrics, s quotas.LoadStats) {
	if s.Err != nil {
		m.QuotaProfileLoads.WithLabelValues(metrics.LoadError).Inc()
		return
	}
	m.QuotaProfileLoads.WithLabelValues(metrics.LoadOK).Inc()
	m.QuotaProfiles.Set(float64(s.Valid))
	m.QuotaProfilesInvalid.Set(float64(s.Invalid))
}

// routes registers the contract endpoints.
func routes(srv *server.Server, decision, usage http.Handler) {
	srv.Handle("POST /v1/decide", decision)
	srv.Handle("GET /v1/decide", decision)
	srv.Handle("POST /v1/usage", usage)
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
