# Quotas and Budgets

## Concept

Every consumer gets a token quota, and optionally a monthly budget. Both are counted in tokens and enforced by **aisa**, not by the gateway, so they work the same with any proxy. The gateway only calls `/v1/decide` before the request and reports usage afterwards ([`PROXY_CONTRACT.md`](../concepts/PROXY_CONTRACT.md)).

## Flow

```
request ─▶ gateway ─▶ /v1/decide
                        1. authenticate consumer
                        2. quota:  tokens used in the current window  < profile limit ?
                        3. budget: tokens used this month             < monthly budget ?
                        4. result: allow | deny_quota (429) | deny_budget (429) | downgrade (X-Aisa-Model rewritten)
          ◀─ headers ─┘
gateway ─▶ backend ─▶ response to client
gateway ─▶ usage event ─▶ aisa: update the token counters; cost = tokens × price, for display
```

## Token quotas (v0.2.0)

- A consumer names its quota profile in its declaration in Consul (`quota_profile` of `aisa/consumers/<name>`, [`CONSUL.md`](../concepts/CONSUL.md#consumers-consul-kv)). The profile, `aisa/quotas/<profile>` in Consul, gives the tokens per hour: `{"tokens_per_hour": 200000}` for batch jobs, more for interactive use. `0` allows none. aisa follows the prefix with blocking queries, so a changed profile applies within seconds, without a restart.
- **Sliding window in Redis**, per consumer: an hour of one-minute buckets. A request's prompt and completion tokens count in the minute the gateway logged it and leave the window 59 to 60 minutes later. The keys are `aisa:quota:{<consumer>}:<minute>`; they expire by themselves, and one consumer's keys share a Redis Cluster slot.
- **The decision** allows a request while the tokens used in the window are fewer than the limit. Its own tokens are known only after the response, so the last request can take a consumer past the limit, as with budgets. An exhausted quota is answered 429 with `Retry-After`, the seconds until enough tokens leave the window, and counted as `deny_quota`.
- **Usage events** add their tokens to the window. Events without a consumer (fail-open, [`PROXY_CONTRACT.md`](../concepts/PROXY_CONTRACT.md#proxy-rules-for-the-decision)) and duplicates are not charged, and neither are events older than the window.
- **Missing usage is charged by estimate.** A successful request whose event has no token counts (a backend that streams no usage, a client that disconnected mid-stream) adds the consumer's recent average for that model to the window; the model's average across consumers while the consumer has none; else `AISA_LEDGER_ESTIMATE_DEFAULT_TOKENS`. That default is 0 unless the holder sets it, so until aisa has seen an exact request of the model, such a request is charged nothing: set the default for a backend that never reports usage, where no exact request ever comes. The estimates are visible in `aisa_tokens_estimated_total` ([`usage-metrics.md`](./usage-metrics.md), [#12](https://github.com/hlan-net/aisa/issues/12)).
- **A consumer without `quota_profile` is denied** with 403 (`no_quota`), counted as `no_quota`, unless the holder sets a default profile with `AISA_DEFAULT_QUOTA_PROFILE` (the chart's `quotas.defaultProfile`), which then applies to it. No consumer is served without a limit the holder set, including one the holder forgot to limit ([#33](https://github.com/hlan-net/aisa/issues/33)). The rule holds with quotas off too: there, the profile is required and not checked.
- **A profile that is named must be in Consul.** aisa is not ready until it has read the default profile, so an install whose default names no profile never comes up; a default removed later, or a consumer whose own `quota_profile` is not under `aisa/quotas/`, is counted in `aisa_quota_profiles_missing{source}` (`default` or `consumer`) and alerted on (`AisaQuotaProfileMissing`), and the consumers that depend on it are answered 503 (`quota_unavailable`) until it is there ([#82](https://github.com/hlan-net/aisa/issues/82)).
- **Failures:** a profile that is missing from Consul or cannot be parsed, or profiles that have not been read yet, deny the consumer's requests with 503 (`quota_unavailable`): fail closed, so a typo does not lift a limit unnoticed. `/readyz` reports `quota_profiles` until the first read. When Redis cannot be read within 250 ms, the request is allowed and counted in `aisa_quota_errors_total{op="check"}`: the credential has been checked, and a quota only limits the rate.
- Quotas are on when `AISA_REDIS_ADDR` is set; Consul (`CONSUL_HTTP_ADDR`) is always read, the consumers are declared there ([`cmd/aisa`](../../cmd/aisa/main.go)).
- Unit: tokens, not money. For local models this is the right unit, because they have no per-token price, and it protects the hardware from runaway scripts.

Gateway-native quota plugins (e.g. APISIX `ai-rate-limiting`) are **not** used. They would split the enforcement logic between aisa and each gateway.

## Budgets (v0.3.0)

Every limit is in tokens; currency is how spend is shown, never how it is limited (decided 2026-09-29).

- Monthly budget per consumer in `aisa/budgets/<consumer>`, in tokens (`monthly_tokens`), **calendar month** in the holder's time zone (`AISA_TIMEZONE`, the chart's `timezone`; UTC by default). Used = Σ (prompt tokens + completion tokens) of the consumer's requests that month to models with a price > 0 (`input_per_1k > 0` or `output_per_1k > 0` in `aisa/pricing/<model>`); prompt and completion tokens weigh 1:1 (decided 2026-10-20). A request whose usage is missing counts by the same estimate as for the quota. Local models cost 0 by default and are limited by the hourly token quota, though a local model can also be given a price (for instance from measured power use) when the holder wants it to count towards the budget. A consumer without a budget is bounded by its quota only; a budget that is broken denies the consumer's requests to priced models with 503 (`budget_unavailable`), fail closed, and is counted in `aisa_budgets_invalid`.
- **The decision applies the budget to priced models only.** A request to a model with a price is denied or downgraded when the month's tokens are used up; a request to a free model is allowed whatever the budget. Every decision of a consumer with a budget carries `X-Aisa-Budget-Remaining` (tokens, 0 when none are left), which the proxy copies to the client's response ([`PROXY_CONTRACT.md`](../concepts/PROXY_CONTRACT.md#proxy-rules-for-the-decision), rule 6). A request's own tokens are known only after the response, so the last request of a month can take a consumer past the budget.
- **Cost is for display**: Σ (prompt tokens × input price + completion tokens × output price), with prices from `aisa/pricing/<model>` ([`CONSUL.md`](../concepts/CONSUL.md)), in `aisa_cost_total` and the dashboard. Only exact reported tokens (`prompt_tokens`, `completion_tokens`) are added to `aisa_cost_total`; a model without a price entry in Consul has no `aisa_cost_total` series. Cost denies nothing.
- **Soft limit** (`soft_ratio`, 0.8 by default): the request is allowed; `aisa_budget_spent / aisa_budget_limit` shows the share used and the chart's `AisaBudgetSoftLimit` alerts at 80 %.
- **Exhausted**, depending on `on_exhausted`:
  - `reject` (the default): 429 with an OpenAI-style error body (`insufficient_quota` / `budget_exhausted`), counted as `deny_budget`. No `Retry-After`: the month decides.
  - `downgrade`: allow the request, but set `X-Aisa-Model` to `downgrade_to`, a local model; counted as `downgrade`. The proxy routes by that header, so when the cloud budget runs out, work continues on local models. A request for the downgrade model itself, or for any free model, is simply allowed.
- **A ceiling per provider account** ([#33](https://github.com/hlan-net/aisa/issues/33)): `aisa/budgets/providers/<key>`, with the same fields, is a budget on everything that goes through a provider key, above the consumers' budgets, so the holder states the total they allow per account. A model is bounded by the budgets of every key whose backends serve it (the service meta `key` in Consul); a model that only keyless backends serve has none. When a key's month is used up, a request to any model it serves is answered 429 (`insufficient_quota` / `provider_budget_exhausted`, counted as `deny_provider_budget`) or sent to the provider budget's `downgrade_to`, after the consumer's own budget has had its say. The tokens of a request are charged to the key of the backend that answered it (the usage event's `backend`, by its Consul ID, address or address:port); when the gateway names a backend the catalog does not know, every key of the model is charged, since the request went through one of them and a ceiling must not be lifted by a name. Fail-open requests count too. The client sees only its own `X-Aisa-Budget-Remaining`: the holder's totals are in `aisa_provider_budget_limit` and `aisa_provider_budget_spent`. aisa reads the catalog every 30 s for this.
- **Counters:** the month's tokens are counted in Redis (`aisa:budget:{<consumer>}:<YYYY-MM>`, kept 35 days past the month) and copied to Consul KV once a minute (`aisa/state/<consumer>/<YYYY-MM>`, written by aisa). A Redis that loses its data starts a month's counter again from the copy, and while Redis cannot be read the copy stands in for it: a budget is a bound on spend, and the copy is a lower bound on it, at most a minute behind. Failed reads and writes are counted in `aisa_budget_errors_total{op}`. The provider budgets have their own counters (`aisa:provider-budget:{<key>}:<YYYY-MM>`, `aisa/state/providers/<key>/<YYYY-MM>`). Budgets, like quotas, are on when `AISA_REDIS_ADDR` is set.

## Design notes

- **Accuracy:** a request's tokens are only known after the response, so the last request of a month can overshoot the budget. This is acceptable and documented. Reserving an estimate up front would add complexity for little gain.
- **Streaming:** token usage arrives at the end of the stream. The proxy must make sure the usage event contains it (for Ollama's OpenAI endpoint via `stream_options.include_usage`; spike S2).
- **Idempotency:** usage events are deduplicated by `request_id`, because log sinks retry.
- **Persistence:** Redis holds the hot counters, and monthly totals are copied to Consul KV every minute.
- **Downgrade needs the proxy to honour `X-Aisa-Model`.** This is part of the proxy contract: the gateway routes by that header and forwards the request with that model. For APISIX, an internal route per model matches the header and its `ai-proxy-multi` instances pin the model name (spike S8).
- **No budget logic in gateway plugins.** If the extra hop to aisa ever becomes a latency problem, a gateway plugin can cache aisa's decisions as an *optimisation*. aisa stays the only place where the rules live.
