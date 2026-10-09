package config

import (
	"log/slog"
	"testing"
	"time"
)

// env returns a getenv for the given variables, with VAULT_ADDR set unless given.
func env(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		if k == "VAULT_ADDR" {
			return "http://vault:8200"
		}
		return ""
	}
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.Vault.Addr = "http://vault:8200"
	if cfg != want {
		t.Errorf("cfg = %+v, want the defaults %+v", cfg, want)
	}
}

func TestFromEnvVault(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{
		"VAULT_ADDR":                 "https://vault.example:8200",
		"VAULT_CACERT":               "/etc/vault/ca.pem",
		"VAULT_TOKEN":                "dev-root",
		"AISA_VAULT_K8S_MOUNT":       "/k8s-cluster/",
		"AISA_VAULT_K8S_ROLE":        "aisa-prod",
		"AISA_VAULT_K8S_TOKEN_PATH":  "/var/run/token",
		"AISA_VAULT_KV_MOUNT":        "kv",
		"AISA_VAULT_PREFIX":          "/teams/aisa/",
		"AISA_CONSUMER_REFRESH":      "30s",
		"AISA_CONSUMER_MISS_REFRESH": "1s",
		"AISA_CONSUMER_MAX_STALE":    "5m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Vault{
		Addr: "https://vault.example:8200", CACert: "/etc/vault/ca.pem", Token: "dev-root",
		KubernetesMount: "k8s-cluster", KubernetesRole: "aisa-prod", KubernetesTokenPath: "/var/run/token",
		KVMount: "kv", Prefix: "teams/aisa",
	}
	if cfg.Vault != want {
		t.Errorf("Vault = %+v, want %+v", cfg.Vault, want)
	}
	if c := cfg.Consumers; c.Refresh != 30*time.Second || c.MissRefresh != time.Second || c.MaxStale != 5*time.Minute {
		t.Errorf("Consumers = %+v", c)
	}
}

func TestFromEnv(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{
		"AISA_ADDR":             "127.0.0.1:9000",
		"AISA_METRICS_ADDR":     ":9090",
		"AISA_LOG_LEVEL":        "debug",
		"AISA_SHUTDOWN_TIMEOUT": "30s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.Addr, want.LogLevel, want.ShutdownTimeout = "127.0.0.1:9000", slog.LevelDebug, 30*time.Second
	want.MetricsAddr = ":9090"
	want.Vault.Addr = "http://vault:8200"
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestFromEnvLogLevels(t *testing.T) {
	for v, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
		"WARN": slog.LevelWarn,
	} {
		cfg, err := FromEnv(env(map[string]string{"AISA_LOG_LEVEL": v}))
		if err != nil {
			t.Errorf("%s: %v", v, err)
		}
		if cfg.LogLevel != want {
			t.Errorf("%s: level = %v, want %v", v, cfg.LogLevel, want)
		}
	}
}

func TestFromEnvRejects(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"address without a port":  {"AISA_ADDR": "localhost"},
		"metrics address no port": {"AISA_METRICS_ADDR": "localhost"},
		"empty port":              {"AISA_ADDR": ":"},
		"metrics empty port":      {"AISA_METRICS_ADDR": "localhost:"},
		"metrics address = api":   {"AISA_ADDR": ":8080", "AISA_METRICS_ADDR": ":8080"},
		"unknown log level":       {"AISA_LOG_LEVEL": "loud"},
		"log level with offset":   {"AISA_LOG_LEVEL": "info+1"},
		"timeout not a duration":  {"AISA_SHUTDOWN_TIMEOUT": "ten"},
		"timeout of zero":         {"AISA_SHUTDOWN_TIMEOUT": "0s"},
		"negative timeout":        {"AISA_SHUTDOWN_TIMEOUT": "-5s"},
		"no Vault address":        {"VAULT_ADDR": ""},
		"refresh not a duration":  {"AISA_CONSUMER_REFRESH": "often"},
		"prefix of slashes only":  {"AISA_VAULT_PREFIX": "/"},
		"mount of slashes only":   {"AISA_VAULT_KV_MOUNT": "//"},
		"max stale below refresh": {"AISA_CONSUMER_REFRESH": "10m", "AISA_CONSUMER_MAX_STALE": "5m"},
		"dedup capacity not int":  {"AISA_LEDGER_DEDUP_CAPACITY": "invalid"},
		"dedup capacity zero":     {"AISA_LEDGER_DEDUP_CAPACITY": "0"},
		"dedup capacity negative": {"AISA_LEDGER_DEDUP_CAPACITY": "-1"},
		"dedup ttl not duration":  {"AISA_LEDGER_DEDUP_TTL": "forever"},
		"dedup ttl zero":          {"AISA_LEDGER_DEDUP_TTL": "0s"},
		"consul without redis":    {"CONSUL_HTTP_ADDR": "consul:8500"},
		"redis without consul":    {"AISA_REDIS_ADDR": "redis:6379"},
		"redis without a port":    {"CONSUL_HTTP_ADDR": "consul:8500", "AISA_REDIS_ADDR": "redis"},
		"consul token twice": {"CONSUL_HTTP_ADDR": "consul:8500", "AISA_REDIS_ADDR": "redis:6379",
			"CONSUL_HTTP_TOKEN": "a", "CONSUL_HTTP_TOKEN_FILE": "/b"},
		"consul prefix of slashes only": {"AISA_CONSUL_PREFIX": "/"},
	} {
		if _, err := FromEnv(env(vars)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFromEnvLedger(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{
		"AISA_LEDGER_DEDUP_CAPACITY": "50000",
		"AISA_LEDGER_DEDUP_TTL":      "10m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ledger.DedupCapacity != 50000 {
		t.Errorf("DedupCapacity = %d, want 50000", cfg.Ledger.DedupCapacity)
	}
	if cfg.Ledger.DedupTTL != 10*time.Minute {
		t.Errorf("DedupTTL = %v, want 10m", cfg.Ledger.DedupTTL)
	}
}

func TestFromEnvQuotas(t *testing.T) {
	cfg, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotasEnabled() {
		t.Error("quotas on without Consul and Redis")
	}

	cfg, err = FromEnv(env(map[string]string{
		"CONSUL_HTTP_ADDR":       "https://consul.example:8501",
		"CONSUL_CACERT":          "/etc/consul/ca.pem",
		"CONSUL_HTTP_TOKEN_FILE": "/vault/secrets/consul-token",
		"AISA_CONSUL_PREFIX":     "/teams/aisa/",
		"AISA_REDIS_ADDR":        "redis:6379",
		"AISA_REDIS_USERNAME":    "aisa",
		"AISA_REDIS_PASSWORD":    "secret",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.QuotasEnabled() {
		t.Error("quotas off with Consul and Redis")
	}
	wantConsul := Consul{
		Addr: "https://consul.example:8501", CACert: "/etc/consul/ca.pem",
		TokenFile: "/vault/secrets/consul-token", Prefix: "teams/aisa",
	}
	if cfg.Consul != wantConsul {
		t.Errorf("Consul = %+v, want %+v", cfg.Consul, wantConsul)
	}
	if want := (Redis{Addr: "redis:6379", Username: "aisa", Password: "secret"}); cfg.Redis != want {
		t.Errorf("Redis = %+v, want %+v", cfg.Redis, want)
	}
}
