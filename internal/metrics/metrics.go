// Package metrics defines aisa's own metrics, the aisa_* series of contract 4 in
// docs/concepts/ADAPTER_CONTRACT.md. They are the same whatever gateway is in use.
package metrics

import (
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

// Values of the result label of aisa_usage_events_total.
const (
	EventAccepted  = "accepted"
	EventDuplicate = "duplicate"
	EventRejected  = "rejected"
)

// Values of the reason label of aisa_usage_requests_rejected_total: why a whole usage request
// was rejected, with none of its events read.
const (
	UsageRequestMalformed  = "malformed"  // not a JSON object or array of that shape (400)
	UsageRequestTooLarge   = "too_large"  // body over the limit (413)
	UsageRequestUnreadable = "unreadable" // the body could not be read (400)
	UsageRequestMethod     = "method"     // not a POST (405)
)

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
	// UsageEvents counts usage events received by result (accepted, duplicate, rejected).
	UsageEvents *prometheus.CounterVec
	// UsageRequestsRejected counts usage requests rejected as a whole, by reason. Their events
	// are in no other metric, and the gateway's log sink may drop them after its retries.
	UsageRequestsRejected *prometheus.CounterVec

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
}

// Values of the result label of aisa_consumer_loads_total.
const (
	LoadOK    = "ok"
	LoadError = "error"
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
		UsageEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_usage_events_total",
			Help: "Usage events received by aisa, by result.",
		}, []string{"result"}),
		UsageRequestsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_usage_requests_rejected_total",
			Help: "Usage requests rejected as a whole, with none of their events read, by reason.",
		}, []string{"reason"}),
		Consumers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumers",
			Help: "Consumers in memory that can authenticate.",
		}),
		ConsumerKeys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumer_keys",
			Help: "Key hashes of the consumers in memory.",
		}),
		ConsumersUnreadable: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumers_unreadable",
			Help: "Consumers that Vault listed and aisa could not read at the last load.",
		}),
		ConsumerLoads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_consumer_loads_total",
			Help: "Loads of the consumers from Vault.",
		}, []string{"result"}),
		ConsumersLoaded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aisa_consumers_loaded_timestamp_seconds",
			Help: "When the consumers were last loaded from Vault.",
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
		m.UsageEvents, m.UsageRequestsRejected,
		m.Consumers, m.ConsumerKeys, m.ConsumersUnreadable, m.ConsumerLoads, m.ConsumersLoaded,
	)
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Gatherer gives access to the registered metrics, for tests.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.registry }
