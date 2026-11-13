# Grafana dashboards

[`aisa.json`](./aisa.json) is the Grafana dashboard for [`aisa`](../docs/concepts/ARCHITECTURE.md), covering the seven sections described in [`docs/features/usage-metrics.md`](../docs/features/usage-metrics.md#grafana-dashboard):

1. **Tokens per model** — stacked prompt and completion token rates, overall and split by consumer (`aisa_tokens_total`).
2. **Requests and decisions** — forward-auth outcomes (`allow`, `downgrade`, `deny_quota`, `deny_budget`, `deny_provider_budget`, `deny_auth`, `no_quota`, `invalid`, `unknown_model`, `unavailable`) from `aisa_decisions_total`, plus `POST /v1/decide` total and per-stage (`consumer`, `quota`) latency (`aisa_decide_duration_seconds`, `aisa_decide_stage_duration_seconds`).
3. **Latency** — p50 and p95 end-to-end request duration (`aisa_latency_seconds`) and time to first token (`aisa_ttft_seconds`) per model and backend.
4. **This month (budgets and cost)** — monthly consumer token budget (`aisa_budget_spent`, `aisa_budget_limit`), monthly provider-account token budget (`aisa_provider_budget_spent`, `aisa_provider_budget_limit`), and cumulative cost in currency (`aisa_cost_total`).
5. **Backends** — model and backend availability (`aisa_models_available`, `aisa_models`, `aisa_config_backends`, and Consul's `consul_catalog_service_node_healthy{service_name="aisa-backend"}`), request rate by backend and HTTP status (`aisa_requests_total`), and active streams from the proxy (`apisix_llm_active_connections`).
6. **Ledger consistency** — 1h ratio of `aisa_tokens_total` to gateway-reported tokens (`apisix_llm_prompt_tokens + apisix_llm_completion_tokens`), share of HTTP 200 responses with missing usage (`aisa_usage_missing_total`), and exact vs byte-estimated token rates (`aisa_tokens_estimated_total`).
7. **Top consumers** — table of total, prompt, and completion tokens, cumulative cost, and local (zero-price) vs cloud (priced) token share.

Primary panels query `aisa_*` metrics so that replacing the proxy does not break monitoring; gateway-native (`apisix_llm_*`) and Consul (`consul_catalog_*`) metrics are used only for active stream counts and cross-checking.

## Importing into Grafana

### Grafana UI

Import [`aisa.json`](./aisa.json) under **Dashboards → New → Import** and select your Prometheus data source.

### kube-prometheus-stack sidecar

With `kube-prometheus-stack`, label a ConfigMap containing `aisa.json` so that the Grafana sidecar loads it automatically:

```bash
kubectl -n monitoring create configmap aisa-dashboard \
  --from-file=aisa.json=dashboards/aisa.json \
  --dry-run=client -o yaml |
  kubectl label --local -f - grafana_dashboard=1 -o yaml |
  kubectl apply -f -
```
