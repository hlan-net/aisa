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

- A consumer names its quota profile in Vault (`quota_profile`, [`VAULT.md`](../concepts/VAULT.md#consumer-credentials)). The profile, `aisa/quotas/<profile>` in Consul, gives the tokens per hour: `{"tokens_per_hour": 200000}` for batch jobs, more for interactive use. `0` allows none. aisa follows the prefix with blocking queries, so a changed profile applies within seconds, without a restart.
- **Sliding window in Redis**, per consumer: an hour of one-minute buckets. A request's prompt and completion tokens count in the minute the gateway logged it and leave the window 59 to 60 minutes later. The keys are `aisa:quota:{<consumer>}:<minute>`; they expire by themselves, and one consumer's keys share a Redis Cluster slot.
- **The decision** allows a request while the tokens used in the window are fewer than the limit. Its own tokens are known only after the response, so the last request can take a consumer past the limit, as with budgets. An exhausted quota is answered 429 with `Retry-After`, the seconds until enough tokens leave the window, and counted as `deny_quota`.
- **Usage events** add their tokens to the window. Events without a consumer (fail-open, [`PROXY_CONTRACT.md`](../concepts/PROXY_CONTRACT.md#proxy-rules-for-the-decision)) and duplicates are not charged, and neither are events older than the window.
- **A consumer without `quota_profile` has no token quota.**
- **Failures:** a profile that is missing from Consul or cannot be parsed, or profiles that have not been read yet, deny the consumer's requests with 503 (`quota_unavailable`): fail closed, so a typo does not lift a limit unnoticed. `/readyz` reports `quota_profiles` until the first read. When Redis cannot be read within 250 ms, the request is allowed and counted in `aisa_quota_errors_total{op="check"}`: the credential has been checked, and a quota only limits the rate.
- Quotas are on when both `CONSUL_HTTP_ADDR` and `AISA_REDIS_ADDR` are set ([`cmd/aisa`](../../cmd/aisa/main.go)).
- Unit: tokens, not money. For local models this is the right unit, because they have no per-token price, and it protects the hardware from runaway scripts.

Gateway-native quota plugins (e.g. APISIX `ai-rate-limiting`) are **not** used. They would split the enforcement logic between aisa and each gateway.

## Budgets (v0.3.0)

Every limit is in tokens; currency is how spend is shown, never how it is limited (decided 2026-09-29).

- Monthly budget per consumer in `aisa/budgets/<consumer>`, in tokens (`monthly_tokens`), **calendar month** in the configured time zone. Used = Σ (prompt tokens + completion tokens) of the consumer's requests that month.
- **Cost is for display**: Σ (prompt tokens × input price + completion tokens × output price), with prices from `aisa/pricing/<model>` ([`CONSUL.md`](../concepts/CONSUL.md)), in `aisa_cost_total` and the dashboard. It denies nothing.
- **Open:** whether every model's tokens count against the budget or only those of models with a price, and whether prompt and completion tokens weigh the same. Decided with the first v0.3.0 PR.
- **Soft limit** (e.g. 80 %): the request is allowed, the response gets `X-Aisa-Budget-Remaining` (tokens), and an alert fires.
- **Exhausted**, depending on `on_exhausted`:
  - `reject`: 429 with an OpenAI-style error body
  - `downgrade`: allow the request, but set `X-Aisa-Model` to the configured local model. The proxy routes by that header, so when the cloud budget runs out, work continues on local models.

## Design notes

- **Accuracy:** a request's tokens are only known after the response, so the last request of a month can overshoot the budget. This is acceptable and documented. Reserving an estimate up front would add complexity for little gain.
- **Streaming:** token usage arrives at the end of the stream. The proxy must make sure the usage event contains it (for Ollama's OpenAI endpoint via `stream_options.include_usage`; spike S2).
- **Idempotency:** usage events are deduplicated by `request_id`, because log sinks retry.
- **Persistence:** Redis holds the hot counters, and monthly totals are copied to Consul KV every minute.
- **Downgrade needs the proxy to honour `X-Aisa-Model`.** This is part of the proxy contract: the gateway routes by that header and forwards the request with that model. For APISIX, an internal route per model matches the header and its `ai-proxy-multi` instances pin the model name (spike S8).
- **No budget logic in gateway plugins.** If the extra hop to aisa ever becomes a latency problem, a gateway plugin can cache aisa's decisions as an *optimisation*. aisa stays the only place where the rules live.
