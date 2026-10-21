package kube

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

// Unthrottled turns off the Kubernetes client's own rate limit of 5 requests a second (burst
// 10), which delays nearly every call of a pass over a few models and consumers by about
// 200 ms (#70). The API server protects itself with API Priority and Fairness, on by default
// in every Kubernetes version the chart supports (1.29 and later).
func Unthrottled(rc *rest.Config) *rest.Config {
	rc = rest.CopyConfig(rc)
	rc.QPS = -1
	rc.RateLimiter = nil
	return rc
}

// ClientMetrics are the Kubernetes client's metrics, under the names client-go's own
// Prometheus adapter uses, so the dashboards written for those work.
type ClientMetrics struct {
	requests    *prometheus.CounterVec
	latency     *prometheus.HistogramVec
	rateLimiter *prometheus.HistogramVec
}

// clientBuckets: from a call to a nearby API server to one that waits for its timeout.
var clientBuckets = []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60}

// NewClientMetrics returns the metrics, not yet in use: Install puts them into client-go.
func NewClientMetrics() *ClientMetrics {
	return &ClientMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rest_client_requests_total",
			Help: "Requests of aisa's Kubernetes client, by status code, method and host.",
		}, []string{"code", "method", "host"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rest_client_request_duration_seconds",
			Help:    "Time of the requests of aisa's Kubernetes client, by verb and host.",
			Buckets: clientBuckets,
		}, []string{"verb", "host"}),
		rateLimiter: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rest_client_rate_limiter_duration_seconds",
			Help:    "Time requests of aisa's Kubernetes client waited for its own rate limiter, by verb and host.",
			Buckets: clientBuckets,
		}, []string{"verb", "host"}),
	}
}

// Collectors are the metrics, to register in aisa's registry.
func (c *ClientMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{c.requests, c.latency, c.rateLimiter}
}

var installOnce sync.Once

// Install makes client-go report into these metrics. client-go takes metrics once per
// process, so only the first call has an effect.
func (c *ClientMetrics) Install() {
	installOnce.Do(func() {
		clientmetrics.Register(clientmetrics.RegisterOpts{
			RequestResult:      resultMetric{c.requests},
			RequestLatency:     latencyMetric{c.latency},
			RateLimiterLatency: latencyMetric{c.rateLimiter},
		})
	})
}

type resultMetric struct{ v *prometheus.CounterVec }

func (m resultMetric) Increment(_ context.Context, code, method, host string) {
	m.v.WithLabelValues(code, method, host).Inc()
}

// latencyMetric drops the URL's path: one series per object name would grow with the catalog.
type latencyMetric struct{ v *prometheus.HistogramVec }

func (m latencyMetric) Observe(_ context.Context, verb string, u url.URL, d time.Duration) {
	m.v.WithLabelValues(verb, u.Host).Observe(d.Seconds())
}
