package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestUnthrottledIsNotRateLimited sends 50 requests, about what a pass over a few dozen models
// takes. client-go's default limit (5 a second, burst 10) needs 8 s for them.
func TestUnthrottledIsNotRateLimited(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"aisa"}}`))
	}))
	defer api.Close()

	k8s, err := kubernetes.NewForConfig(Unthrottled(&rest.Config{Host: api.URL}))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for range 50 {
		if _, err := k8s.CoreV1().Namespaces().Get(context.Background(), "aisa", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("50 requests took %v: the client is rate limited", d)
	}
}

func TestUnthrottledLeavesTheConfigAlone(t *testing.T) {
	rc := &rest.Config{Host: "https://kubernetes.default.svc", QPS: 5, Burst: 10}
	if got := Unthrottled(rc); got.QPS != -1 {
		t.Errorf("QPS = %v, want -1", got.QPS)
	}
	if rc.QPS != 5 {
		t.Errorf("the given config was changed: QPS = %v", rc.QPS)
	}
}

func TestClientMetrics(t *testing.T) {
	c := NewClientMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c.Collectors()...)

	resultMetric{c.requests}.Increment(context.Background(), "200", "GET", "10.0.0.1:443")
	u := mustURL(t, "https://10.0.0.1:443/apis/aisa.hlan.net/v1alpha1/models/qwen3-61b6a805")
	latencyMetric{c.latency}.Observe(context.Background(), "GET", u, 20*time.Millisecond)
	latencyMetric{c.rateLimiter}.Observe(context.Background(), "GET", u, 200*time.Millisecond)

	if n := testutil.ToFloat64(c.requests.WithLabelValues("200", "GET", "10.0.0.1:443")); n != 1 {
		t.Errorf("rest_client_requests_total = %v, want 1", n)
	}
	for _, name := range []string{"rest_client_request_duration_seconds", "rest_client_rate_limiter_duration_seconds"} {
		if n, err := testutil.GatherAndCount(reg, name); err != nil || n != 1 {
			t.Errorf("%s: %d series, err %v; want 1 series without the path", name, n, err)
		}
	}
}

func mustURL(t *testing.T, s string) url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return *u
}

// TestClientConfigCountsRequests makes a request with a client of ClientConfig and finds it in
// the registry the metrics were registered in, and in a registry given them later: client-go
// reports into one set per process, which every registration must share.
func TestClientConfigCountsRequests(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"aisa"}}`))
	}))
	defer api.Close()
	host := strings.TrimPrefix(api.URL, "http://")

	reg := prometheus.NewRegistry()
	rc, err := ClientConfig(&rest.Config{Host: api.URL}, registerIn(reg))
	if err != nil {
		t.Fatal(err)
	}
	if rc.QPS != -1 {
		t.Errorf("QPS = %v, want -1", rc.QPS)
	}
	k8s, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k8s.CoreV1().Namespaces().Get(context.Background(), "aisa", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := requestsTo(t, reg, host); n != 1 {
		t.Errorf("rest_client_requests_total{host=%q} = %v after one request, want 1", host, n)
	}
	if n, err := testutil.GatherAndCount(reg, "rest_client_request_duration_seconds"); err != nil || n == 0 {
		t.Errorf("rest_client_request_duration_seconds: %d series, err %v; want the request's", n, err)
	}

	later := prometheus.NewRegistry()
	if _, err := ClientConfig(&rest.Config{Host: api.URL}, registerIn(later)); err != nil {
		t.Fatal(err)
	}
	if n := requestsTo(t, later, host); n != 1 {
		t.Errorf("rest_client_requests_total{host=%q} = %v in a registry given the metrics later, want 1", host, n)
	}
}

// registerIn registers collectors in reg, as metrics.Register does in aisa's registry.
func registerIn(reg prometheus.Registerer) func(...prometheus.Collector) error {
	return func(cs ...prometheus.Collector) error {
		for _, c := range cs {
			if err := reg.Register(c); err != nil {
				return err
			}
		}
		return nil
	}
}

// requestsTo is rest_client_requests_total for a host in reg, over every code and method.
func requestsTo(t *testing.T, reg prometheus.Gatherer, host string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, f := range families {
		if f.GetName() != "rest_client_requests_total" {
			continue
		}
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetName() == "host" && l.GetValue() == host {
					n += s.GetCounter().GetValue()
				}
			}
		}
	}
	return n
}

func TestClientConfigFailsWithoutItsMetrics(t *testing.T) {
	_, err := ClientConfig(&rest.Config{}, func(...prometheus.Collector) error { return errors.New("duplicate") })
	if err == nil {
		t.Fatal("want an error when the metrics cannot be registered")
	}
}
