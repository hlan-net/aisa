# Concept: Proxy contract

aisa is split into a **core** that knows no specific gateway and a **proxy**, an existing AI gateway that carries the traffic ([`ARCHITECTURE.md`](./ARCHITECTURE.md#what-aisa-is)). This document is the contract between the two. APISIX is the first proxy and the reference implementation, not a dependency of the core; what makes a gateway work as aisa's proxy lives in `proxies/<name>/`. The same core should work in front of or beside LiteLLM, Envoy AI Gateway / Agent Router, API7 AISIX, Kong or a plain reverse proxy.

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
 clients ──▶│ proxy: an AI gateway (APISIX first)            │──▶ model backends
            └────────────────────────────────────────────────┘
```

A gateway integrates through three contracts. The fourth is the output aisa itself produces.

## 1. Decision API (before the request)

This follows the forward-auth / external authorization pattern that most gateways already support: APISIX `forward-auth`, Envoy `ext_authz`, Traefik `ForwardAuth`, Kong, and LiteLLM custom auth.

```
POST /v1/decide            (GET is accepted too, for gateways that cannot send a body)
  in:  Authorization         the client's credential, unchanged
       X-Request-Id          the gateway's id for the request, repeated in its usage event
       X-Aisa-Requested-Model  the model from the request body, set by the proxy
       (or the JSON request body, when the header is absent)
       X-Forwarded-Uri / -Method / -Host   the original request, where the gateway sends them
  out: 200 + headers   X-Aisa-Consumer: batch-jobs
                       X-Aisa-Model: qwen3              (possibly rewritten, e.g. budget downgrade)
                       X-Aisa-Budget-Remaining: 1200000  (tokens; only when the consumer has a budget)
       400 no model in the header or the body, or a model name longer than 256 bytes
       401 unknown or invalid credential
       413 no model in the header, and the body is larger than 16 MiB
       429 quota or budget exhausted, with Retry-After in seconds for a token quota
       503 aisa cannot verify credentials: its consumers could not be read from Vault recently
           enough; or it cannot know the consumer's token quota: its quota profile is missing
           or broken in Consul (fail closed)
       (errors carry an OpenAI-style JSON error body, which the proxy passes to the client)
```

- aisa authenticates the client (consumer key or JWT, both backed by Vault), so the gateway needs no per-consumer configuration.
- The decision needs the **model name**, which is in the request body. Gateways that forward the body (Envoy `with_request_body`, APISIX `forward-auth` with `POST`) can send it directly. The preferred form is the `X-Aisa-Requested-Model` header, set by a small pre-step that copies `model` from the body, so aisa never receives the prompt (spike S8). The header takes precedence over the body.
- **Fail policy** is a property of the backend, set in its Consul service meta `fail_policy` ([`CONSUL.md`](./CONSUL.md#backends-consul-catalog)): `closed` (the default) or `open`. When aisa is unreachable, the gateway cannot tell who the consumer is, so the consumer plays no part: a request is served only by the requested model's backends whose policy is `open`, and answered with 503 when the model has none. Typically local models are `open` and paid providers `closed`.

### Proxy rules for the decision

Spike S8 showed that these must hold for the decision to be safe and for downgrade to work:

1. **Strip client-supplied `X-Aisa-*` headers** before the decision. Some forward-auth implementations clear them only on a successful answer, so a client could otherwise choose the model itself when aisa is unreachable and the proxy fails open.
2. **Set `X-Aisa-Requested-Model` from the body**, overwriting any client value, or forward the body.
3. **Route by `X-Aisa-Model` and forward the request with that model**, not the one the client asked for. A downgrade changes both the backend and the model name in the forwarded body.
4. **Pass every answer aisa returns to the client unchanged**: 400, 401, 413, 429 and 503, with `Retry-After` when aisa sets it. A 503 from aisa is a decision (fail closed: it cannot verify credentials or quotas), not an outage, and must never lead to fail-open routing. aisa is *unreachable* only when the gateway gets no answer from it: the connection fails or the request times out. Then the gateway answers 503 itself when the requested model has no backend with `fail_policy = "open"`.
5. **When aisa is unreachable, route by `X-Aisa-Requested-Model`** (rule 2), and only to the model's backends with `fail_policy = "open"`. Never route by `X-Aisa-Model` then: no decision set it. There is no downgrade, and any credential is accepted, including an unknown one. The usage events of these requests have an empty `consumer`; aisa counts them under the consumer `unknown` and charges no quota or budget.

## 2. Usage events (after the request)

The gateway reports each finished request through an access log sink to `POST /v1/usage`. aisa accepts one normalized schema, over HTTP (primary) or OTLP logs:

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

The endpoint accepts a single event JSON object or a JSON array of up to 10 000 events. The request body is bounded to 16 MiB (`413 Payload Too Large` if exceeded).

Each proxy maps its own log format to this schema: the APISIX `http-logger` with a custom `log_format` (spike S6), LiteLLM callbacks, or Envoy access logs. aisa computes cost from the prices and updates the budget counters, so **cost and budgets never depend on gateway-specific metrics**.

Both contracts carry a `request_id` so a decision and its usage event can be matched: the proxy sends the same value to `/v1/decide` as `X-Request-Id`. aisa also uses it to deduplicate retried log deliveries.

| Field | Meaning |
|---|---|
| `ts` | when the gateway logged the request (its end), ISO 8601 |
| `request_id` | **required** non-empty string (max 256 bytes) matching the decision; used for deduplication |
| `consumer` | `X-Aisa-Consumer` from the decision; empty when aisa denied the request or was not asked |
| `model`, `requested_model` | the model the backend served (`X-Aisa-Model`) and the one the client asked for; they differ after a downgrade (max 256 bytes) |
| `backend` | the backend instance's name as registered in Consul, so aisa can look up its provider and prices (max 256 bytes). `provider` is optional for the same reason |
| `status` | **required** upstream or gateway HTTP status code (100–599) |
| `prompt_tokens`, `completion_tokens` | non-negative token counts |
| `latency_ms` | time spent at the backend, including the whole stream; non-negative; empty when no backend was called |
| `ttft_ms` | time to the first token (for non-streamed responses, to the complete response); non-negative |

What aisa accepts, as found in spike S6:

- **Numbers and booleans may arrive as strings.** A gateway log format substitutes variables as text in some cases (APISIX logs `"0"` and `"true"`), so aisa parses numeric and boolean strings. Missing values may be empty or `null`.
- **Every request produces an event, including ones aisa denied** (401, 429) and backend errors. They count as requests; only events with token counts change quotas and budgets.
- **Usage events carry only these fields, never request headers or bodies**, so no credential reaches the usage sink ([#5](https://github.com/hlan-net/aisa/issues/5)). The proxy sets the log sink's format explicitly and never relies on the gateway's default: the default of APISIX's `http-logger` includes the client's request headers, `Authorization` among them. aisa ignores fields it does not know and never logs a raw event body at info or warn level, so a misconfigured sink does not copy a credential into aisa's logs.
- **A successful response without token counts means the gateway did not see the usage**, not that none was used. The counts are then missing or zero, as numbers or as strings. This happens when a backend does not stream usage or the client disconnects before the end of a stream (spike S2). Proxies must ask for streamed usage (`stream_options.include_usage` for OpenAI-compatible backends). aisa counts the remaining cases, a 2xx status with neither prompt nor completion tokens, in `aisa_usage_missing_total`; it does not yet charge an estimate for them ([#12](https://github.com/hlan-net/aisa/issues/12)).
- **Partial batch acceptance:** Invalid events in a batch are individually skipped and counted under `rejected`. An event is invalid when it fails validation (e.g. missing `request_id`, status out of range 100–599, negative tokens), when a field value cannot be parsed (e.g. `"prompt_tokens": "abc"` or `1.9`, `"latency_ms": "NaN"`, `"stream": "yes"`), or when an array element is not an object. Valid events in the same batch are ingested and counted under `accepted`. Duplicates (by `request_id` within the deduplication TTL) are acknowledged and counted as `accepted` so log sinks do not endlessly retry them. The endpoint answers `200 OK` with `{"accepted": n, "rejected": m}`.
- **Whole-request rejection:** the endpoint answers `400 Bad Request` only when the body is not well-formed JSON of the expected shape (a single object, or an array with its brackets and commas intact and nothing after it), has more than 10 000 events, or cannot be read; and `413` when it is larger than 16 MiB. None of its events are read, and the rejection is counted in `aisa_usage_requests_rejected_total`. A method other than `POST` is answered `405` and not counted: a log sink always posts, so such a request is no usage being lost.
- **Logging:** rejected events are logged at `warn` level once per request, with their number and the first error, never with a field's value.

## 3. Config rendering (gateway configuration)

Backends (Consul catalog), provider keys (Vault) and model routing live in aisa's sources of truth. They are rendered into the gateway's native config by **consul-template**, which reads both Consul and Vault (KV v2 included) and can reload the gateway after a change. Each proxy ships one template:

| Proxy | Template output | Reload |
|---|---|---|
| APISIX | `apisix.yaml` (standalone mode): the client-facing route (pre-step, forward-auth), one internal route per model with its `ai-proxy-multi` instances, http-logger | APISIX reloads standalone config on file change |
| LiteLLM (planned second proxy) | `config.yaml` `model_list` | restart or config API |
| Envoy AI Gateway / Agent Router (possible) | `AIServiceBackend` / `AIGatewayRoute` resources | apply to the cluster |

What a template does, as found in spike S9:

- **Only healthy backends are rendered**, grouped by the models in their service meta. A model without a healthy backend gets an answer from the gateway itself: 503 with an error body, not a missing route.
- **A backend names its provider key** with the service meta `key`: the name of the secret under `secret/aisa/providers/`. A backend without it gets no key, and **no backend gets the client's `Authorization`**: APISIX's `ai-proxy-multi` forwards the client's headers, so the internal route removes it.
- **A second hop keeps the request's id.** The internal route of APISIX is a new request with an id of its own, so the client-facing route passes the id it sent to the decision along, and the usage event carries that one.
- **A request aisa denied is reported by the client-facing route**, because it never reaches the internal one. Its logger runs only for requests without `X-Aisa-Consumer`, so an allowed request still has one event.
- **A backend is named by its Consul service ID**, which must be unique in the catalog, and **reached over `https` when it has a key**, unless its meta `scheme` says `http` ([`CONSUL.md`](./CONSUL.md#backends-consul-catalog)).
- **Names from Consul are made valid before they become identifiers of the gateway.** A model is called `llama3.2:3b`, and a gateway may not take the colon in the id of a route; it then leaves the route out (spike S5).
- **Values from Consul and Vault are rendered as quoted strings**, so a name cannot change the structure of the config.
- **The rendered file is shared as a directory.** consul-template replaces the file by renaming a new one over it, which a mount of the single file does not show.
- **The template sets how often secrets are read again** (`default_lease_duration`); it is the time a rotated key takes to reach the gateway.
- **The upstream timeout is always set explicitly**, never left to the gateway's default: APISIX's `ai-proxy-multi` cuts off after 30 s, before a slow local model has finished a non-streamed answer (spike S2, [#14](https://github.com/hlan-net/aisa/issues/14)). It comes from the backend's meta `timeout` in seconds, 300 when absent, and is capped at the largest value the gateway accepts; the proxy's README states that cap (600 s for APISIX). An internal hop between routes gets a read timeout at least as long. Clients of backends that can take longer than the cap must stream: the timeout limits the wait for the next byte, not the whole response.

Rendering replaces gateway-specific discovery and secret integrations. APISIX's Vault limitations (KV v1 only, static token) and the question of Consul discovery in `ai-proxy-multi` stop mattering, because the gateway never talks to Vault or Consul itself.

### Checking the rendered config

A value in Consul or Vault must not be able to change what the gateway serves beyond its own backend (spike S9, [#18](https://github.com/hlan-net/aisa/issues/18)). APISIX, for instance, drops an invalid route without failing the file, so one bad value can turn the client-facing route into a 404. A proxy therefore never hands the gateway a config it has not checked:

1. **The template checks the values it renders.** A backend with an unknown `provider`, no `models` or an invalid `timeout` is left out, with a comment in the rendered file that names it; the other backends and the client-facing route are rendered as usual.
2. **consul-template renders to a staging file**, and its `command` checks that file and only then moves it to the path the gateway reads. The check uses the gateway's own validation where it has one, otherwise a schema the proxy ships, and also requires the client-facing route. A file that fails the check is not promoted: the gateway keeps serving the last good config.
3. **An empty catalog is promoted only after a grace period** (5 minutes by default). Until then the last config stays. The catalog can come back empty although the backends are fine, for example when the render token may not read the service or Consul lost its data; after the grace period the empty config is promoted, and every model gets the gateway's 503. The same delay applies when every backend fails its health check at once: requests during it get the gateway's error for an unreachable backend instead of 503.

How the proxy reports a rejected render or an empty catalog to aisa, so that it shows in `aisa_*` metrics and alerts and not only in a log, is still open ([#18](https://github.com/hlan-net/aisa/issues/18)).

## 4. Metrics (output of aisa)

aisa exports its own normalized metrics, so the dashboards and alerts work with any proxy:

| Metric | Labels |
|---|---|
| `aisa_requests_total` | consumer, model, backend, status |
| `aisa_tokens_total` | consumer, model, backend, direction (`prompt`/`completion`) |
| `aisa_cost_total` | consumer, model, currency |
| `aisa_budget_limit`, `aisa_budget_spent` | consumer, period |
| `aisa_latency_seconds`, `aisa_ttft_seconds` (histograms) | model, backend |
| `aisa_decisions_total` | consumer, result (`allow`/`deny_auth`/`invalid`/`unavailable`/`deny_quota`/`deny_budget`/`downgrade`); a rejected credential and a 503 before the credential is checked count under the consumer `unknown` |
| `aisa_usage_events_total` | result (`accepted`/`duplicate`/`rejected`) |
| `aisa_usage_requests_rejected_total` | reason (`malformed`/`too_large`/`unreadable`): usage requests rejected as a whole, whose events are in no other metric |
| `aisa_usage_missing_total` | consumer, model, backend: successful requests reported without token counts, whose usage aisa has not accounted |

Gateway-native metrics (e.g. `apisix_llm_*`) are still scraped, but they only serve as a cross-check and for gateway internals.

## Proxy checklist

A proxy is complete when it provides:
1. a way to call `/v1/decide` before proxying, including the model name, and to apply the returned headers following the [proxy rules](#proxy-rules-for-the-decision)
2. a usage log sink with an explicit format that maps to the event schema, including streaming token counts, and carries no credentials
3. a consul-template template for the gateway config, with an explicit upstream timeout and the [check](#checking-the-rendered-config) before the gateway loads it
4. an example deployment and an integration test against a mock backend

The contract is versioned (`/v1/`) from the start and stays marked unstable until a second proxy implements it.
