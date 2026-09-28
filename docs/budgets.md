# Quotas and budgets

Both are enforced by **aisa**, not by the gateway, so they work the same with any adapter. The gateway only calls `/v1/decide` before the request and reports usage afterwards ([interfaces.md](interfaces.md)).

## Flow

```
request ─▶ gateway ─▶ /v1/decide
                        1. authenticate consumer
                        2. quota:  tokens used in the current window  < profile limit ?
                        3. budget: spend this month                   < monthly budget ?
                        4. result: allow | deny_quota (429) | deny_budget (429) | downgrade (X-Aisa-Model rewritten)
          ◀─ headers ─┘
gateway ─▶ backend ─▶ response to client
gateway ─▶ usage event ─▶ aisa: cost = tokens × price, then update the counters
```

## 1. Token quotas (first version)

- A quota profile (`aisa/quotas/<profile>` in Consul) gives tokens per window, e.g. 200 000 tokens per hour for batch jobs and more for interactive use.
- Sliding window counters in Redis, keyed by consumer.
- Unit: tokens, not money. For local models this is the right unit, because they have no per-token price, and it protects the hardware from runaway scripts.

Gateway-native quota plugins (e.g. APISIX `ai-rate-limiting`) are **not** used. They would split the enforcement logic between aisa and each gateway.

## 2. Money budgets

- Monthly budget per consumer in `aisa/budgets/<consumer>`, **calendar month** in the configured time zone.
- Spend = Σ (prompt tokens × input price + completion tokens × output price), with prices from `aisa/pricing/<model>`.
- **Soft limit** (e.g. 80 %): the request is allowed, the response gets `X-Aisa-Budget-Remaining`, and an alert fires.
- **Exhausted**, depending on `on_exhausted`:
  - `reject`: 429 with an OpenAI-style error body
  - `downgrade`: allow the request, but set `X-Aisa-Model` to the configured local model. The adapter routes by that header, so when the cloud budget runs out, work continues on local models.

## Design notes

- **Accuracy:** cost is only known after the response, so the last request of a month can overshoot the budget. This is acceptable and documented. Reserving an estimate up front would add complexity for little gain.
- **Streaming:** token usage arrives at the end of the stream. The adapter must make sure the usage event contains it (for Ollama's OpenAI endpoint via `stream_options.include_usage`; spike S2).
- **Idempotency:** usage events are deduplicated by `request_id`, because log sinks retry.
- **Persistence:** Redis holds the hot counters, and monthly totals are copied to Consul KV every minute ([consul.md](consul.md)).
- **Downgrade needs the adapter to honour `X-Aisa-Model`.** This is part of the adapter contract. For APISIX, the route selects the `ai-proxy-multi` instance by that header (spike S8).
- **No budget logic in gateway plugins.** If the extra hop to aisa ever becomes a latency problem, a gateway plugin can cache aisa's decisions as an *optimisation*. aisa stays the only place where the rules live.
