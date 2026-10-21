// Package config reads aisa's configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is aisa's configuration.
type Config struct {
	// Addr is the address the HTTP server listens on (AISA_ADDR).
	Addr string
	// MetricsAddr is the address of /metrics, apart from the API (AISA_METRICS_ADDR). Empty:
	// /metrics is on Addr. Set, Addr serves only the API, so a network policy can let a scraper
	// reach the metrics without reaching /v1/usage and /v1/decide (#46). The probes answer on
	// both.
	MetricsAddr string
	// LogLevel is the lowest level that is logged (AISA_LOG_LEVEL).
	LogLevel slog.Level
	// ShutdownTimeout is how long requests in flight get to finish after SIGTERM
	// (AISA_SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration

	Vault     Vault
	Consumers Consumers
	Ledger    Ledger
	// Consul is required: the consumers are declared there. Quotas are enabled when Redis is
	// configured too.
	Consul Consul
	Redis  Redis
	// DefaultQuotaProfile is the quota profile of a consumer whose declaration names none
	// (AISA_DEFAULT_QUOTA_PROFILE). Empty: such a consumer is denied (403 no_quota).
	DefaultQuotaProfile string
	Admin               Admin
	// Kubernetes is off outside a cluster.
	Kubernetes Kubernetes
}

// Admin configures the admin interface and Web UI at /admin.
type Admin struct {
	// Enabled reports whether the admin interface is served at /admin (AISA_ADMIN_ENABLED).
	// Default false.
	Enabled bool
}

// Kubernetes sets what aisa writes to the Kubernetes API: each consumer's Secret with a key it
// issues, and the Model objects (docs/concepts/KUBERNETES_OBJECTS.md).
type Kubernetes struct {
	// Enabled turns it on (AISA_KUBERNETES=true); aisa then runs in a pod and uses its service
	// account.
	Enabled bool
	// ProxyURL is the proxy's URL that applications call, such as
	// http://aisa-apisix.aisa.svc.cluster.local/v1 (AISA_PROXY_URL): OPENAI_BASE_URL in the
	// Secrets and spec.endpoint of the Models. Required when enabled.
	ProxyURL string
	// Namespace and PodName are aisa's own (POD_NAMESPACE, POD_NAME, from the downward API).
	// The namespace names the install on its objects and holds the leader's Lease.
	Namespace string
	PodName   string
	// Lease is the name of the Lease that elects the replica that writes (AISA_LEADER_LEASE).
	Lease string
	// Resync is the interval of the passes over the objects (AISA_KUBE_RESYNC); a change of
	// the consumers' declarations starts one at once.
	Resync time.Duration
	// Rotation is the age at which a consumer's key is replaced (AISA_KEY_ROTATION_INTERVAL);
	// 0 never replaces one for its age.
	Rotation time.Duration
	// Grace is how long a replaced key stays valid (AISA_KEY_ROTATION_GRACE).
	Grace time.Duration
	// EmptyGrace is how long the catalog may offer no model before the Model objects are
	// deleted (AISA_MODELS_EMPTY_GRACE): the proxy keeps its model routes as long.
	EmptyGrace time.Duration
}

// Consul is where aisa reads its quota profiles from (docs/concepts/CONSUL.md). The variables
// are Consul's own, except the prefix.
type Consul struct {
	// Addr is Consul's address (CONSUL_HTTP_ADDR): host:port, or a URL with http or https.
	// Empty turns quotas off.
	Addr string
	// CACert is a PEM file with the CA of Consul's certificate (CONSUL_CACERT).
	CACert string
	// Token is an ACL token (CONSUL_HTTP_TOKEN), for development. TokenFile is a file that holds
	// one (CONSUL_HTTP_TOKEN_FILE), such as one the Vault Agent writes from Vault's Consul
	// secrets engine. At most one of them is set.
	Token     string
	TokenFile string
	// Prefix is where aisa's keys are in Consul KV (AISA_CONSUL_PREFIX): quota profiles are
	// under <Prefix>/quotas/.
	Prefix string
}

// Redis holds the quota counters.
type Redis struct {
	// Addr is Redis's host:port (AISA_REDIS_ADDR). Empty turns quotas off.
	Addr string
	// Username and Password authenticate to Redis (AISA_REDIS_USERNAME, AISA_REDIS_PASSWORD).
	Username string
	Password string
}

// QuotasEnabled reports whether quotas are configured: Consul is always there, Redis is not.
func (c Config) QuotasEnabled() bool { return c.Redis.Addr != "" }

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
	// EstimateDefaultTokens is charged for a successful request whose usage event has no
	// token counts when aisa knows nothing yet of its consumer and model
	// (AISA_LEDGER_ESTIMATE_DEFAULT_TOKENS). Zero charges nothing then.
	EstimateDefaultTokens int64
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
		Kubernetes: Kubernetes{
			Lease:    "aisa-leader",
			Resync:   30 * time.Second,
			Rotation: 30 * 24 * time.Hour,
			Grace:    7 * 24 * time.Hour,
			// As the proxy's guard (GUARD_EMPTY_GRACE).
			EmptyGrace: 5 * time.Minute,
		},
		Consul: Consul{Prefix: "aisa"},
		Admin:  Admin{Enabled: false},
	}
}

// AddrFromEnv reads only AISA_ADDR, for the health check, which needs nothing else.
func AddrFromEnv(getenv func(string) string) (string, error) {
	v := getenv("AISA_ADDR")
	if v == "" {
		return Defaults().Addr, nil
	}
	if err := checkAddr("AISA_ADDR", v); err != nil {
		return "", err
	}
	return v, nil
}

// checkAddr checks that v is host:port or :port with a port: net.SplitHostPort takes ":" too,
// and a listener on it would get a port of the kernel's choosing.
func checkAddr(name, v string) error {
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%s: want host:port or :port, got %q: %w", name, v, err)
	}
	if port == "" {
		return fmt.Errorf("%s: want host:port or :port, got %q: no port", name, v)
	}
	return nil
}

// FromEnv reads the configuration with getenv, which is os.Getenv outside tests.
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Defaults()

	addr, err := AddrFromEnv(getenv)
	if err != nil {
		return cfg, err
	}
	cfg.Addr = addr
	if v := getenv("AISA_METRICS_ADDR"); v != "" {
		if err := checkAddr("AISA_METRICS_ADDR", v); err != nil {
			return cfg, err
		}
		if v == cfg.Addr {
			return cfg, fmt.Errorf("AISA_METRICS_ADDR: %q is AISA_ADDR too; unset it to serve /metrics there", v)
		}
		cfg.MetricsAddr = v
	}
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
	if v := getenv("AISA_LEDGER_ESTIMATE_DEFAULT_TOKENS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("AISA_LEDGER_ESTIMATE_DEFAULT_TOKENS: want a non-negative integer, got %q", v)
		}
		cfg.Ledger.EstimateDefaultTokens = n
	}
	if cfg.Consumers.MaxStale < cfg.Consumers.Refresh {
		return cfg, fmt.Errorf("AISA_CONSUMER_MAX_STALE (%s) must not be shorter than AISA_CONSUMER_REFRESH (%s)",
			cfg.Consumers.MaxStale, cfg.Consumers.Refresh)
	}

	if v := getenv("AISA_ADMIN_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("AISA_ADMIN_ENABLED: want a boolean, got %q: %w", v, err)
		}
		cfg.Admin.Enabled = b
	}

	if err := vaultFromEnv(getenv, &cfg.Vault); err != nil {
		return cfg, err
	}
	if err := quotasFromEnv(getenv, &cfg); err != nil {
		return cfg, err
	}
	if err := kubernetesFromEnv(getenv, &cfg.Kubernetes); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// kubernetesFromEnv reads what aisa writes to the Kubernetes API.
func kubernetesFromEnv(getenv func(string) string, k *Kubernetes) error {
	switch v := strings.ToLower(getenv("AISA_KUBERNETES")); v {
	case "", "false", "0":
	case "true", "1":
		k.Enabled = true
	default:
		return fmt.Errorf("AISA_KUBERNETES: want true or false, got %q", v)
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"AISA_KUBE_RESYNC", &k.Resync},
		{"AISA_KEY_ROTATION_GRACE", &k.Grace},
		{"AISA_MODELS_EMPTY_GRACE", &k.EmptyGrace},
	} {
		if err := duration(getenv, d.name, d.dst); err != nil {
			return err
		}
	}
	if v := getenv("AISA_KEY_ROTATION_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("AISA_KEY_ROTATION_INTERVAL: want a duration such as 720h, or 0 for no rotation, got %q", v)
		}
		k.Rotation = d
	}
	if v := getenv("AISA_LEADER_LEASE"); v != "" {
		k.Lease = v
	}
	k.ProxyURL = getenv("AISA_PROXY_URL")
	k.Namespace = getenv("POD_NAMESPACE")
	k.PodName = getenv("POD_NAME")
	if !k.Enabled {
		return nil
	}
	if u, err := url.Parse(k.ProxyURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("AISA_PROXY_URL: want the proxy's URL such as http://aisa-apisix.aisa.svc.cluster.local/v1, got %q", k.ProxyURL)
	}
	if k.Namespace == "" || k.PodName == "" {
		return fmt.Errorf("POD_NAMESPACE and POD_NAME are required with AISA_KUBERNETES: set them from the downward API")
	}
	return nil
}

// quotasFromEnv reads the Consul and Redis configuration. Consul is required, since the
// consumers are declared there; quotas need Redis too, and are off without it.
func quotasFromEnv(getenv func(string) string, cfg *Config) error {
	c := &cfg.Consul
	c.Addr = getenv("CONSUL_HTTP_ADDR")
	if c.Addr == "" {
		return fmt.Errorf("CONSUL_HTTP_ADDR is required: the consumers are declared in Consul KV")
	}
	c.CACert = getenv("CONSUL_CACERT")
	c.Token = getenv("CONSUL_HTTP_TOKEN")
	c.TokenFile = getenv("CONSUL_HTTP_TOKEN_FILE")
	if raw := getenv("AISA_CONSUL_PREFIX"); raw != "" {
		p := strings.Trim(raw, "/")
		if p == "" {
			return fmt.Errorf("AISA_CONSUL_PREFIX: want a path, got %q", raw)
		}
		c.Prefix = p
	}
	if c.Token != "" && c.TokenFile != "" {
		return fmt.Errorf("set CONSUL_HTTP_TOKEN or CONSUL_HTTP_TOKEN_FILE, not both")
	}
	cfg.DefaultQuotaProfile = strings.TrimSpace(getenv("AISA_DEFAULT_QUOTA_PROFILE"))
	r := &cfg.Redis
	r.Addr = getenv("AISA_REDIS_ADDR")
	r.Username = getenv("AISA_REDIS_USERNAME")
	r.Password = getenv("AISA_REDIS_PASSWORD")
	if r.Addr != "" {
		if _, _, err := net.SplitHostPort(r.Addr); err != nil {
			return fmt.Errorf("AISA_REDIS_ADDR: want host:port, got %q: %w", r.Addr, err)
		}
	}
	return nil
}

// vaultFromEnv reads the Vault configuration from environment variables.
func vaultFromEnv(getenv func(string) string, v *Vault) error {
	v.Addr = getenv("VAULT_ADDR")
	if v.Addr == "" {
		return fmt.Errorf("VAULT_ADDR is required: aisa reads its consumers from Vault")
	}
	v.CACert = getenv("VAULT_CACERT")
	v.Token = getenv("VAULT_TOKEN")
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{"AISA_VAULT_K8S_MOUNT", &v.KubernetesMount},
		{"AISA_VAULT_K8S_ROLE", &v.KubernetesRole},
		{"AISA_VAULT_KV_MOUNT", &v.KVMount},
		{"AISA_VAULT_PREFIX", &v.Prefix},
	} {
		raw := getenv(f.name)
		if raw == "" {
			continue
		}
		// Mount paths and the prefix are used with and without slashes around them.
		s := strings.Trim(raw, "/")
		if s == "" {
			return fmt.Errorf("%s: want a path, got %q", f.name, raw)
		}
		*f.dst = s
	}
	if p := getenv("AISA_VAULT_K8S_TOKEN_PATH"); p != "" {
		v.KubernetesTokenPath = p
	}
	return nil
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
