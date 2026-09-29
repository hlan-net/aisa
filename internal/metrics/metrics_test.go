package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestContractMetrics(t *testing.T) {
	m := New("v1.2.3")
	m.Requests.WithLabelValues("chat-ui", "qwen3", "ollama-1", "200").Inc()
	m.Tokens.WithLabelValues("chat-ui", "qwen3", "ollama-1", DirectionPrompt).Add(21)
	m.Tokens.WithLabelValues("chat-ui", "qwen3", "ollama-1", DirectionCompletion).Add(32)
	m.Cost.WithLabelValues("chat-ui", "qwen3", "EUR").Add(0.5)
	m.BudgetLimit.WithLabelValues("chat-ui", "2026-09").Set(25)
	m.BudgetSpent.WithLabelValues("chat-ui", "2026-09").Set(0.5)
	m.Latency.WithLabelValues("qwen3", "ollama-1").Observe(12)
	m.TTFT.WithLabelValues("qwen3", "ollama-1").Observe(0.8)
	m.Decisions.WithLabelValues("chat-ui", ResultAllow).Inc()
	m.UsageEvents.WithLabelValues(EventAccepted).Inc()

	// The names of contract 4. A renamed metric breaks dashboards and alerts.
	for _, name := range []string{
		"aisa_requests_total", "aisa_tokens_total", "aisa_cost_total", "aisa_budget_limit",
		"aisa_budget_spent", "aisa_latency_seconds", "aisa_ttft_seconds", "aisa_decisions_total",
		"aisa_usage_events_total",
		"aisa_build_info",
	} {
		n, err := testutil.GatherAndCount(m.Gatherer(), name)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("%s is not exported", name)
		}
	}

	want := `
# HELP aisa_tokens_total Tokens accounted by aisa.
# TYPE aisa_tokens_total counter
aisa_tokens_total{backend="ollama-1",consumer="chat-ui",direction="completion",model="qwen3"} 32
aisa_tokens_total{backend="ollama-1",consumer="chat-ui",direction="prompt",model="qwen3"} 21
`
	if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(want), "aisa_tokens_total"); err != nil {
		t.Error(err)
	}
}

func TestInstancesAreIndependent(t *testing.T) {
	a, b := New("a"), New("b")
	a.Decisions.WithLabelValues("chat-ui", ResultDenyQuota).Inc()
	if n, _ := testutil.GatherAndCount(b.Gatherer(), "aisa_decisions_total"); n != 0 {
		t.Errorf("an observation in one instance shows in another: %d series", n)
	}
}

func TestHandler(t *testing.T) {
	m := New("v1.2.3")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`aisa_build_info{version="v1.2.3"} 1`, "go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("the output lacks %q", want)
		}
	}
}
