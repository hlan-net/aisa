# Usage Metrics

## Concept

aisa keeps the books: every request is accounted per consumer, model and backend, as tokens and as cost. It exports normalized metrics, so dashboards and alerts are the **same for every gateway adapter**.

## Primary metrics: aisa (`aisa_*`)

Full list in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md#4-metrics-output-of-aisa):

- `aisa_requests_total`, `aisa_tokens_total{direction}`, `aisa_cost_total`
- `aisa_budget_limit`, `aisa_budget_spent`
- `aisa_latency_seconds`, `aisa_ttft_seconds`
- `aisa_usage_events_total{result}` (`accepted`, `duplicate`, `rejected`) and `aisa_usage_requests_rejected_total{reason}` for usage requests rejected as a whole (`malformed`, `too_large`, `unreadable`)
- `aisa_usage_missing_total{consumer,model,backend}`: successful requests whose event has no token counts, because the backend did not stream usage or the client disconnected mid-stream. Their tokens are in no other metric and are not charged to quotas ([#12](https://github.com/hlan-net/aisa/issues/12))
- `aisa_decisions_total{result}`: `allow`, `deny_auth` (unknown key, under the consumer `unknown`), `invalid` (no model), `unavailable` (aisa cannot verify credentials, under the consumer `unknown`, or cannot know a consumer's token quota, and answers 503), `deny_quota`, `deny_budget`, `downgrade`

Cost is computed in aisa from the Consul prices, so no Prometheus rules with hardcoded prices are needed.

aisa also reports on its own state. These are not part of the adapter contract:

| Metric | Meaning |
|---|---|
| `aisa_build_info{version}` | The running build |
| `aisa_consumers`, `aisa_consumer_keys` | Consumers in memory that can authenticate, and their key hashes |
| `aisa_consumers_unreadable` | Consumers that Vault listed and aisa could not read at the last load |
| `aisa_consumer_loads_total{result}` | Loads of the consumers from Vault, `ok` or `error` |
| `aisa_consumers_loaded_timestamp_seconds` | When the consumers were last loaded |
| `aisa_quota_profiles`, `aisa_quota_profiles_invalid` | Quota profiles read from Consul, and those that could not be parsed |
| `aisa_quota_profile_loads_total{result}` | Reads of the quota profiles from Consul, `ok` or `error` |
| `aisa_quota_errors_total{op}` | Failed reads (`check`) and writes (`charge`) of the quota counters in Redis; while reads fail, quotas are not enforced |

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
6. **Ledger consistency**: aisa tokens vs gateway tokens (should be about 1.0), and the share of successful requests with missing usage per backend
7. **Top consumers** table: tokens, cost, and the local vs cloud share

## Alerts

Shipped as a `PrometheusRule`:

| Alert | Condition |
|---|---|
| `AisaBudgetSoftLimit` | `aisa_budget_spent / aisa_budget_limit > 0.8` |
| `AisaBudgetExhausted` | a consumer is being denied or downgraded because its budget is used up |
| `AisaBackendDown` | Consul health check for an `aisa-backend` is critical for more than 10 min |
| `AisaDown` | aisa's scrape target is down for 2 min (fail policies are now in effect) |
| `AisaConsumersStale` | the consumers were last loaded more than 5 min ago (aisa fails closed after 15 min) |
| `AisaConsumersUnreadable` | `aisa_consumers_unreadable > 0` for 5 min: a consumer in Vault cannot be read and loses its access after 15 min |
| `AisaQuotaProfileInvalid` | `aisa_quota_profiles_invalid > 0`: the consumers of a broken profile get 503 |
| `AisaQuotaCountersFailing` | `aisa_quota_errors_total` increases for 5 min: Redis is unreachable and token quotas are not enforced |
| `AisaUsageEventsLost` | the aisa/gateway token ratio < 0.95 over 1 h |
| `AisaUsageMissing` | `aisa_usage_missing_total` increases for 15 min on a backend: its requests are not counted towards quotas. The gateway's own token metrics miss the same usage, so `AisaUsageEventsLost` cannot see it |
| `AisaUsageEventsRejected` | `aisa_usage_events_total{result="rejected"}` increases for 15 min: the adapter sends events aisa cannot use. They are answered 200, so the log sink does not retry them and they are lost |
| `AisaUsageRequestsRejected` | `aisa_usage_requests_rejected_total` increases: the adapter sends usage aisa cannot read, and the log sink drops it after its retries |
| `AisaSlowFirstToken` | p95 time to first token > 20 s for 15 min (model too large for the backend, or the backend is swapping) |
