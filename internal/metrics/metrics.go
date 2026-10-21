// Package metrics defines aisa's own metrics, the aisa_* series of contract 4 in
// docs/concepts/PROXY_CONTRACT.md. They are the same whatever gateway is in use.
package metrics

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Values of the direction label of aisa_tokens_total.
const (
	DirectionPrompt     = "prompt"
	DirectionCompletion = "completion"
)

// Values of the result label of aisa_decisions_total.
const (
	ResultAllow       = "allow"
	ResultDenyAuth    = "deny_auth"   // unknown, invalid or missing credential (401)
	ResultInvalid     = "invalid"     // no model in the request (400), or a body too large to look for it (413)
	ResultUnavailable = "unavailable" // aisa cannot verify credentials and fails closed (503)
	ResultDenyQuota   = "deny_quota"  // quota exhausted (429)
	ResultDenyBudget  = "deny_budget" // budget exhausted (429)
	ResultDowngrade   = "downgrade"
)

// Values of the stage label of aisa_decide_stage_duration_seconds.
const (
	StageConsumer = "consumer" // finding the consumer of the key; waits for a Vault reload on an unknown key
	StageQuota    = "quota"    // reading the consumer's token usage from Redis
)

// Values of the result label of aisa_usage_events_total.
const (
	EventAccepted  = "accepted"
	EventDuplicate = "duplicate"
	EventRejected  = "rejected"
)

// Values of the op label of aisa_quota_errors_total.
const (
	QuotaOpCheck  = "check"  // reading a consumer's usage for a decision
	QuotaOpCharge = "charge" // adding the tokens of a usage event
)

// Values of the reason label of aisa_usage_requests_rejected_total: why a whole usage request
// was rejected, with none of its events read.
const (
	UsageRequestMalformed  = "malformed"  // not a JSON object or array of that shape (400)
	UsageRequestTooLarge   = "too_large"  // body over the limit (413)
	UsageRequestUnreadable = "unreadable" // the body could not be read (400)
)

// UsageRequestReasons are all values of the reason label of aisa_usage_requests_rejected_total.
// Each series starts at 0.
var UsageRequestReasons = []string{UsageRequestMalformed, UsageRequestTooLarge, UsageRequestUnreadable}

// ConsumerUnknown is the consumer label of a decision without a known consumer, such as a
// rejected credential. Usage served without a decision is accounted under it too.
const ConsumerUnknown = "unknown"

// Metrics holds aisa's metrics and the registry they are registered in. Each instance has a
// registry of its own, so tests do not share state.
type Metrics struct {
	registry *prometheus.Registry

	// Requests counts requests by consumer, model, backend and status.
	Requests *prometheus.CounterVec
	// Tokens counts tokens by consumer, model, backend and direction.
	Tokens *prometheus.CounterVec
	// Cost sums the cost of requests by consumer, model and currency.
	Cost *prometheus.CounterVec
	// BudgetLimit and BudgetSpent are the budget of a consumer and what is used of it, by
	// consumer and period.
	BudgetLimit *prometheus.GaugeVec
	BudgetSpent *prometheus.GaugeVec
	// Latency and TTFT are the time a request took at the backend and the time to its first
	// token, by model and backend.
	Latency *prometheus.HistogramVec
	TTFT    *prometheus.HistogramVec
	// Decisions counts the answers of the decision API by consumer and result.
	Decisions *prometheus.CounterVec
	// DecideDuration is the time aisa takes to answer the decision API, by result, and
	// DecideStageDuration the part of it spent in each stage, by stage.
	DecideDuration      *prometheus.HistogramVec
	DecideStageDuration *prometheus.HistogramVec
	// UsageEvents counts usage events received by result (accepted, duplicate, rejected).
	UsageEvents *prometheus.CounterVec
	// UsageRequestsRejected counts usage requests rejected as a whole, by reason. Their events
	// are in no other metric, and the gateway's log sink may drop them after its retries.
	UsageRequestsRejected *prometheus.CounterVec
	// UsageMissing counts successful requests whose event has no token counts, by consumer,
	// model and backend: usage the gateway did not see, which aisa charges by estimate.
	UsageMissing *prometheus.CounterVec
	// TokensEstimated counts the estimated tokens of those requests, by consumer, model and
	// backend: charged to the consumer's quota when the request has one (a fail-open request,
	// under the consumer unknown, is estimated and charged to nobody). They are not in Tokens.
	TokensEstimated *prometheus.CounterVec

	// Consumers, ConsumerKeys and ConsumersUnreadable describe the consumers in memory: how
	// many can authenticate, how many key hashes they have, and how many Vault listed and aisa
	// could not read.
	Consumers           prometheus.Gauge
	ConsumerKeys        prometheus.Gauge
	ConsumersUnreadable prometheus.Gauge
	// ConsumerLoads counts the loads of the consumers by result, LoadOK or LoadError.
	ConsumerLoads *prometheus.CounterVec
	// ConsumersLoaded is when the consumers were last loaded, in seconds since the epoch.
	ConsumersLoaded prometheus.Gauge
	// ConsumerDeclarations and ConsumerDeclarationsInvalid count the consumers declared in
	// Consul whose declarations could and could not be read.
	ConsumerDeclarations        prometheus.Gauge
	ConsumerDeclarationsInvalid prometheus.Gauge
	// ConsumerDeclarationLoads counts the loads of the declarations by result, LoadOK or LoadError.
	ConsumerDeclarationLoads *prometheus.CounterVec

	// QuotaProfiles and QuotaProfilesInvalid count the quota profiles read from Consul that
	// could and could not be parsed.
	QuotaProfiles        prometheus.Gauge
	QuotaProfilesInvalid prometheus.Gauge
	// QuotaProfileLoads counts the loads of the quota profiles by result, LoadOK or LoadError.
	QuotaProfileLoads *prometheus.CounterVec
	// QuotaErrors counts failed calls to the quota counters in Redis, by op.
	QuotaErrors *prometheus.CounterVec

	// ModelPrices and ModelPricesInvalid count the model prices in Consul whose entries could
	// and could not be read.
	ModelPrices        prometheus.Gauge
	ModelPricesInvalid prometheus.Gauge
	// ModelPriceLoads counts the loads of the model prices by result, LoadOK or LoadError.
	ModelPriceLoads *prometheus.CounterVec

	// ConfigRenders counts the proxy's renders of the gateway config by result: promoted,
	// rejected or held. ConfigRendered is when a render was last promoted, in seconds since
	// the epoch, and ConfigBackends and ConfigModels are what that config holds. The three
	// gauges have no labels and are absent until the first promoted report since aisa
	// started (a rejected or held one leaves them absent): a restart forgets the config, and
	// an absent gauge says so where a zero would claim the gateway runs an empty one.
	ConfigRenders  *prometheus.CounterVec
	ConfigRendered *prometheus.GaugeVec
	ConfigBackends *prometheus.GaugeVec
	ConfigModels   *prometheus.GaugeVec

	// The objects aisa writes to the Kubernetes API (KUBERNETES_OBJECTS.md), kept by the
	// replica that holds the leader's Lease. KubeLeader is 1 on that replica.
	KubeLeader prometheus.Gauge
	// KubePasses counts the passes over the objects by kind (secrets or models) and result,
	// LoadOK or LoadError.
	KubePasses *prometheus.CounterVec
	// ConsumerSecrets counts the consumers that name a Secret by the state of the last pass:
	// ok, not_owned (a Secret of that name is not the consumer's), duplicate_target (another
	// consumer names it too) or error.
	ConsumerSecrets *prometheus.GaugeVec
	// ConsumerKeysIssued counts the keys issued, by reason: new, missing, unknown_key or
	// rotation.
	ConsumerKeysIssued *prometheus.CounterVec
	// Models and ModelsAvailable count the Model objects and those with a healthy backend;
	// ModelConflicts the models without an object because of a name; ModelDescriptionsInvalid
	// the descriptions in Consul that could not be read.
	Models                   prometheus.Gauge
	ModelsAvailable          prometheus.Gauge
	ModelConflicts           prometheus.Gauge
	ModelDescriptionsInvalid prometheus.Gauge
}

// Values of the kind label of aisa_kube_passes_total.
const (
	KubeSecrets = "secrets"
	KubeModels  = "models"
)

// Values of the result label of aisa_consumer_loads_total.
const (
	LoadOK    = "ok"
	LoadError = "error"
)

// Values of the result label of aisa_config_renders_total.
const (
	RenderPromoted = "promoted"
	RenderRejected = "rejected"
	RenderHeld     = "held"
)

// New returns aisa's metrics, registered in a new registry together with the Go runtime and
// process collectors and aisa_build_info for the given version.
func New(version string) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_requests_total",
			Help: "Requests accounted by aisa.",
		}, []string{"consumer", "model", "backend", "status"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_tokens_total",
			Help: "Tokens accounted by aisa.",
		}, []string{"consumer", "model", "backend", "direction"}),
		Cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_cost_total",
			Help: "Cost of the accounted requests, in the currency of the label.",
		}, []string{"consumer", "model", "currency"}),
		BudgetLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aisa_budget_limit",
			Help: "Budget of a consumer for the period.",
		}, []string{"consumer", "period"}),
		BudgetSpent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aisa_budget_spent",
			Help: "What a consumer has spent of its budget in the period.",
		}, []string{"consumer", "period"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "aisa_latency_seconds",
			Help: "Time a request took at the backend, including the whole stream.",
			// Local models on small hardware take minutes; the gateway ends a request after 600 s.
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600},
		}, []string{"model", "backend"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "aisa_ttft_seconds",
			Help: "Time to the first token of a response.",
			// A response that is not streamed has its first token at its end, so as for latency.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600},
		}, []string{"model", "backend"}),
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_decisions_total",
			Help: "Answers of the decision API.",
		}, []string{"consumer", "result"}),
		DecideDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aisa_decide_duration_seconds",
			Help:    "Time aisa took to answer the decision API, by result as in aisa_decisions_total.",
			Buckets: decideBuckets,
		}, []string{"result"}),
		DecideStageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aisa_decide_stage_duration_seconds",
			Help:    "Time a decision spent in a stage: consumer (finding the key, a Vault reload on a miss) or quota (Redis).",
			Buckets: decideBuckets,
		}, []string{"stage"}),
		UsageEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_usage_events_total",
			Help: "Usage events received by aisa, by result.",
		}, []string{"result"}),
		UsageRequestsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_usage_requests_rejected_total",
			Help: "Usage requests rejected as a whole, with none of their events read, by reason.",
		}, []string{"reason"}),
		UsageMissing: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_usage_missing_total",
			Help: "Successful requests reported without token counts: the gateway did not see their usage.",
		}, []string{"consumer", "model", "backend"}),
		TokensEstimated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_tokens_estimated_total",
			Help: "Estimated tokens of successful requests reported without token counts, charged to the consumer's quota when there is one; not in aisa_tokens_total.",
		}, []string{"consumer", "model", "backend"}),
		Consumers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumers",
			Help: "Consumers with key hashes from Vault in memory, declared in Consul or not; only declared ones authenticate (aisa_consumer_declarations).",
		}),
		ConsumerKeys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumer_keys",
			Help: "Key hashes from Vault in memory, of the consumers in aisa_consumers.",
		}),
		ConsumersUnreadable: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumers_unreadable",
			Help: "Consumers that Vault listed and aisa could not read at the last load.",
		}),
		ConsumerLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_consumer_loads_total",
			Help: "Loads of the consumers' key hashes from Vault.",
		}, []string{"result"}),
		ConsumerDeclarations: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumer_declarations",
			Help: "Consumers declared in Consul KV whose declarations can be read.",
		}),
		ConsumerDeclarationsInvalid: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumer_declarations_invalid",
			Help: "Consumers declared in Consul KV whose declarations cannot be read; their keys are answered 503.",
		}),
		ConsumerDeclarationLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_consumer_declaration_loads_total",
			Help: "Reads of the consumer declarations from Consul KV.",
		}, []string{"result"}),
		ConsumersLoaded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumers_loaded_timestamp_seconds",
			Help: "When the consumers were last loaded from Vault.",
		}),
		QuotaProfiles: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_quota_profiles",
			Help: "Quota profiles in memory.",
		}),
		QuotaProfilesInvalid: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_quota_profiles_invalid",
			Help: "Quota profiles in Consul that aisa could not parse at the last load.",
		}),
		QuotaProfileLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_quota_profile_loads_total",
			Help: "Loads of the quota profiles from Consul.",
		}, []string{"result"}),
		QuotaErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_quota_errors_total",
			Help: "Failed reads and writes of the quota counters in Redis.",
		}, []string{"op"}),
		ModelPrices: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_model_prices",
			Help: "Models with a valid price in Consul KV.",
		}),
		ModelPricesInvalid: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_model_prices_invalid",
			Help: "Model prices in Consul KV that cannot be read.",
		}),
		ModelPriceLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_model_price_loads_total",
			Help: "Reads of the model prices from Consul KV.",
		}, []string{"result"}),
		ConfigRenders: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_config_renders_total",
			Help: "Renders of the gateway config reported by the proxy, by result: promoted, rejected or held.",
		}, []string{"result"}),
		ConfigRendered: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aisa_config_rendered_timestamp_seconds",
			Help: "When the proxy last promoted a rendered gateway config, in seconds since the epoch; absent until the first promoted report since aisa started.",
		}, nil),
		ConfigBackends: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aisa_config_backends",
			Help: "Backends in the gateway config the proxy last promoted; absent until the first promoted report since aisa started.",
		}, nil),
		ConfigModels: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aisa_config_models",
			Help: "Models with a route in the gateway config the proxy last promoted; absent until the first promoted report since aisa started.",
		}, nil),
		KubeLeader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_kube_leader",
			Help: "1 on the replica that holds the leader's Lease and writes the Kubernetes objects, else 0.",
		}),
		KubePasses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_kube_passes_total",
			Help: "Passes over the Kubernetes objects aisa writes, by kind (secrets, models) and result.",
		}, []string{"kind", "result"}),
		ConsumerSecrets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aisa_consumer_secrets",
			Help: "Consumers that name a Secret, by the state of the last pass: ok, not_owned, duplicate_target or error.",
		}, []string{"state"}),
		ConsumerKeysIssued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_consumer_keys_issued_total",
			Help: "Keys issued to consumers, by reason: new, missing, unknown_key or rotation.",
		}, []string{"reason"}),
		Models: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_models",
			Help: "Model objects this install keeps.",
		}),
		ModelsAvailable: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_models_available",
			Help: "Model objects with at least one healthy backend.",
		}),
		ModelConflicts: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_model_conflicts",
			Help: "Models on offer without a Model object: their names derive to one object name, or an object of that name is not this install's.",
		}),
		ModelDescriptionsInvalid: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_model_descriptions_invalid",
			Help: "Model descriptions in Consul KV that cannot be read; their models get the default description.",
		}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aisa_build_info",
		Help: "The running build; the value is always 1.",
	}, []string{"version"})
	buildInfo.WithLabelValues(version).Set(1)

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		m.Requests, m.Tokens, m.Cost, m.BudgetLimit, m.BudgetSpent, m.Latency, m.TTFT, m.Decisions,
		m.DecideDuration, m.DecideStageDuration,
		m.UsageEvents, m.UsageRequestsRejected, m.UsageMissing, m.TokensEstimated,
		m.ConfigRenders, m.ConfigRendered, m.ConfigBackends, m.ConfigModels,
		m.Consumers, m.ConsumerKeys, m.ConsumersUnreadable, m.ConsumerLoads, m.ConsumersLoaded,
		m.ConsumerDeclarations, m.ConsumerDeclarationsInvalid, m.ConsumerDeclarationLoads,
		m.QuotaProfiles, m.QuotaProfilesInvalid, m.QuotaProfileLoads, m.QuotaErrors,
		m.ModelPrices, m.ModelPricesInvalid, m.ModelPriceLoads,
		m.KubeLeader, m.KubePasses, m.ConsumerSecrets, m.ConsumerKeysIssued,
		m.Models, m.ModelsAvailable, m.ModelConflicts, m.ModelDescriptionsInvalid,
	)
	// The reasons are fixed: start each at 0, so the first rejection is an increase that an
	// alert on increase() or rate() can see.
	for _, reason := range UsageRequestReasons {
		m.UsageRequestsRejected.WithLabelValues(reason)
	}
	for _, result := range []string{RenderPromoted, RenderRejected, RenderHeld} {
		m.ConfigRenders.WithLabelValues(result)
	}
	return m
}

// decideBuckets cover a decision from memory (well under a millisecond) to one that waits on
// Vault or Redis until their timeouts (seconds).
var decideBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Gatherer gives access to the registered metrics, for tests.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.registry }

// Register adds collectors of another package to aisa's registry, such as those of the
// Kubernetes client.
func (m *Metrics) Register(cs ...prometheus.Collector) error {
	for _, c := range cs {
		if err := m.registry.Register(c); err != nil {
			return fmt.Errorf("register metrics: %w", err)
		}
	}
	return nil
}
