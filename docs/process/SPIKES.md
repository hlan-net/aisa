# Spikes: unknowns to verify first

Each spike is a small experiment on a throwaway APISIX instance (standalone mode, one route to an Ollama backend) with a stub aisa, e.g. a 50-line Go HTTP server that logs what it receives. Its result decides a design choice in the concept docs. Record the outcome here, with the date.

Most spikes test whether the **APISIX adapter** can meet the contract in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md). A failed spike means extra adapter work (for example a small Lua pre-step), not a change to the core. The PRs that run them are listed in [`ROADMAP.md`](../../ROADMAP.md) v0.1.0.

| # | Question | Why it matters | How to test | Outcome |
|---|---|---|---|---|
| S2 | Are token counts in the gateway's log data correct for **streaming** responses from Ollama's OpenAI endpoint? | Usage events feed cost and budgets | Compare the logged counts with Ollama's own `eval_count` for streamed and non-streamed requests, with and without `stream_options.include_usage` | **Partly** (2026-09-28): exact through APISIX when the backend streams usage, which a real Ollama does; lost when a backend does not or the client disconnects. [Details](#s2--s6-usage-events-from-apisix) |
| S5 | Does the official `apache/apisix` Helm chart support standalone mode well, or are plain manifests simpler? | Adapter deployment shape | Install the chart with standalone values and check for an etcd dependency | — |
| S6 | Can APISIX `http-logger` produce the usage event schema: token counts, model, TTFT and upstream from the AI plugin variables in `log_format`? | Contract 2 | Configure `log_format` with the `llm_*` variables and receive the events in the stub | **Yes** (2026-09-28), with type and latency caveats. [Details](#s2--s6-usage-events-from-apisix) |
| S7 | Resource use on a small arm64 node (e.g. Raspberry Pi 4) of APISIX, aisa and consul-template together | Requests and limits, whether it runs on small clusters | Measure CPU and memory while running parallel streams | — |
| S8 | Can APISIX `forward-auth` send the **model name** (it is in the body) to `/v1/decide`, and can the route then choose the backend from the returned `X-Aisa-Model` header? | Contract 1, including downgrade | Try `forward-auth` with body forwarding; if that fails, a `serverless-pre-function` that copies `model` to a header. Route to `ai-proxy-multi` by header. | **Yes, with an internal hop** (2026-09-28). [Details below](#s8-forward-auth-and-model-routing-on-apisix) |
| S9 | Does consul-template render and reload a standalone `apisix.yaml` cleanly, without dropped requests during a reload? | Contract 3 | Render from a test Consul service, flip its health check, and run a request loop during re-renders | **Yes** (2026-09-28): no request failed in 20 reloads under load, and a change reaches the gateway in 1 to 2 s. The rendered file needs a guard ([#18](https://github.com/hlan-net/aisa/issues/18)). [Details](#s9-config-rendering-with-consul-template) |
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
5. **Spoofing.** `forward-auth` overwrites the headers listed in `upstream_headers` on success, so a client-supplied `X-Aisa-Model` is replaced by aisa's answer. **But with `allow_degradation: true` and aisa unreachable, it returns before clearing them**: a request with an unknown key and `X-Aisa-Model: cloud-large` was served by the paid backend. The pre-step must strip every client-supplied `X-Aisa-*` header before `forward-auth` runs; with that, the same request finds no inner route and gets 404. The chosen two-hop route strips them too, so on a successful decision only aisa's `X-Aisa-Consumer` and `X-Aisa-Model` reach the inner route (verified from its usage events).
6. **Errors.** When aisa is unreachable and degradation is off, `forward-auth` answers 403 by default; with `status_on_error: 503` on the client-facing route it answers 503 (verified on the two-hop variant). aisa's own 401 and 429 bodies reach the client unchanged.

Consequences, reflected in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md) and [`ARCHITECTURE.md`](../concepts/ARCHITECTURE.md):

- The decision API reads the requested model from `X-Aisa-Requested-Model`, or from the JSON body when the header is absent, and answers 400 when there is neither.
- Adapter rules: strip client-supplied `X-Aisa-*` headers before the decision, route by `X-Aisa-Model` and forward the request with that model, and answer 503 when aisa is unreachable and the fail policy is closed.
- The APISIX template renders the client-facing route plus one internal route per model. A model-aware instance selector in `ai-proxy-multi` would remove the hop and is listed as an upstream contribution in [`ROADMAP.md`](../../ROADMAP.md).
- The fail policy cannot be per consumer as written: when aisa is unreachable, the gateway does not know who the consumer is. This is filed as a design issue; until it is settled the adapter fails closed.
- Also observed: `ai-proxy-multi` has a 30 s default `timeout`, too short for slow local models; the template must set it. The default `http-logger` format includes the client's `Authorization` header, so the usage log format (S6) must be explicit.

### S2 + S6: usage events from APISIX

**2026-09-28**, APISIX 3.18.0 in standalone mode, dev stack with the stub aisa and mock backends. The configuration, scripts and full output are in [`adapters/apisix/spikes/s2-s6-usage-events/`](../../adapters/apisix/spikes/s2-s6-usage-events/).

**S6 result: yes.** An explicit `http-logger` `log_format` produces every field of the usage event schema from APISIX variables, and nothing else, so no credential reaches the sink. The mapping is in the spike's README. Details that shape the contract:

1. **Correlation works.** `forward-auth` can send `$apisix_request_id` to `/v1/decide` as `X-Request-Id` (`extra_headers`), and the usage event logs the same value.
2. **The backend's name is available** as `$balancer_ip`, where `ai-proxy-multi` stores the chosen instance's name. aisa can look up provider and prices from it, so `provider` need not come from the gateway.
3. **Types are not stable.** Token counts are JSON numbers when usage was seen and the strings `"0"` when not; `stream` is `"true"`/`"false"`. aisa's ingest must accept numeric and boolean strings.
4. **Latency in milliseconds** comes from `$apisix_upstream_response_time` (time at the backend, including the whole stream). `$request_time` is in seconds, and a log format cannot convert units. `$llm_time_to_first_token` is in milliseconds.
5. **Every request is logged**, including aisa's 401/429 (no tokens, no backend) and backend errors (their status, zero tokens). A denied request keeps any `X-Aisa-Consumer` the client sent, and that value would be logged as its consumer, so the adapter's pre-step must strip client-supplied `X-Aisa-*` headers (rule 1 from S8) on every route that logs usage.

**S2 result: partly, and it found a gap.** Against the mock backends, whose counts are known exactly:

1. **Exact when the backend streams usage.** APISIX adds `stream_options.include_usage` to streamed OpenAI-style requests itself, so counts are right even when the client did not ask for usage. A side effect: such clients now receive a final usage chunk they did not request (valid OpenAI streaming, but new to them).
2. **Lost when the backend does not stream usage.** The event reports `"0"`/`"0"` with status 200 although 16 tokens were streamed. The same backend's non-streamed responses are exact.
3. **Lost when the client disconnects mid-stream.** `"0"`/`"0"` with status 200, after more than 10 tokens had reached the client.
4. The planned `AisaUsageEventsLost` alert compares aisa with the gateway's own metrics, which miss the same usage, so it cannot catch 2 or 3.

**Against a real Ollama** (2026-09-28, Ollama 0.31.1 with CPU inference on a Raspberry Pi 5, arm64; `ollama.sh` with `llama3.2:3b`, `deepseek-r1:1.5b` and `qwen3`):

1. **Ollama honours `include_usage`, and the counts are exact.** For all three models the usage event from APISIX equals Ollama's native `prompt_eval_count` and `eval_count`, non-streamed and streamed, whether the answer ended by itself or at `max_tokens`. Ollama's OpenAI endpoint reports the same numbers as its native API.
2. **Without `include_usage` Ollama streams no usage.** Asked directly, a streamed request without it returns no counts. Through APISIX the counts are still exact, because APISIX adds the option (result 1 above). An adapter for a gateway that does not must add it.
3. **Client disconnect: the gap is real.** A client that left after 149 streamed tokens produced an event with `"0"`/`"0"` and status 200. Ollama stopped generating when the connection closed.
4. **The 30 s default `timeout` of `ai-proxy-multi` is too short** (as suspected in S8). Non-streamed requests to `deepseek-r1:1.5b` and `qwen3` ended in 504 after 30 s, with an event of status 504 and no tokens, and Ollama stopped generating. With `timeout: 600000`, the plugin's maximum, the same requests succeeded. The timeout limits the wait for data, not the whole response: with the default, a stream of 68 s (250 tokens) completed with exact counts. The APISIX template must set the timeout, and a non-streamed answer that takes longer than 10 minutes cannot be served ([#14](https://github.com/hlan-net/aisa/issues/14)).

The Ollama host served other inference requests during the run, so the latencies above show what happened, not what the hardware can do; S7 and S10 measure performance.

**Against hailo-ollama, a backend without usage** (2026-09-28, hailo-ollama 0.5.1 on a Hailo-10H accelerator, arm64; `qwen2.5:1.5b` and `qwen3:1.7b`). It offers a part of Ollama's API, and it is the "backend sends no usage" case of the mock on real hardware:

1. **Its OpenAI endpoint reports no usage and does not stream.** A non-streamed answer has no `usage` field, and through APISIX its event has status 200 with `prompt_tokens: 0` and `completion_tokens: 0`, as numbers this time, not the strings `"0"`. A streamed request is answered with 400.
2. **Its native `/api/chat` streams and reports `eval_count`, but no `prompt_eval_count`.** Streamed, `eval_count` equals the number of content chunks; non-streamed it is one higher for the same answer (33 and 32, 65 and 64). Prompt tokens are not available from this backend at all.
3. **It serves one request and one model at a time.** A second request waits for the first, a change of model took 8 to 12 s, and `total_duration` contains neither wait, so latency must be measured at the gateway. A client disconnect stops the generation.
4. **LiteLLM in front of it closes most of the gap.** LiteLLM 1.103.0 (`ollama_chat/` provider) takes the OpenAI-style request from APISIX and calls `/api/chat`. Non-streamed and streamed requests then work, and the usage events have completion tokens equal to the backend's `eval_count` for both models. **Prompt tokens are LiteLLM's own estimate** (17 for a prompt of 8 words); nothing in this setup can check them. It needed one setting: hailo-ollama rejects every `Content-Type` except `application/json` with a 500, and LiteLLM sends `application/octet-stream` to an Ollama backend unless `extra_headers` sets it.
5. **Client disconnect is not fixed by this.** With LiteLLM in between, a client that left after 19 tokens still produced an event with `"0"`/`"0"` and status 200.

Consequences, reflected in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md#2-usage-events-after-the-request): the field meanings above, lenient parsing of numbers and booleans, `X-Request-Id` as a decision input, events for denied requests, no credentials in events (#5), and "a successful response without token counts means usage unknown, not zero", whether the zeros arrive as numbers or as strings. A backend that reports no usage needs a translating proxy in front of it, and its prompt tokens are then estimates. How aisa accounts for unknown usage is [#12](https://github.com/hlan-net/aisa/issues/12).

### S9: config rendering with consul-template

**2026-09-28**, APISIX 3.18.0 in standalone mode, consul-template 0.43.0, dev stack with dev-mode Consul and Vault, the stub aisa and mock backends, on arm64. The template, configuration, script and full output are in [`adapters/apisix/spikes/s9-config-rendering/`](../../adapters/apisix/spikes/s9-config-rendering/).

**Result: yes. consul-template renders the adapter's `apisix.yaml` from Consul and Vault, and APISIX reloads it without dropping a request.**

What was verified:

1. **One template renders the whole adapter**: the client-facing route, one internal route per model with the healthy backends that serve it, the provider key from Vault (KV v2) for the backends that have one, and a route that answers 503 with a readable error for a model that no healthy backend serves.
2. **Reloads are clean.** A backend left and returned ten times under load: 20 reloads, 3792 requests, none failed. Streams that were in flight on a backend when it left the config ran to their end, with exact usage events.
3. **A change reaches the gateway in 1 to 2 s**: consul-template's quiet period of 1 s, and APISIX looks at the file once a second. After that nothing is sent to a backend that left.
4. **A rotated provider key arrives after `default_lease_duration`**: 9 s with the 10 s set in the spike. The default is 5 minutes, since a KV secret has no lease.
5. **When Consul or Vault cannot be reached, the last config stays** and consul-template keeps running and retrying.
6. **A broken file is partly caught.** APISIX keeps its routes when the file is invalid YAML, lacks the final `#END` or has no routes at all. **But a file with one invalid route is loaded without that route**, with an error in the log only: an invalid client-facing route meant 404 for every request.
7. **The file must be shared as a directory.** consul-template renames a new file over the old one, and a mount of the file itself keeps showing the old one. APISIX's config path is a link into the shared directory.

Also observed, about backends rather than rendering:

- **A backend that dies takes requests with it until Consul notices.** With a health check every 2 s the config changed about 3 s after the container stopped. The requests sent to it in between failed, 18 of 155, and streams in flight on it ended incomplete.
- **Those requests hung for more than 60 s.** `ai-proxy-multi` has one `timeout` for connecting and for reading. The 10 minutes that slow models need ([#14](https://github.com/hlan-net/aisa/issues/14)) are then also the time a request waits for a backend that is gone.

Consequences, reflected in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md#3-config-rendering-gateway-configuration), [`CONSUL.md`](../concepts/CONSUL.md) and [`VAULT.md`](../concepts/VAULT.md):

- A backend names its provider key with the service meta `key`; backends without it get no key.
- Values from Consul and Vault are rendered as quoted strings, and the rendered file is shared with the gateway as a directory.
- The health check interval of a backend decides how long requests fail after it dies; 30 s in the example of `CONSUL.md` means up to half a minute of failures for a share of the requests.
- The template must set `default_lease_duration`, or a rotated key takes 5 minutes to arrive.
- Open: nothing checks the rendered file before the gateway loads it, and a catalog that comes back empty renders a config without backends. Both need a guard in the adapter ([#18](https://github.com/hlan-net/aisa/issues/18)).
