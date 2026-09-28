# Concept: Adapter contract

aisa is split into a **gateway-agnostic core** and **thin adapters** for specific AI gateways. APISIX is the first adapter and the reference implementation, not a dependency of the core. The same core should work in front of or beside LiteLLM, Envoy AI Gateway / Agent Router, API7 AISIX, Kong or a plain reverse proxy.

Related: [`ARCHITECTURE.md`](./ARCHITECTURE.md), [`VAULT.md`](./VAULT.md), [`CONSUL.md`](./CONSUL.md), [`../process/SPIKES.md`](../process/SPIKES.md).

The core owns everything aisa adds: identities, budgets, pricing, backends and normalized usage metrics. The gateway only proxies traffic and translates between providers.

```
            ┌──────────────── aisa core (Go) ───────────────┐
 Vault ────▶│ identities · provider keys                     │
 Consul ───▶│ backends · prices · budgets                    │──▶ /metrics (aisa_*)
            │ decision API · usage ledger · config renderer  │
            └────▲──────────────────▲────────────────┬───────┘
      1. decide  │   2. usage events │    3. config   │
            ┌────┴──────────────────┴────────────────▼───────┐
 clients ──▶│ AI gateway (adapter: APISIX first)             │──▶ model backends
            └────────────────────────────────────────────────┘
```

A gateway integrates through three contracts. The fourth is the output aisa itself produces.

## 1. Decision API (before the request)

This follows the forward-auth / external authorization pattern that most gateways already support: APISIX `forward-auth`, Envoy `ext_authz`, Traefik `ForwardAuth`, Kong, and LiteLLM custom auth.

```
POST /v1/decide            (GET is accepted too, for gateways that cannot send a body)
  in:  Authorization         the client's credential, unchanged
       X-Request-Id          the gateway's id for the request, repeated in its usage event
       X-Aisa-Requested-Model  the model from the request body, set by the adapter
       (or the JSON request body, when the header is absent)
       X-Forwarded-Uri / -Method / -Host   the original request, where the gateway sends them
  out: 200 + headers   X-Aisa-Consumer: batch-jobs
                       X-Aisa-Model: qwen3              (possibly rewritten, e.g. budget downgrade)
                       X-Aisa-Budget-Remaining: 4.20
       400 no model in the header or the body
       401 unknown or invalid credential
       429 quota or budget exhausted
       (errors carry an OpenAI-style JSON error body, which the adapter passes to the client)
```

- aisa authenticates the client (consumer key or JWT, both backed by Vault), so the gateway needs no per-consumer configuration.
- The decision needs the **model name**, which is in the request body. Gateways that forward the body (Envoy `with_request_body`, APISIX `forward-auth` with `POST`) can send it directly. The preferred form is the `X-Aisa-Requested-Model` header, set by a small pre-step that copies `model` from the body, so aisa never receives the prompt (spike S8). The header takes precedence over the body.
- **Fail policy** is configurable per consumer: `closed` (reject if aisa is down) for paid providers, `open` for local models. When aisa is unreachable the gateway cannot tell which consumer a request belongs to, so this is an open design question; until it is settled, adapters fail closed with 503.

### Adapter rules for the decision

Spike S8 showed that these must hold for the decision to be safe and for downgrade to work:

1. **Strip client-supplied `X-Aisa-*` headers** before the decision. Some forward-auth implementations clear them only on a successful answer, so a client could otherwise choose the model itself when aisa is unreachable and the adapter fails open.
2. **Set `X-Aisa-Requested-Model` from the body**, overwriting any client value, or forward the body.
3. **Route by `X-Aisa-Model` and forward the request with that model**, not the one the client asked for. A downgrade changes both the backend and the model name in the forwarded body.
4. **Pass aisa's 401, 429 and 400 answers to the client unchanged**, and answer 503 when aisa is unreachable and the fail policy is closed.

## 2. Usage events (after the request)

The gateway reports each finished request through an access log sink. aisa accepts one normalized schema, over HTTP (primary) or OTLP logs:

```json
{
  "ts": "2026-09-28T12:00:00Z",
  "request_id": "…",
  "consumer": "batch-jobs",
  "model": "qwen3",
  "requested_model": "cloud-large",
  "backend": "ollama-1",
  "provider": "openai-compatible",
  "prompt_tokens": 812,
  "completion_tokens": 240,
  "status": 200,
  "latency_ms": 9120,
  "ttft_ms": 1450,
  "stream": true
}
```

Each adapter maps its own log format to this schema: the APISIX `http-logger` with a custom `log_format` (spike S6), LiteLLM callbacks, or Envoy access logs. aisa computes cost from the prices and updates the budget counters, so **cost and budgets never depend on gateway-specific metrics**.

Both contracts carry a `request_id` so a decision and its usage event can be matched: the adapter sends the same value to `/v1/decide` as `X-Request-Id`. aisa also uses it to deduplicate retried log deliveries.

| Field | Meaning |
|---|---|
| `ts` | when the gateway logged the request (its end), ISO 8601 |
| `consumer` | `X-Aisa-Consumer` from the decision; empty when aisa denied the request or was not asked |
| `model`, `requested_model` | the model the backend served (`X-Aisa-Model`) and the one the client asked for; they differ after a downgrade |
| `backend` | the backend instance's name as registered in Consul, so aisa can look up its provider and prices. `provider` is optional for the same reason |
| `latency_ms` | time spent at the backend, including the whole stream; empty when no backend was called |
| `ttft_ms` | time to the first token (for non-streamed responses, to the complete response) |

What aisa accepts, as found in spike S6:

- **Numbers and booleans may arrive as strings.** A gateway log format substitutes variables as text in some cases (APISIX logs `"0"` and `"true"`), so aisa parses numeric and boolean strings. Missing values may be empty or `null`.
- **Every request produces an event, including ones aisa denied** (401, 429) and backend errors. They count as requests; only events with token counts change quotas and budgets.
- **Usage events carry only these fields, never request headers or bodies**, so no credential reaches the usage sink ([#5](https://github.com/hlan-net/aisa/issues/5)).
- **A successful response without token counts means the gateway did not see the usage**, not that none was used. The counts are then missing or zero, as numbers or as strings. This happens when a backend does not stream usage or the client disconnects before the end of a stream (spike S2). Adapters must ask for streamed usage (`stream_options.include_usage` for OpenAI-compatible backends); how aisa accounts for the remaining cases is an open question ([#12](https://github.com/hlan-net/aisa/issues/12)).

## 3. Config rendering (gateway configuration)

Backends (Consul catalog), provider keys (Vault) and model routing live in aisa's sources of truth. They are rendered into the gateway's native config by **consul-template**, which reads both Consul and Vault (KV v2 included) and can reload the gateway after a change. Each adapter ships one template:

| Adapter | Template output | Reload |
|---|---|---|
| APISIX | `apisix.yaml` (standalone mode): the client-facing route (pre-step, forward-auth), one internal route per model with its `ai-proxy-multi` instances, http-logger | APISIX reloads standalone config on file change |
| LiteLLM (planned second adapter) | `config.yaml` `model_list` | restart or config API |
| Envoy AI Gateway / Agent Router (possible) | `AIServiceBackend` / `AIGatewayRoute` resources | apply to the cluster |

Rendering replaces gateway-specific discovery and secret integrations. APISIX's Vault limitations (KV v1 only, static token) and the question of Consul discovery in `ai-proxy-multi` stop mattering, because the gateway never talks to Vault or Consul itself.

## 4. Metrics (output of aisa)

aisa exports its own normalized metrics, so the dashboards and alerts work with any adapter:

| Metric | Labels |
|---|---|
| `aisa_requests_total` | consumer, model, backend, status |
| `aisa_tokens_total` | consumer, model, backend, direction (`prompt`/`completion`) |
| `aisa_cost_total` | consumer, model, currency |
| `aisa_budget_limit`, `aisa_budget_spent` | consumer, period |
| `aisa_latency_seconds`, `aisa_ttft_seconds` (histograms) | model, backend |
| `aisa_decisions_total` | consumer, result (`allow`/`deny_quota`/`deny_budget`/`downgrade`) |

Gateway-native metrics (e.g. `apisix_llm_*`) are still scraped, but they only serve as a cross-check and for gateway internals.

## Adapter checklist

An adapter is complete when it provides:
1. a way to call `/v1/decide` before proxying, including the model name, and to apply the returned headers following the [adapter rules](#adapter-rules-for-the-decision)
2. a usage log sink that maps to the event schema, including streaming token counts, and carries no credentials
3. a consul-template template for the gateway config
4. an example deployment and an integration test against a mock backend

The contract is versioned (`/v1/`) from the start and stays marked unstable until a second adapter implements it.
