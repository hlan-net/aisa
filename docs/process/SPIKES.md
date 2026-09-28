# Spikes: unknowns to verify first

Each spike is a small experiment on a throwaway APISIX instance (standalone mode, one route to an Ollama backend) with a stub aisa, e.g. a 50-line Go HTTP server that logs what it receives. Its result decides a design choice in the concept docs. Record the outcome here, with the date.

Most spikes test whether the **APISIX adapter** can meet the contract in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md). A failed spike means extra adapter work (for example a small Lua pre-step), not a change to the core. The PRs that run them are listed in [`ROADMAP.md`](../../ROADMAP.md) v0.1.0.

| # | Question | Why it matters | How to test | Outcome |
|---|---|---|---|---|
| S2 | Are token counts in the gateway's log data correct for **streaming** responses from Ollama's OpenAI endpoint? | Usage events feed cost and budgets | Compare the logged counts with Ollama's own `eval_count` for streamed and non-streamed requests, with and without `stream_options.include_usage` | — |
| S5 | Does the official `apache/apisix` Helm chart support standalone mode well, or are plain manifests simpler? | Adapter deployment shape | Install the chart with standalone values and check for an etcd dependency | — |
| S6 | Can APISIX `http-logger` produce the usage event schema: token counts, model, TTFT and upstream from the AI plugin variables in `log_format`? | Contract 2 | Configure `log_format` with the `llm_*` variables and receive the events in the stub | — |
| S7 | Resource use on a small arm64 node (e.g. Raspberry Pi 4) of APISIX, aisa and consul-template together | Requests and limits, whether it runs on small clusters | Measure CPU and memory while running parallel streams | — |
| S8 | Can APISIX `forward-auth` send the **model name** (it is in the body) to `/v1/decide`, and can the route then choose the backend from the returned `X-Aisa-Model` header? | Contract 1, including downgrade | Try `forward-auth` with body forwarding; if that fails, a `serverless-pre-function` that copies `model` to a header. Route to `ai-proxy-multi` by header. | **Yes, with an internal hop** (2026-09-28). [Details below](#s8-forward-auth-and-model-routing-on-apisix) |
| S9 | Does consul-template render and reload a standalone `apisix.yaml` cleanly, without dropped requests during a reload? | Contract 3 | Render from a test Consul service, flip its health check, and run a request loop during re-renders | — |
| S10 | Latency added by the forward-auth hop | Whether decision caching in the adapter is ever needed | Compare p50/p95 with and without forward-auth on non-streaming requests | — |

Numbering follows the original design notes; S1, S3 and S4 became unnecessary once authentication and config rendering moved into aisa.

## Outcomes

### S8: forward-auth and model routing on APISIX

**2026-09-28**, APISIX 3.18.0 in standalone mode, dev stack with the stub aisa and mock backends. The configuration, script and full output are in [`adapters/apisix/spikes/s8-forward-auth-model/`](../../adapters/apisix/spikes/s8-forward-auth-model/).

**Result: the APISIX adapter can meet contract 1, including downgrade, with stock plugins.** Routing needs a second hop inside APISIX, because nothing in `ai-proxy-multi` selects an instance by a header.

What was verified:

1. **Model to `/v1/decide`: yes, two ways.** `forward-auth` with `request_method: POST` forwards the whole client body, so aisa can read `model` from it. Alternatively a `serverless-pre-function` copies `model` from the body to `X-Aisa-Requested-Model` and `forward-auth` sends only headers. The header variant is chosen: aisa does not need the prompt, and every request would otherwise send it over another hop. The pre-step overwrites any client-supplied value, so a client cannot claim a different model than it sends.
2. **Routing by the returned header on a single route: no.** Route matching (`vars`) happens before any plugin runs, so a route cannot match on a header that `forward-auth` sets. `ai-proxy-multi` picks its instance with `roundrobin`, `chash` or `semantic` only.
3. **Routing with an internal hop: yes.** The client-facing route runs the pre-step and `forward-auth`, then proxies to a listener bound to `127.0.0.1:9081`. There, one route per model matches `X-Aisa-Model` and holds an `ai-proxy-multi` with the backends of that model. Each instance pins `options.model`, so a downgrade (`X-Aisa-Model: qwen3` for a `cloud-large` request) also rewrites the model in the forwarded body. Streaming passes through unbuffered (first byte after 63 ms of a 590 ms stream), usage chunks arrive, the added latency was below 1 ms at p50, and the internal port is not reachable from other containers.
4. **Re-picking the instance on one route: works, not chosen.** A `serverless-post-function` in the access phase runs after `ai-proxy-multi` has picked an instance and before it sends the request, and can replace `ctx.picked_ai_instance`. This depends on `ai-proxy-multi` internals and bypasses its balancing, health checks and fallback between several backends of the same model.
5. **Spoofing.** `forward-auth` overwrites the headers listed in `upstream_headers` on success, so a client-supplied `X-Aisa-Model` is replaced by aisa's answer. **But with `allow_degradation: true` and aisa unreachable, it returns before clearing them**: a request with an unknown key and `X-Aisa-Model: cloud-large` was served by the paid backend. The pre-step must strip every client-supplied `X-Aisa-*` header before `forward-auth` runs; with that, the same request finds no inner route and gets 404.
6. **Errors.** When aisa is unreachable and degradation is off, `forward-auth` answers 403 by default; the adapter sets `status_on_error: 503`. aisa's own 401 and 429 bodies reach the client unchanged.

Consequences, reflected in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md) and [`ARCHITECTURE.md`](../concepts/ARCHITECTURE.md):

- The decision API reads the requested model from `X-Aisa-Requested-Model`, or from the JSON body when the header is absent, and answers 400 when there is neither.
- Adapter rules: strip client-supplied `X-Aisa-*` headers before the decision, route by `X-Aisa-Model` and forward the request with that model, and answer 503 when aisa is unreachable and the fail policy is closed.
- The APISIX template renders the client-facing route plus one internal route per model. A model-aware instance selector in `ai-proxy-multi` would remove the hop and is listed as an upstream contribution in [`ROADMAP.md`](../../ROADMAP.md).
- The fail policy cannot be per consumer as written: when aisa is unreachable, the gateway does not know who the consumer is. This is filed as a design issue; until it is settled the adapter fails closed.
- Also observed: `ai-proxy-multi` has a 30 s default `timeout`, too short for slow local models; the template must set it. The default `http-logger` format includes the client's `Authorization` header, so the usage log format (S6) must be explicit.
