// Package config reads aisa's configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

// Config is aisa's configuration.
type Config struct {
	// Addr is the address the HTTP server listens on (AISA_ADDR).
	Addr string
	// LogLevel is the lowest level that is logged (AISA_LOG_LEVEL).
	LogLevel slog.Level
	// ShutdownTimeout is how long requests in flight get to finish after SIGTERM
	// (AISA_SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration

	Vault     Vault
	Consumers Consumers
	Ledger    Ledger
}

// Vault is where aisa reads consumers from (docs/concepts/VAULT.md).
type Vault struct {
	// Addr is Vault's address (VAULT_ADDR, required).
	Addr string
	// CACert is a PEM file with the CA of Vault's certificate (VAULT_CACERT); empty uses the
	// system's CAs.
	CACert string
	// Token is a fixed Vault token (VAULT_TOKEN), for development. When it is empty, aisa logs
	// in with its Kubernetes service account.
	Token string
	// KubernetesMount and KubernetesRole are the Kubernetes auth method's mount path and the role
	// aisa logs in as (AISA_VAULT_K8S_MOUNT, AISA_VAULT_K8S_ROLE).
	KubernetesMount string
	KubernetesRole  string
	// KubernetesTokenPath is the service account token file (AISA_VAULT_K8S_TOKEN_PATH).
	KubernetesTokenPath string
	// KVMount and Prefix locate aisa's secrets: consumers are under <KVMount>/<Prefix>/consumers
	// (AISA_VAULT_KV_MOUNT, AISA_VAULT_PREFIX).
	KVMount string
	Prefix  string
}

// Consumers sets how the consumers are kept in memory.
type Consumers struct {
	// Refresh is the interval of the periodic reload (AISA_CONSUMER_REFRESH).
	Refresh time.Duration
	// MissRefresh is the least time between reloads caused by unknown keys
	// (AISA_CONSUMER_MISS_REFRESH).
	MissRefresh time.Duration
	// MaxStale is how old the consumers may get while Vault cannot be read before aisa fails
	// closed (AISA_CONSUMER_MAX_STALE).
	MaxStale time.Duration
}

// Ledger sets how the usage ledger operates.
type Ledger struct {
	// DedupCapacity is the max number of request IDs kept to deduplicate events
	// (AISA_LEDGER_DEDUP_CAPACITY).
	DedupCapacity int
	// DedupTTL is how long a request ID is kept before expiring (AISA_LEDGER_DEDUP_TTL).
	DedupTTL time.Duration
}

// Defaults are the values used for variables that are unset or empty.
func Defaults() Config {
	return Config{
		Addr:            ":8080",
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
		Vault: Vault{
			KubernetesMount:     "kubernetes",
			KubernetesRole:      "aisa",
			KubernetesTokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token",
			KVMount:             "secret",
			Prefix:              "aisa",
		},
		Consumers: Consumers{
			Refresh:     time.Minute,
			MissRefresh: 5 * time.Second,
			MaxStale:    15 * time.Minute,
		},
		Ledger: Ledger{
			DedupCapacity: 100_000,
			DedupTTL:      15 * time.Minute,
		},
	}
}

// AddrFromEnv reads only AISA_ADDR, for the health check, which needs nothing else.
func AddrFromEnv(getenv func(string) string) (string, error) {
	v := getenv("AISA_ADDR")
	if v == "" {
		return Defaults().Addr, nil
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return "", fmt.Errorf("AISA_ADDR: want host:port or :port, got %q: %w", v, err)
	}
	return v, nil
}

// FromEnv reads the configuration with getenv, which is os.Getenv outside tests.
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Defaults()

	addr, err := AddrFromEnv(getenv)
	if err != nil {
		return cfg, err
	}
	cfg.Addr = addr
	if v := getenv("AISA_LOG_LEVEL"); v != "" {
		// Not slog's own parser: it also takes offsets such as INFO+1.
		switch strings.ToLower(v) {
		case "debug":
			cfg.LogLevel = slog.LevelDebug
		case "info":
			cfg.LogLevel = slog.LevelInfo
		case "warn":
			cfg.LogLevel = slog.LevelWarn
		case "error":
			cfg.LogLevel = slog.LevelError
		default:
			return cfg, fmt.Errorf("AISA_LOG_LEVEL: want debug, info, warn or error, got %q", v)
		}
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"AISA_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout},
		{"AISA_CONSUMER_REFRESH", &cfg.Consumers.Refresh},
		{"AISA_CONSUMER_MISS_REFRESH", &cfg.Consumers.MissRefresh},
		{"AISA_CONSUMER_MAX_STALE", &cfg.Consumers.MaxStale},
		{"AISA_LEDGER_DEDUP_TTL", &cfg.Ledger.DedupTTL},
	} {
		if err := duration(getenv, d.name, d.dst); err != nil {
			return cfg, err
		}
	}
	if v := getenv("AISA_LEDGER_DEDUP_CAPACITY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("AISA_LEDGER_DEDUP_CAPACITY: want a positive integer, got %q", v)
		}
		cfg.Ledger.DedupCapacity = n
	}
	if cfg.Consumers.MaxStale < cfg.Consumers.Refresh {
		return cfg, fmt.Errorf("AISA_CONSUMER_MAX_STALE (%s) must not be shorter than AISA_CONSUMER_REFRESH (%s)",
			cfg.Consumers.MaxStale, cfg.Consumers.Refresh)
	}

	cfg.Vault.Addr = getenv("VAULT_ADDR")
	if cfg.Vault.Addr == "" {
		return cfg, fmt.Errorf("VAULT_ADDR is required: aisa reads its consumers from Vault")
	}
	cfg.Vault.CACert = getenv("VAULT_CACERT")
	cfg.Vault.Token = getenv("VAULT_TOKEN")
	for _, v := range []struct {
		name string
		dst  *string
	}{
		{"AISA_VAULT_K8S_MOUNT", &cfg.Vault.KubernetesMount},
		{"AISA_VAULT_K8S_ROLE", &cfg.Vault.KubernetesRole},
		{"AISA_VAULT_KV_MOUNT", &cfg.Vault.KVMount},
		{"AISA_VAULT_PREFIX", &cfg.Vault.Prefix},
	} {
		raw := getenv(v.name)
		if raw == "" {
			continue
		}
		// Mount paths and the prefix are used with and without slashes around them.
		s := strings.Trim(raw, "/")
		if s == "" {
			return cfg, fmt.Errorf("%s: want a path, got %q", v.name, raw)
		}
		*v.dst = s
	}
	if p := getenv("AISA_VAULT_K8S_TOKEN_PATH"); p != "" {
		cfg.Vault.KubernetesTokenPath = p
	}
	return cfg, nil
}

// duration sets *dst from the variable name when it is set, and requires a positive duration.
func duration(getenv func(string) string, name string, dst *time.Duration) error {
	v := getenv(name)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: want a duration such as 10s, got %q: %w", name, v, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s: want a positive duration, got %q", name, v)
	}
	*dst = d
	return nil
}
