# Usage Metrics

## Concept

aisa keeps the books: every request is accounted per consumer, model and backend, as tokens and as cost. It exports normalized metrics, so dashboards and alerts are the **same for every proxy**.

## Primary metrics: aisa (`aisa_*`)

Full list in [`PROXY_CONTRACT.md`](../concepts/PROXY_CONTRACT.md#4-metrics-output-of-aisa):

- `aisa_requests_total`, `aisa_tokens_total{direction}`, `aisa_cost_total`
- `aisa_budget_limit`, `aisa_budget_spent`, in tokens
- `aisa_latency_seconds`, `aisa_ttft_seconds`
- `aisa_usage_events_total{result}` (`accepted`, `duplicate`, `rejected`) and `aisa_usage_requests_rejected_total{reason}` for usage requests rejected as a whole (`malformed`, `too_large`, `unreadable`)
- `aisa_usage_missing_total{consumer,model,backend}`: successful requests whose event has no token counts, because the backend did not stream usage or the client disconnected mid-stream. aisa charges them to the quota by estimate: the consumer's recent average for the model, else the model's across consumers, else `AISA_LEDGER_ESTIMATE_DEFAULT_TOKENS` (0: nothing). The estimates are in `aisa_tokens_estimated_total{consumer,model,backend}`, apart from the exact `aisa_tokens_total`, so a dashboard shows how much of the usage is a guess ([#12](https://github.com/hlan-net/aisa/issues/12))
- `aisa_decisions_total{result}`: `allow`, `deny_auth` (unknown key, under the consumer `unknown`), `invalid` (no model), `no_quota` (the consumer has no quota profile and there is no default, 403), `unavailable` (aisa cannot verify credentials, under the consumer `unknown`, or cannot know a consumer's token quota, and answers 503), `deny_quota`, `deny_budget`, `downgrade`
- `aisa_decide_duration_seconds{result}`: how long aisa took to answer `/v1/decide`, by the same results, and `aisa_decide_stage_duration_seconds{stage}` for the part of it spent finding the consumer (`consumer`; in memory, except for an unknown key, which waits for a reload from Vault) and reading its token usage from Redis (`quota`). This is aisa's share of the gateway's overhead, measured the same way whatever the proxy; whether a gateway-side decision cache would help is read from it. A rising p95 with a flat `consumer` stage points at Redis ([#71](https://github.com/hlan-net/aisa/issues/71))
- `aisa_config_renders_total{result}`: the proxy's renders of the gateway config, as it reports them: `promoted`, `rejected` (the check failed; the previous config stays) or `held` (a config without model routes waits for the grace period)
- `aisa_config_rendered_timestamp_seconds`, `aisa_config_backends`, `aisa_config_models`: when the proxy last promoted a rendered config, and the backends and the models with a route in it. Absent until the first promoted report since aisa started: a restart forgets the config, and an absent gauge says so where a zero would claim an empty one; the proxy's pod sends a report aisa did not take again, every 30 s

aisa serves `/metrics` on its own address when `AISA_METRICS_ADDR` is set (the chart sets `:9090`, with a Service `<release>-metrics` that its ServiceMonitor scrapes); `AISA_ADDR` then serves only the proxy's endpoints, `/v1/decide`, `/v1/usage` and `/v1/render`. Allow Prometheus the metrics port only: a scraper that reaches `/v1/usage` could send usage events that charge consumers' quotas ([#46](https://github.com/hlan-net/aisa/issues/46)).

Cost is computed in aisa from the Consul prices, so no Prometheus rules with hardcoded prices are needed.

aisa also reports on its own state. These are not part of the proxy contract:

| Metric | Meaning |
|---|---|
| `aisa_build_info{version}` | The running build |
| `aisa_consumers`, `aisa_consumer_keys` | Consumers with key hashes from Vault in memory, and those hashes; a consumer among them authenticates only when Consul declares it (`aisa_consumer_declarations`) |
| `aisa_consumers_unreadable` | Consumers that Vault listed and aisa could not read at the last load |
| `aisa_consumer_loads_total{result}` | Loads of the consumers' key hashes from Vault, `ok` or `error` |
| `aisa_consumer_declarations`, `aisa_consumer_declarations_invalid` | Consumers declared in Consul KV, and those whose declarations could not be parsed (their keys are answered 503) |
| `aisa_consumer_declaration_loads_total{result}` | Reads of the consumer declarations from Consul, `ok` or `error` |
| `aisa_consumers_loaded_timestamp_seconds` | When the consumers were last loaded |
| `aisa_quota_profiles`, `aisa_quota_profiles_invalid` | Quota profiles read from Consul, and those that could not be parsed |
| `aisa_quota_profiles_missing{source}` | Quota profiles that are named and not in Consul: by the default profile (`default`, 0 or 1) or by the consumers' declarations (`consumer`, one per consumer); their consumers get 503 |
| `aisa_quota_profile_loads_total{result}` | Reads of the quota profiles from Consul, `ok` or `error` |
| `aisa_quota_errors_total{op}` | Failed reads (`check`) and writes (`charge`) of the quota counters in Redis; while reads fail, quotas are not enforced |
| `aisa_model_prices`, `aisa_model_prices_invalid` | Models with a valid price in Consul KV, and price entries that could not be parsed |
| `aisa_model_price_loads_total{result}` | Reads of the model prices from Consul, `ok` or `error` |
| `aisa_kube_leader` | 1 on the replica that holds the leader's Lease and writes the Kubernetes objects ([`KUBERNETES_OBJECTS.md`](../concepts/KUBERNETES_OBJECTS.md)) |
| `aisa_kube_passes_total{kind,result}` | Passes over the consumers' Secrets (`secrets`) and the `Model` objects (`models`), `ok` or `error` |
| `aisa_consumer_secrets{state}` | Consumers that name a Secret, by the state of the last pass: `ok`, `not_owned` (a Secret of that name is not the consumer's), `duplicate_target` (another consumer names it too) or `error` |
| `aisa_consumer_keys_issued_total{reason}` | Keys aisa issued: `new`, `missing` (the Secret was gone), `unknown_key` (the Secret's key was not one aisa issued) or `rotation` |
| `rest_client_requests_total{code,method,host}`, `rest_client_request_duration_seconds{verb,host}`, `rest_client_rate_limiter_duration_seconds{verb,host}` | aisa's Kubernetes client, under client-go's own names (with `kubernetes.enabled`). The client has no rate limit of its own, the API server's Priority and Fairness applies, so the rate limiter histogram stays empty ([#70](https://github.com/hlan-net/aisa/issues/70)) |
| `aisa_models`, `aisa_models_available` | `Model` objects this install keeps, and those with a healthy backend |
| `aisa_model_conflicts`, `aisa_model_descriptions_invalid` | Models on offer without an object because of their name, and model descriptions in Consul that could not be read |

Keep label cardinality in mind: `consumer` × `model` × `backend`. For large deployments, consumers can be aggregated into groups.

## Secondary metrics: the gateway

APISIX's [prometheus plugin](https://apisix.apache.org/docs/apisix/plugins/prometheus/) also exports `apisix_llm_prompt_tokens`, `apisix_llm_completion_tokens`, `apisix_llm_latency` (with time to first token) and `apisix_llm_active_connections`. They are scraped and used for:
- a **cross-check**: gateway token counts vs aisa's ledger. A mismatch means lost usage events.
- gateway internals: active streams and upstream errors.

No dashboard panel or alert depends only on gateway metrics, so swapping the proxy does not break monitoring.

## Grafana dashboard

To be shipped as JSON in `dashboards/` (`ROADMAP.md`, v0.2.0 PR 7):

1. **Tokens per model**, stacked prompt and completion, split by consumer
2. **Requests and decisions**: allow, deny_quota, deny_budget and downgrade over time
3. **Latency**: p50/p95 total and time to first token per model and backend
4. **This month** per consumer: tokens against the budget and what remains, and their cost in currency
5. **Backends**: Consul health of each `aisa-backend` and, from the proxy, active streams
6. **Ledger consistency**: aisa tokens vs gateway tokens (should be about 1.0), the share of successful requests with missing usage per backend, and the estimated tokens next to the exact ones
7. **Top consumers** table: tokens, cost, and the local vs cloud share

## Alerts

aisa's chart ships them as a `PrometheusRule` (`prometheusRule.enabled`; `prometheusRule.labels` for the Prometheus that selects it), on aisa's own metrics only, so they are the same for every proxy. `AisaDown` watches `up{}` of the ServiceMonitor's job, the Service `<release>-metrics`. The alerts of a feature that is off (`kubernetes.enabled`, `quotas.enabled`) are left out. The conditions:

| Alert | Condition |
|---|---|
| `AisaBudgetSoftLimit` | `aisa_budget_spent / aisa_budget_limit > 0.8` |
| `AisaBudgetExhausted` | a consumer is being denied or downgraded because its budget is used up |
| `AisaBackendDown` | Consul health check for an `aisa-backend` is critical for more than 10 min. Not in the chart: it reads Consul's health, not aisa's metrics; `AisaModelUnavailable` is aisa's view of the same |
| `AisaModelUnavailable` | `aisa_models - aisa_models_available > 0` for 10 min: a model on offer has no backend whose health check passes (with `kubernetes.enabled`) |
| `AisaConfigRenderRejected` | `aisa_config_renders_total{result="rejected"}` increases: a value in Consul or Vault renders a config the gateway cannot load in full; the gateway keeps the previous one until it is fixed |
| `AisaConfigEmpty`, `AisaConfigHeld` | `aisa_config_models == 0` for 5 min, or `aisa_config_renders_total{result="held"}` increases: the catalog has no backend the proxy can render; held, the gateway serves the last config, promoted, every model gets 503. The gauge is absent until the proxy's first promoted report after an aisa restart, so the alert does not fire on a restart |
| `AisaDown` | aisa's scrape target is down for 2 min, or Prometheus has no such target (fail policies are now in effect: an `open` backend serves any credential) |
| `AisaConsumersStale` | the consumers were last loaded more than 5 min ago (aisa fails closed after 15 min) |
| `AisaConsumersUnreadable` | `aisa_consumers_unreadable > 0` for 5 min: a consumer in Vault cannot be read and loses its access after 15 min |
| `AisaQuotaProfileInvalid` | `aisa_quota_profiles_invalid > 0`: the consumers of a broken profile get 503 |
| `AisaQuotaProfileMissing` | `aisa_quota_profiles_missing > 0`: the default profile or a consumer's names a profile that is not in Consul; its consumers get 503 |
| `AisaConsumerDeclarationInvalid` | `aisa_consumer_declarations_invalid > 0`: a consumer with a broken declaration in Consul gets 503 |
| `AisaConsumerSecretFailing` | `aisa_consumer_secrets{state!="ok"} > 0` for 10 min: a consumer's Secret is not written, because of its name, another consumer, or an error |
| `AisaKubeWritesFailing`, `AisaKubeNoLeader` | `aisa_kube_passes_total{result="error"}` increases for 10 min, or `sum(aisa_kube_leader) == 0` for 5 min: the Kubernetes objects are not kept up to date |
| `AisaQuotaCountersFailing` | `aisa_quota_errors_total` increases for 5 min: Redis is unreachable and token quotas are not enforced |
| `AisaUsageEventsLost` | the aisa/gateway token ratio < 0.95 over 1 h. Not in aisa's chart: the gateway's token metric is the proxy's, so the rule belongs to the proxy's chart |
| `AisaUsageMissing` | `aisa_usage_missing_total` increases for 15 min on a backend: its requests are charged by estimate, not counted. The gateway's own token metrics miss the same usage, so `AisaUsageEventsLost` cannot see it |
| `AisaUsageEventsRejected` | `aisa_usage_events_total{result="rejected"}` increases for 15 min: the proxy sends events aisa cannot use. They are answered 200, so the log sink does not retry them and they are lost |
| `AisaUsageRequestsRejected` | `aisa_usage_requests_rejected_total` increases: the proxy sends usage aisa cannot read, and the log sink drops it after its retries |
| `AisaSlowDecisions` | p95 of `aisa_decide_duration_seconds` > 250 ms for 10 min: every request waits for the decision, and a backend's seconds hide it from the client; `aisa_decide_stage_duration_seconds` shows whether Redis or Vault is slow |
| `AisaSlowFirstToken` | p95 time to first token > 20 s for 15 min (model too large for the backend, or the backend is swapping) |
