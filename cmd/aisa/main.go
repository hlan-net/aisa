// Command aisa is the governance layer for LLM traffic: identities, token quotas, money
// budgets and usage metrics next to an AI gateway. See docs/concepts/ARCHITECTURE.md.
//
// It is configured with environment variables:
//
//	AISA_ADDR                   address to listen on                              :8080
//	AISA_METRICS_ADDR           address of /metrics apart from the API (#46)     (unset: AISA_ADDR)
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
//	AISA_LEDGER_ESTIMATE_DEFAULT_TOKENS  tokens charged for missing usage with no
//	                            history of the consumer and model (0: nothing)    0
//	CONSUL_HTTP_ADDR            Consul's address, required: the consumers are declared there
//	CONSUL_CACERT               CA certificate of Consul, PEM                     system CAs
//	CONSUL_HTTP_TOKEN           a fixed Consul ACL token, for development
//	CONSUL_HTTP_TOKEN_FILE      file with the Consul ACL token, read for each request
//	AISA_CONSUL_PREFIX          prefix of aisa's keys in Consul KV                aisa
//	AISA_ADMIN_ENABLED          enable admin interface and web UI at /admin       false
//	AISA_REDIS_ADDR             Redis host:port for the quota counters; turns token quotas on
//	AISA_REDIS_USERNAME         Redis ACL user
//	AISA_REDIS_PASSWORD         Redis password
//	AISA_KUBERNETES             true: write the consumers' Secrets and the Model objects
//	                            (docs/concepts/KUBERNETES_OBJECTS.md)                false
//	AISA_PROXY_URL              the proxy's URL that applications call (required with it)
//	POD_NAMESPACE, POD_NAME     aisa's namespace and pod, from the downward API (required with it)
//	AISA_LEADER_LEASE           Lease that elects the replica that writes          aisa-leader
//	AISA_KUBE_RESYNC            interval of the passes over the objects            30s
//	AISA_KEY_ROTATION_INTERVAL  age at which a consumer's key is replaced (0: never) 720h
//	AISA_KEY_ROTATION_GRACE     how long a replaced key stays valid                168h
//
// With -healthcheck it asks the aisa that runs on AISA_ADDR for /healthz and exits, for the
// health check of a container.
package main

import (
	// The budgets' calendar month is in the holder's time zone; the image has no zoneinfo.
	_ "time/tzdata"

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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/hlan-net/aisa/internal/admin"
	"github.com/hlan-net/aisa/internal/budgets"
	"github.com/hlan-net/aisa/internal/config"
	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/decide"
	"github.com/hlan-net/aisa/internal/kube"
	"github.com/hlan-net/aisa/internal/ledger"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/pricing"
	"github.com/hlan-net/aisa/internal/quotas"
	"github.com/hlan-net/aisa/internal/render"
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
	cc, err := consul.New(consul.Options{
		Addr: cfg.Consul.Addr, CACert: cfg.Consul.CACert, Token: cfg.Consul.Token, TokenFile: cfg.Consul.TokenFile,
	})
	if err != nil {
		return fmt.Errorf("consul: %w", err)
	}
	// The consumers: declared in Consul KV, with the hashes of their keys in Vault. A change of
	// the declarations starts a pass over their Secrets.
	declared := make(chan struct{}, 1)
	declarations := consumers.NewDeclarations(
		consul.KVPrefix{Client: cc, Prefix: cfg.Consul.Prefix + "/consumers/", What: "consumer declarations"},
		log.With("component", "consumers"),
		consumers.DeclarationOptions{OnLoad: func(s consumers.DeclarationStats) {
			observeDeclarations(m, s)
			select {
			case declared <- struct{}{}:
			default:
			}
		}})
	go declarations.Run(ctx)
	store := consumers.New(
		consumers.Vault{Client: vc, Mount: cfg.Vault.KVMount, Prefix: cfg.Vault.Prefix},
		log.With("component", "consumers"),
		consumers.Options{
			Refresh:     cfg.Consumers.Refresh,
			MissRefresh: cfg.Consumers.MissRefresh,
			MaxStale:    cfg.Consumers.MaxStale,
			OnLoad:      func(s consumers.Stats) { observeLoad(m, s, time.Now()) },
			Declared:    declarations,
		})
	go store.Run(ctx)

	prices := pricing.New(
		consul.KVPrefix{Client: cc, Prefix: cfg.Consul.Prefix + "/pricing/", What: "model prices"},
		log.With("component", "pricing"),
		pricing.Options{OnLoad: func(s pricing.LoadStats) { observePrices(m, s) }})
	go prices.Run(ctx)

	checks := []server.Check{
		{Name: "consumers", Probe: store.Ready},
		{Name: "consumer_declarations", Probe: declarations.Ready},
		{Name: "model_prices", Probe: prices.Ready},
	}
	// Interfaces stay nil without quotas, not a nil *quotas.Quota inside them.
	var (
		quota           decide.QuotaChecker
		charger         ledger.Charger
		budget          decide.BudgetChecker
		budgetCharger   ledger.Charger
		provider        decide.ProviderChecker
		providerCharger ledger.ProviderCharger
		budgetUsage     admin.BudgetUsage
	)
	if cfg.QuotasEnabled() {
		q, profiles, rdb, closeQuotas := newQuotas(cfg, cc, declarations, m, log.With("component", "quotas"))
		defer closeQuotas()
		if err := m.Register(profiles.Collectors()...); err != nil {
			return fmt.Errorf("quota profile metrics: %w", err)
		}
		go profiles.Run(ctx)
		quota, charger = q, q
		checks = append(checks, server.Check{Name: "quota_profiles", Probe: profiles.Ready})

		// The budgets count the months in the same Redis, and keep a copy of the totals in
		// Consul KV (docs/features/quotas-and-budgets.md, Budgets).
		monthly, budgetSet := newBudgets(cfg, cc, rdb, prices, m, log.With("component", "budgets"))
		go budgetSet.Run(ctx)
		go monthly.Run(ctx)
		budget, budgetCharger, budgetUsage = monthly, monthly, monthly
		checks = append(checks, server.Check{Name: "budgets", Probe: budgetSet.Ready})

		// The ceiling above the consumers (#33): a budget per provider key, applied through
		// the catalog, which says which keys serve a model.
		ceiling, providerSet, providerMonthly, catalog := newCeiling(cfg, cc, rdb, m, log.With("component", "budgets"))
		go providerSet.Run(ctx)
		go providerMonthly.Run(ctx)
		go catalog.Run(ctx)
		provider, providerCharger = ceiling, ceiling
		checks = append(checks, server.Check{Name: "provider_budgets", Probe: ceiling.Ready})
	} else {
		log.Warn("token quotas and budgets are off: set AISA_REDIS_ADDR to turn them on")
	}

	var adminHandler http.Handler
	if cfg.Admin.Enabled {
		adminHandler = admin.New(cc, vc, m, log.With("component", "admin"),
			admin.Options{ConsulPrefix: cfg.Consul.Prefix, VaultMount: cfg.Vault.KVMount, VaultPrefix: cfg.Vault.Prefix, Budgets: budgetUsage},
			checks...)
	}

	if cfg.Kubernetes.Enabled {
		src := kubeSources{vault: vc, consul: cc, declarations: declarations, declared: declared}
		if err := runKubernetes(ctx, cfg, src, m, log.With("component", "kubernetes")); err != nil {
			return err
		}
	} else {
		log.Info("aisa writes no Kubernetes objects: set AISA_KUBERNETES=true to issue the consumers' Secrets and the Model objects")
	}

	srv := server.New(log, m.Handler(), cfg.ShutdownTimeout, checks...)
	routes(srv,
		decide.New(store, quota, m, log.With("component", "decide"),
			decide.Options{DefaultQuotaProfile: cfg.DefaultQuotaProfile, Budget: budget, Provider: provider}),
		ledger.New(m, ledger.NewDedup(cfg.Ledger.DedupCapacity, cfg.Ledger.DedupTTL), charger, log.With("component", "ledger"),
			ledger.Options{EstimateDefaultTokens: cfg.Ledger.EstimateDefaultTokens, Pricer: prices, Budget: budgetCharger, Provider: providerCharger}),
		render.New(m, log.With("component", "render")),
		adminHandler)
	if err := srv.Run(ctx, cfg.Addr, cfg.MetricsAddr); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info("stopped")
	return nil
}

// newQuotas returns the token quotas with their profiles from Consul and counters in Redis,
// the Redis client, and a function that closes the connections to Redis. The profiles know
// the default and the consumers' declarations, to report a profile that is named and not
// there (#82).
func newQuotas(cfg config.Config, cc *consul.Client, declarations *consumers.Declarations, m *metrics.Metrics, log *slog.Logger) (*quotas.Quota, *quotas.Profiles, *redis.Client, func()) {
	profiles := quotas.NewProfiles(
		quotas.ConsulSource{Client: cc, Prefix: cfg.Consul.Prefix + "/quotas/"},
		log,
		quotas.ProfileOptions{
			OnLoad:    func(s quotas.LoadStats) { observeProfiles(m, s) },
			Default:   cfg.DefaultQuotaProfile,
			Consumers: declarations,
		})
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Redis.Addr, Username: cfg.Redis.Username, Password: cfg.Redis.Password,
	})
	q := quotas.New(profiles, quotas.NewWindow(rdb, "aisa:"), m, log, quotas.Options{})
	return q, profiles, rdb, func() { _ = rdb.Close() }
}

// newBudgets returns the monthly budgets with their definitions from Consul, counters in
// Redis, persisted totals in Consul KV and the prices that say which models they bound.
func newBudgets(cfg config.Config, cc *consul.Client, rdb *redis.Client, prices *pricing.Prices, m *metrics.Metrics, log *slog.Logger) (*budgets.Monthly, *budgets.Budgets) {
	// Validated by the config.
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	var monthly *budgets.Monthly
	set := budgets.NewBudgets(
		consul.KVPrefix{Client: cc, Prefix: cfg.Consul.Prefix + "/budgets/", What: "budgets"},
		log,
		budgets.Options{OnLoad: func(s budgets.LoadStats) {
			observeBudgets(m, s)
			if s.Err == nil && monthly != nil {
				monthly.Observe(context.Background())
			}
		}})
	monthly = budgets.NewMonthly(set, budgets.NewMonths(rdb, "aisa:"), cc, prices, m, log,
		budgets.MonthlyOptions{Location: loc, StatePrefix: cfg.Consul.Prefix + "/state/"})
	return monthly, set
}

// newCeiling returns the ceiling of the provider budgets, aisa/budgets/providers/<key>, with
// the budgets themselves, their counters (in Redis, persisted under aisa/state/providers/) and
// the catalog that says which keys serve a model; the caller runs the last three.
func newCeiling(cfg config.Config, cc *consul.Client, rdb *redis.Client, m *metrics.Metrics, log *slog.Logger) (*budgets.Ceiling, *budgets.Budgets, *budgets.Monthly, *budgets.Catalog) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	var monthly *budgets.Monthly
	set := budgets.NewBudgets(
		consul.KVPrefix{Client: cc, Prefix: cfg.Consul.Prefix + "/budgets/providers/", What: "provider budgets"},
		log,
		budgets.Options{OnLoad: func(s budgets.LoadStats) {
			if s.Err != nil {
				m.ProviderBudgetLoads.WithLabelValues(metrics.LoadError).Inc()
				return
			}
			m.ProviderBudgetLoads.WithLabelValues(metrics.LoadOK).Inc()
			m.ProviderBudgets.Set(float64(s.Valid))
			m.ProviderBudgetsInvalid.Set(float64(s.Invalid))
			if monthly != nil {
				monthly.Observe(context.Background())
			}
		}})
	monthly = budgets.NewMonthly(set, budgets.NewMonths(rdb, "aisa:provider-"), cc, budgets.Always{}, m, log,
		budgets.MonthlyOptions{
			Location: loc, StatePrefix: cfg.Consul.Prefix + "/state/providers/",
			Limit: m.ProviderBudgetLimit, Spent: m.ProviderBudgetSpent, Label: "provider",
		})
	catalog := budgets.NewCatalog(cc, log, budgets.CatalogOptions{Service: "aisa-backend"})
	return budgets.NewCeiling(catalog, monthly, log), set, monthly, catalog
}

// observeBudgets puts the result of a load of the budgets into the metrics.
func observeBudgets(m *metrics.Metrics, s budgets.LoadStats) {
	if s.Err != nil {
		m.BudgetLoads.WithLabelValues(metrics.LoadError).Inc()
		return
	}
	m.BudgetLoads.WithLabelValues(metrics.LoadOK).Inc()
	m.Budgets.Set(float64(s.Valid))
	m.BudgetsInvalid.Set(float64(s.Invalid))
}

// kubeSources are what the Kubernetes objects are made from: the consumers' declarations, a
// signal of their changes, the hashes in Vault and the catalog in Consul.
type kubeSources struct {
	vault        *vault.Client
	consul       *consul.Client
	declarations *consumers.Declarations
	declared     <-chan struct{}
}

// runKubernetes starts the passes over the consumers' Secrets and the Model objects, on the
// replica that the leader's Lease elects.
func runKubernetes(ctx context.Context, cfg config.Config, src kubeSources, m *metrics.Metrics, log *slog.Logger) error {
	// client-go logs through klog, its leader election included: into aisa's log, as JSON.
	klog.SetSlogLogger(log)
	rc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	if rc, err = kube.ClientConfig(rc, m.Register); err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	k8s, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	k := cfg.Kubernetes
	obs := newKubeObserver(m)
	keys := kube.NewKeys(src.vault, k8s, src.declarations, log, kube.KeysOptions{
		Mount: cfg.Vault.KVMount, Prefix: cfg.Vault.Prefix, BaseURL: k.ProxyURL, Install: k.Namespace,
		Rotation: k.Rotation, Grace: k.Grace, OnIssue: obs.issued, OnPass: obs.secrets,
	})
	models := kube.NewModels(src.consul, dyn, log, kube.ModelsOptions{
		Service:      "aisa-backend",
		Descriptions: consul.KVPrefix{Client: src.consul, Prefix: cfg.Consul.Prefix + "/models/", What: "model descriptions"},
		Endpoint:     k.ProxyURL,
		Install:      k.Namespace,
		EmptyGrace:   k.EmptyGrace,
		OnPass:       obs.models,
	})
	go kube.Run(ctx, k8s, log, kube.RunOptions{
		Namespace: k.Namespace, Identity: k.PodName, Lease: k.Lease, Resync: k.Resync, Trigger: src.declared,
		OnLeader: obs.leader,
	},
		kube.Pass{Name: metrics.KubeSecrets, Run: keys.Reconcile, Done: obs.passed(metrics.KubeSecrets)},
		kube.Pass{Name: metrics.KubeModels, Run: models.Reconcile, Done: obs.passed(metrics.KubeModels)},
	)
	return nil
}

// kubeObserver puts what the passes over the Kubernetes objects report into the metrics.
type kubeObserver struct{ m *metrics.Metrics }

// newKubeObserver starts the series of fixed labels at 0, so that the first increase shows.
func newKubeObserver(m *metrics.Metrics) kubeObserver {
	for _, reason := range []string{kube.IssueNew, kube.IssueMissing, kube.IssueUnknownKey, kube.IssueRotation} {
		m.ConsumerKeysIssued.WithLabelValues(reason)
	}
	for _, kind := range []string{metrics.KubeSecrets, metrics.KubeModels} {
		m.KubePasses.WithLabelValues(kind, metrics.LoadOK)
		m.KubePasses.WithLabelValues(kind, metrics.LoadError)
	}
	return kubeObserver{m: m}
}

func (o kubeObserver) issued(reason string) { o.m.ConsumerKeysIssued.WithLabelValues(reason).Inc() }

func (o kubeObserver) secrets(states map[string]int) {
	for state, n := range states {
		o.m.ConsumerSecrets.WithLabelValues(state).Set(float64(n))
	}
}

func (o kubeObserver) models(s kube.ModelStats) {
	o.m.Models.Set(float64(s.Models))
	o.m.ModelsAvailable.Set(float64(s.Available))
	o.m.ModelConflicts.Set(float64(s.Conflicts))
	o.m.ModelDescriptionsInvalid.Set(float64(s.InvalidDescriptions))
}

// leader marks this replica as the one that writes, or not. A replica that stops clears what its
// passes reported, so that only the leader's state is exported, and a fixed problem does not
// stay in an old leader's gauges.
func (o kubeObserver) leader(on bool) {
	if on {
		o.m.KubeLeader.Set(1)
		return
	}
	o.m.KubeLeader.Set(0)
	o.m.ConsumerSecrets.Reset()
	o.m.Models.Set(0)
	o.m.ModelsAvailable.Set(0)
	o.m.ModelConflicts.Set(0)
	o.m.ModelDescriptionsInvalid.Set(0)
}

// passed returns the Done of a kind of pass.
func (o kubeObserver) passed(kind string) func(error) {
	return func(err error) {
		result := metrics.LoadOK
		if err != nil {
			result = metrics.LoadError
		}
		o.m.KubePasses.WithLabelValues(kind, result).Inc()
	}
}

// observeDeclarations puts the result of a load of the consumer declarations into the metrics.
func observeDeclarations(m *metrics.Metrics, s consumers.DeclarationStats) {
	if s.Err != nil {
		m.ConsumerDeclarationLoads.WithLabelValues(metrics.LoadError).Inc()
		return
	}
	m.ConsumerDeclarationLoads.WithLabelValues(metrics.LoadOK).Inc()
	m.ConsumerDeclarations.Set(float64(s.Valid))
	m.ConsumerDeclarationsInvalid.Set(float64(s.Invalid))
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

// observePrices puts the result of a load of the model prices into the metrics.
func observePrices(m *metrics.Metrics, s pricing.LoadStats) {
	if s.Err != nil {
		m.ModelPriceLoads.WithLabelValues(metrics.LoadError).Inc()
		return
	}
	m.ModelPriceLoads.WithLabelValues(metrics.LoadOK).Inc()
	m.ModelPrices.Set(float64(s.Valid))
	m.ModelPricesInvalid.Set(float64(s.Invalid))
}

// routes registers the contract endpoints and optional admin interface.
func routes(srv *server.Server, decision, usage, renders, adm http.Handler) {
	srv.Handle("POST /v1/decide", decision)
	srv.Handle("GET /v1/decide", decision)
	srv.Handle("POST /v1/usage", usage)
	srv.Handle("POST /v1/render", renders)
	if adm != nil {
		srv.Handle("/admin/", adm)
		srv.Handle("GET /admin", adm)
	}
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
