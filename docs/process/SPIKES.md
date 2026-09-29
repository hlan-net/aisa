# Spikes: unknowns to verify first

Each spike is a small experiment on a throwaway APISIX instance (standalone mode, one route to an Ollama backend) with a stub aisa, e.g. a 50-line Go HTTP server that logs what it receives. Its result decides a design choice in the concept docs. Record the outcome here, with the date.

Most spikes test whether the **APISIX adapter** can meet the contract in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md). A failed spike means extra adapter work (for example a small Lua pre-step), not a change to the core. The PRs that run them are listed in [`ROADMAP.md`](../../ROADMAP.md) v0.1.0.

| # | Question | Why it matters | How to test | Outcome |
|---|---|---|---|---|
| S2 | Are token counts in the gateway's log data correct for **streaming** responses from Ollama's OpenAI endpoint? | Usage events feed cost and budgets | Compare the logged counts with Ollama's own `eval_count` for streamed and non-streamed requests, with and without `stream_options.include_usage` | **Partly** (2026-09-28): exact through APISIX when the backend streams usage, which a real Ollama does; lost when a backend does not or the client disconnects. [Details](#s2--s6-usage-events-from-apisix) |
| S5 | Does the official `apache/apisix` Helm chart support standalone mode well, or are plain manifests simpler? | Adapter deployment shape | Install the chart with standalone values and check for an etcd dependency | **The chart supports standalone mode, but not the adapter** (2026-09-29): it reads its routes from a ConfigMap only. The adapter uses plain manifests. [Details](#s5-apisix-in-standalone-mode-on-kubernetes) |
| S6 | Can APISIX `http-logger` produce the usage event schema: token counts, model, TTFT and upstream from the AI plugin variables in `log_format`? | Contract 2 | Configure `log_format` with the `llm_*` variables and receive the events in the stub | **Yes** (2026-09-28), with type and latency caveats. [Details](#s2--s6-usage-events-from-apisix) |
| S7 | Resource use on a small arm64 node (e.g. Raspberry Pi 4) of APISIX, aisa and consul-template together | Requests and limits, whether it runs on small clusters | Measure CPU and memory while running parallel streams | **Small enough** (2026-09-28, on a Raspberry Pi 5): about 85 MB idle and 115 MB under 100 streams with one nginx worker. Requests and limits are still to be confirmed on a Raspberry Pi 4 with real aisa ([#16](https://github.com/hlan-net/aisa/issues/16)). [Details](#s7--s10-footprint-and-added-latency) |
| S8 | Can APISIX `forward-auth` send the **model name** (it is in the body) to `/v1/decide`, and can the route then choose the backend from the returned `X-Aisa-Model` header? | Contract 1, including downgrade | Try `forward-auth` with body forwarding; if that fails, a `serverless-pre-function` that copies `model` to a header. Route to `ai-proxy-multi` by header. | **Yes, with an internal hop** (2026-09-28). [Details below](#s8-forward-auth-and-model-routing-on-apisix) |
| S9 | Does consul-template render and reload a standalone `apisix.yaml` cleanly, without dropped requests during a reload? | Contract 3 | Render from a test Consul service, flip its health check, and run a request loop during re-renders | **Yes** (2026-09-28): no request failed in 20 reloads under load, and a change reaches the gateway in 1 to 2 s. The rendered file needs a guard ([#18](https://github.com/hlan-net/aisa/issues/18)). [Details](#s9-config-rendering-with-consul-template) |
| S10 | Latency added by the forward-auth hop | Whether decision caching in the adapter is ever needed | Compare p50/p95 with and without forward-auth on non-streaming requests | **0.3 to 2 ms at p50** (2026-09-28), with a stub that decides from memory: no cache needed for the hop itself. [Details](#s7--s10-footprint-and-added-latency) |

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
- Adapter rules: strip client-supplied `X-Aisa-*` headers before the decision, route by `X-Aisa-Model` and forward the request with that model, and answer 503 when aisa is unreachable and the requested model has no fail-open backend.
- The APISIX template renders the client-facing route plus one internal route per model. A model-aware instance selector in `ai-proxy-multi` would remove the hop and is listed as an upstream contribution in [`ROADMAP.md`](../../ROADMAP.md).
- The fail policy cannot be per consumer as written: when aisa is unreachable, the gateway does not know who the consumer is. Settled in [#4](https://github.com/hlan-net/aisa/issues/4): the fail policy is the backend's Consul service meta `fail_policy`, and a request fails open only to the requested model's `open` backends.
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

### S7 + S10: footprint and added latency

**2026-09-28**, APISIX 3.18.0 in standalone mode, consul-template 0.43.0, dev stack with the stub aisa and mock backends, on a Raspberry Pi 5 (arm64, 4 cores) in Docker. The configuration, script and full output are in [`adapters/apisix/spikes/s7-s10-footprint/`](../../adapters/apisix/spikes/s7-s10-footprint/).

The load was parallel streams of 256 tokens at 50 tokens a second through the adapter shape chosen in S8 (pre-step, `forward-auth`, internal hop, `ai-proxy-multi`, usage event), 30 s per step. Memory is peak proportional set size (PSS), CPU is average cores.

| | APISIX, 1 worker | APISIX, 4 workers | stub aisa | consul-template |
|---|---|---|---|---|
| idle | 0.01 cores, 61 MB | 0.01 cores, 119 MB | 0.01 cores, 8 MB | 0.00 cores, 15 MB |
| 10 streams | 0.06 cores, 72 MB | 0.14 cores, 143 MB | 0.01 cores, 9 MB | 0.00 cores, 15 MB |
| 50 streams | 0.25 cores, 79 MB | 0.39 cores, 169 MB | 0.02 cores, 13 MB | 0.00 cores, 15 MB |
| 100 streams | 0.48 cores, 85 MB | 0.61 cores, 178 MB | 0.03 cores, 13 MB | 0.00 cores, 15 MB |

**S7 result: the three fit on a small node, if the number of nginx workers is set.** No request failed and every stream had its usage event, 598 of 598 at 100 streams with one worker.

1. **The worker count decides the memory.** Each nginx worker has its own Lua VM: 61 MB with one worker, 119 MB with four, idle. nginx starts one worker per core of the *node* by default, whatever CPU limit the container has, so the adapter's deployment must set `nginx_config.worker_processes`.
2. **One worker was enough** for 100 parallel streams, 5000 chunks a second, at half a core. Local models produce far fewer tokens a second than the mock.
3. **Memory grows with load and is not given back**: after the load APISIX stayed at its peak. Limits must allow for the peak, not the idle size.
4. **consul-template costs 15 MB and no measurable CPU** while it watches two backends. The stub aisa's 8 to 14 MB are a floor for aisa, not an estimate: the stub decides from a map in memory and has no Redis, Vault or Consul client.
5. **Not measured on a Raspberry Pi 4, and that does not change the answer.** The memory numbers should carry over, since the images and the architecture are the same. The CPU numbers do not: they are from the faster cores of a Raspberry Pi 5, but the load was far above what a small cluster sees, so slower cores leave room. The numbers that a deployment needs are measured with real aisa on a Raspberry Pi 4 ([#16](https://github.com/hlan-net/aisa/issues/16)).

**S10 result: the hops are cheap.** Non-streamed requests to a backend that answers at once, 600 per row over kept-alive connections, one client at a time:

| Route | p50, 1 worker | p50, 4 workers | APISIX CPU per request |
|---|---|---|---|
| one hop, no decision | 1.4 ms | 2.8 ms | 1.2 to 1.6 ms |
| two hops, no decision | 1.6 ms | 3.3 ms | 1.5 to 2.3 ms |
| two hops and `forward-auth` | 1.9 ms | 4.6 ms | 1.8 to 2.8 ms |

1. **`forward-auth` added 0.3 to 1.3 ms at p50, the internal hop 0.2 to 0.5 ms.** With ten clients at once the additions were 0.6 to 2.1 ms and 0.9 to 2.8 ms, on a machine that also ran the load generator. The differences between runs are as large as the additions, so these are orders of magnitude, not exact costs.
2. **A decision cache in the adapter is not needed for the hop.** What remains is aisa's own time to decide, which the stub does not show: it must be measured when `/v1/decide` exists, and that number decides about caching.

Consequences:

- The APISIX adapter's Helm chart, when it exists ([#16](https://github.com/hlan-net/aisa/issues/16)), must set `worker_processes` explicitly, one worker by default. Starting values for its resources, to be confirmed on a Raspberry Pi 4 ([#16](https://github.com/hlan-net/aisa/issues/16)): requests of 100m CPU and 96Mi memory, a memory limit of 192Mi.
- The APISIX template must not share an `upstream` between routes through a YAML anchor: APISIX adds fields to the upstream it has loaded, and the second route then fails validation and is left out, with an error in the log only. Anchors for plugin configurations work.
- The upstream of the internal hop needs its own read timeout; the default of 60 s would end a slow stream there, as the 30 s of `ai-proxy-multi` does ([#14](https://github.com/hlan-net/aisa/issues/14)).

### S9: config rendering with consul-template

**2026-09-28**, APISIX 3.18.0 in standalone mode, consul-template 0.43.0, dev stack with dev-mode Consul and Vault, the stub aisa and mock backends, on arm64. The template, configuration, script and full output are in [`adapters/apisix/spikes/s9-config-rendering/`](../../adapters/apisix/spikes/s9-config-rendering/).

**Result: yes. consul-template renders the adapter's `apisix.yaml` from Consul and Vault, and APISIX reloads it without dropping a request.**

What was verified:

1. **One template renders the whole adapter**: the client-facing route, one internal route per model with the healthy backends that serve it, the provider key from Vault (KV v2) for the backends that have one, and a route that answers 503 with a readable error for a model that no healthy backend serves.
2. **Reloads are clean.** A backend left and returned ten times under load: 20 reloads, 3792 requests, none failed. Streams that were in flight on a backend when it left the config ran to their end, with exact usage events.
3. **A change reaches the gateway in 1 to 2 s**: consul-template's quiet period of 1 s, and APISIX looks at the file once a second. After that nothing is sent to a backend that left.
4. **A rotated provider key arrives after `default_lease_duration`**: 9 s with the 10 s set in the spike. The default is 5 minutes, since a KV secret has no lease.
5. **When Consul or Vault cannot be reached, the last config stays** and consul-template keeps running and retrying.
6. **A broken file is partly caught.** APISIX keeps its routes when the file is invalid YAML or lacks the final `#END`. **But a file with no routes at all is loaded, and so is a file with one invalid route, without that route**, with an error in the log only: both meant 404 for every request. (The first recording said APISIX kept its routes for an empty file; consul-template had written the good file back before the request.)
7. **The file must be shared as a directory.** consul-template renames a new file over the old one, and a mount of the file itself keeps showing the old one. APISIX's config path is a link into the shared directory.

Also observed, about backends rather than rendering:

- **A backend that dies takes requests with it until Consul notices.** With a health check every 2 s the config changed about 3 s after the container stopped. The requests sent to it in between failed, 18 of 155, and streams in flight on it ended incomplete.
- **Some of those requests were still waiting when the client gave up at 60 s** (`curl -m 60` in `run.sh`); how long the gateway would have held them was not measured. `ai-proxy-multi` has one `timeout` for connecting and for reading, 600 s in the spike, so the 10 minutes that slow models need ([#14](https://github.com/hlan-net/aisa/issues/14)) are expected to be also the time a request can wait for a backend that is gone.

Consequences, reflected in [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md#3-config-rendering-gateway-configuration), [`CONSUL.md`](../concepts/CONSUL.md) and [`VAULT.md`](../concepts/VAULT.md):

- A backend names its provider key with the service meta `key`; backends without it get no key.
- Values from Consul and Vault are rendered as quoted strings, and the rendered file is shared with the gateway as a directory.
- How long requests fail after a backend dies is its health check's interval and timeout, plus the template's quiet period and the gateway's reload; 30 s in the example of `CONSUL.md` means more than half a minute of failures for a share of the requests. Requests already sent to it can wait until the gateway's timeout.
- The template must set `default_lease_duration`, or a rotated key takes 5 minutes to arrive.
- Found in review: `ai-proxy-multi` forwards the client's `Authorization` to a backend without a key, the internal hop has a request id of its own, and a denied request never reaches the internal route's logger. The template removes the header, passes the decision's id along and logs denials on the client route.
- Open: nothing checks the rendered file before the gateway loads it, a file without routes takes every route away, and a catalog that comes back empty renders a config without backends. Both need a guard in the adapter ([#18](https://github.com/hlan-net/aisa/issues/18)).

### S5: APISIX in standalone mode on Kubernetes

**2026-09-29**, the official chart `apisix/apisix` 2.17.0 (APISIX 3.18.0) and plain manifests with consul-template 0.43.0, on k3s 1.36 with Raspberry Pi 4 nodes (arm64, 4 cores, 8 GB) and a real Ollama 0.31.1 outside the cluster. The values, manifests, script and full output are in [`adapters/apisix/spikes/s5-helm-chart/`](../../adapters/apisix/spikes/s5-helm-chart/).

**Result: the official chart runs APISIX in standalone mode without etcd, but it cannot carry the adapter. The adapter is deployed with plain manifests.**

What was verified:

1. **The chart supports standalone mode.** With `apisix.deployment.mode: standalone` and `etcd.enabled: false` it renders no etcd; a Secret with an empty password and an environment variable are left of it. The number of workers and the internal listener of the two-hop route are values. Requests to Ollama worked, streamed and not.
2. **The chart reads its routes from a ConfigMap only.** It mounts the ConfigMap at a fixed path and links `apisix.yaml` to it. A directory that a sidecar renders into cannot be mounted at that path: Kubernetes rejects the second mount. So the chart leaves no place for consul-template, and provider keys would be in a ConfigMap, which the cluster stores.
3. **A change of the ConfigMap reached APISIX after 17 to 61 s** in four tries, the time the kubelet takes to update a mounted ConfigMap. `helm upgrade` does not replace the pod for it. No request failed.
4. **Plain manifests carry the adapter as designed.** APISIX and consul-template run in one pod and share a directory in memory. An init container renders once, because APISIX does not start without an `apisix.yaml`. A request passed the client-facing route, the decision, the internal route, Ollama and the usage event; the pod was ready 5 s after it was applied and 5 s after it was replaced.
5. **A change in Consul reached the rendered file after 2 to 3 s**, measured through the Kubernetes API. APISIX reads the file once a second (S9), so the change reaches APISIX about a second later; this part did not measure that through requests, unlike the chart's 17 to 61 s. A backend left and returned five times under a request loop, and no request was answered otherwise than expected. The loop asked for a model that no backend serves, whose route stays in place; requests to a route that changes were tested under reloads in S9, not here.
6. **Model names are not route ids.** An APISIX route id takes letters, digits, `-`, `_` and `.`; `llama3.2:3b` has a colon. APISIX left that model's route out, with an error in its log only, and requests for the model got the 503 of "no healthy backend". The template now replaces the other characters and appends a hash of the name.
7. **On a Raspberry Pi 4, APISIX with one worker had a working set of 46 to 49 MiB** after a few requests, and consul-template 3 to 4 MiB. This is not a measurement under load; it is the first number from the hardware that [#16](https://github.com/hlan-net/aisa/issues/16) asks about, and it is below the 61 MB of proportional set size on a Raspberry Pi 5 (S7), which counts differently.

Consequences, reflected in [`ARCHITECTURE.md`](../concepts/ARCHITECTURE.md) and [`ADAPTER_CONTRACT.md`](../concepts/ADAPTER_CONTRACT.md#3-config-rendering-gateway-configuration):

- The APISIX adapter ships manifests, or a small chart of its own, with APISIX and consul-template in one pod. It does not build on the official chart.
- A template makes the names it takes from Consul valid for the gateway before it uses them as identifiers.
- Not tested here: consul-template with Consul's ACLs and with Vault's Kubernetes auth, and the adapter's NetworkPolicy. They belong to `feature/apisix-adapter`.
