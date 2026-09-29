# Usage Metrics

## Concept

aisa keeps the books: every request is accounted per consumer, model and backend, as tokens and as cost. It exports normalized metrics, so dashboards and alerts are the **same for every gateway adapter**.

## Primary metrics: aisa (`aisa_*`)

Full list in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md#4-metrics-output-of-aisa):

- `aisa_requests_total`, `aisa_tokens_total{direction}`, `aisa_cost_total`
- `aisa_budget_limit`, `aisa_budget_spent`
- `aisa_latency_seconds`, `aisa_ttft_seconds`
- `aisa_decisions_total{result}`: `allow`, `deny_auth` (unknown key, under the consumer `unknown`), `invalid` (no model), `deny_quota`, `deny_budget`, `downgrade`

Cost is computed in aisa from the Consul prices, so no Prometheus rules with hardcoded prices are needed.

Keep label cardinality in mind: `consumer` × `model` × `backend`. For large deployments, consumers can be aggregated into groups.

## Secondary metrics: the gateway

APISIX's [prometheus plugin](https://apisix.apache.org/docs/apisix/plugins/prometheus/) also exports `apisix_llm_prompt_tokens`, `apisix_llm_completion_tokens`, `apisix_llm_latency` (with time to first token) and `apisix_llm_active_connections`. They are scraped and used for:
- a **cross-check**: gateway token counts vs aisa's ledger. A mismatch means lost usage events.
- gateway internals: active streams and upstream errors.

No dashboard panel or alert depends only on gateway metrics, so swapping the adapter does not break monitoring.

## Grafana dashboard

Shipped as JSON in `dashboards/`:

1. **Tokens per model**, stacked prompt and completion, split by consumer
2. **Requests and decisions**: allow, deny_quota, deny_budget and downgrade over time
3. **Latency**: p50/p95 total and time to first token per model and backend
4. **Cost this month** per consumer, against the budget and what remains
5. **Backends**: Consul health of each `aisa-backend` and, from the adapter, active streams
6. **Ledger consistency**: aisa tokens vs gateway tokens (should be about 1.0)
7. **Top consumers** table: tokens, cost, and the local vs cloud share

## Alerts

Shipped as a `PrometheusRule`:

| Alert | Condition |
|---|---|
| `AisaBudgetSoftLimit` | `aisa_budget_spent / aisa_budget_limit > 0.8` |
| `AisaBudgetExhausted` | a consumer is being denied or downgraded because its budget is used up |
| `AisaBackendDown` | Consul health check for an `aisa-backend` is critical for more than 10 min |
| `AisaDown` | aisa's scrape target is down for 2 min (fail policies are now in effect) |
| `AisaUsageEventsLost` | the aisa/gateway token ratio < 0.95 over 1 h |
| `AisaSlowFirstToken` | p95 time to first token > 20 s for 15 min (model too large for the backend, or the backend is swapping) |
