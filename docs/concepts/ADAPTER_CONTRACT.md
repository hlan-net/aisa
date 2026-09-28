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
POST /v1/decide
  in:  Authorization header, requested model, route, optional body summary
  out: 200 + headers   X-Aisa-Consumer: batch-jobs
                       X-Aisa-Model: qwen3              (possibly rewritten, e.g. budget downgrade)
                       X-Aisa-Budget-Remaining: 4.20
       401 unknown or invalid credential
       429 quota or budget exhausted (OpenAI-style JSON error body)
```

- aisa authenticates the client (consumer key or JWT, both backed by Vault), so the gateway needs no per-consumer configuration.
- The decision needs the **model name**, which is in the request body. Gateways that forward the body (Envoy `with_request_body`) send it directly. Gateways that only forward headers need a small pre-step that copies `model` to a header (spike S8).
- **Fail policy** is configurable per consumer: `closed` (reject if aisa is down) for paid providers, `open` for local models.

## 2. Usage events (after the request)

The gateway reports each finished request through an access log sink. aisa accepts one normalized schema, over HTTP (primary) or OTLP logs:

```json
{
  "ts": "2026-09-28T12:00:00Z",
  "request_id": "…",
  "consumer": "batch-jobs",
  "model": "qwen3",
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

Both contracts carry a `request_id` so a decision and its usage event can be matched. aisa also uses it to deduplicate retried log deliveries.

## 3. Config rendering (gateway configuration)

Backends (Consul catalog), provider keys (Vault) and model routing live in aisa's sources of truth. They are rendered into the gateway's native config by **consul-template**, which reads both Consul and Vault (KV v2 included) and can reload the gateway after a change. Each adapter ships one template:

| Adapter | Template output | Reload |
|---|---|---|
| APISIX | `apisix.yaml` (standalone mode): routes, `ai-proxy-multi` instances, forward-auth, http-logger | APISIX reloads standalone config on file change |
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
1. a way to call `/v1/decide` before proxying, including the model name, and to apply the returned headers
2. a usage log sink that maps to the event schema, including streaming token counts
3. a consul-template template for the gateway config
4. an example deployment and an integration test against a mock backend

The contract is versioned (`/v1/`) from the start and stays marked unstable until a second adapter implements it.
