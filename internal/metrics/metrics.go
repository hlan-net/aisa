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
	ResultAllow      = "allow"
	ResultDenyQuota  = "deny_quota"
	ResultDenyBudget = "deny_budget"
	ResultDowngrade  = "downgrade"
)

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
}

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
			Name:    "aisa_ttft_seconds",
			Help:    "Time to the first token of a response.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120},
		}, []string{"model", "backend"}),
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aisa_decisions_total",
			Help: "Answers of the decision API.",
		}, []string{"consumer", "result"}),
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
	)
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Gatherer gives access to the registered metrics, for tests.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.registry }
