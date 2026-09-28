# Spike S8: model name to `/v1/decide`, routing by `X-Aisa-Model`

The question and the outcome are recorded in [`docs/process/SPIKES.md`](../../../../docs/process/SPIKES.md#s8-forward-auth-and-model-routing-on-apisix). This directory holds what the outcome is based on, so it can be re-run against a newer APISIX.

```bash
./adapters/apisix/spikes/s8-forward-auth-model/run.sh   # starts the dev stack with this config
docker compose -f dev/compose.yaml down -v              # afterwards
```

| File | Contents |
|---|---|
| [`config.yaml`](config.yaml) | The dev config plus an internal listener on `127.0.0.1:9081` |
| [`apisix.yaml`](apisix.yaml) | One route (or route set) per variant, `/a/` … `/f2/` |
| [`run.sh`](run.sh) | Sends requests through each variant and checks what the stub aisa and the mock backends saw; exits non-zero unless exactly the checks of c and f1 fail |

## Variants

| Variant | Model to `/v1/decide` | Backend chosen by | Result |
|---|---|---|---|
| a | forward-auth `POST`: the whole request body | — | works; aisa receives the full prompt |
| b | `serverless-pre-function` copies `model` to `X-Aisa-Requested-Model`, forward-auth `GET` | — | works; aisa receives only headers |
| c | — | route `vars` on `X-Aisa-Model` | **fails**: routes are matched before forward-auth runs, so the header is not there yet (404) |
| d | as b | outer route → `127.0.0.1:9081` → inner route per model matching `X-Aisa-Model`, each with its own `ai-proxy-multi` | works, stock plugins only |
| e | as a | one route; `serverless-post-function` replaces the instance `ai-proxy-multi` picked | works, but relies on `ai-proxy-multi` internals (`ctx.picked_ai_instance`) |
| f1 | as a | as d, with `allow_degradation: true` | **unsafe**: with aisa unreachable, a client-supplied `X-Aisa-Model` reaches the paid backend |
| f2 | as a | as f1, the pre-step strips client `X-Aisa-*` headers | safe: no header, no inner route, 404 |

The pre-steps of b, d and f2 strip every client-supplied `X-Aisa-*` header, d's `forward-auth` sets `status_on_error: 503`, and every `http-logger` has an explicit `log_format` without request headers, as the adapter rules in `ADAPTER_CONTRACT.md` require. f1 keeps the unsafe configuration on purpose.

## Output of the recorded run (2026-09-28, APISIX 3.18.0, amd64)

The mock backend's TTFT is 50 ms, so the latency figures are dominated by it; what matters is the difference between variants. Compose progress lines are omitted.

```
== Part 1: does the model reach /v1/decide?
variant a
  ok    request succeeds                                           200
  ok    decide saw the model                                       qwen3
  ok    model source                                               body
        decide received: {"model":"qwen3","model_source":"body","return_model":"qwen3","status":200,"body_bytes":67}
  ok    spoofed X-Aisa-Requested-Model ignored                     qwen3
variant b
  ok    request succeeds                                           200
  ok    decide saw the model                                       qwen3
  ok    model source                                               header
        decide received: {"model":"qwen3","model_source":"header","return_model":"qwen3","status":200,"body_bytes":4}
  ok    spoofed X-Aisa-Requested-Model ignored                     qwen3

== Part 2: can the route pick the backend from X-Aisa-Model?
variant c (route vars)
  FAIL  route matching sees forward-auth's header                  want 200, got 404
variant d
  ok    qwen3 → mock-local                                       mock-local qwen3
  ok    cloud-large → mock-cloud                                 mock-cloud cloud-large
  ok    downgrade: batch-jobs cloud-large → mock-local qwen3     mock-local qwen3
  ok    spoofed X-Aisa-Model overwritten by forward-auth           mock-local qwen3
  ok    model without a backend is rejected                        404
  ok    denied consumer still gets 429                             429
  ok    client X-Aisa-* headers stripped before the inner route    0
  ok    streaming: usage chunk arrives                             100
  ok    streaming: first byte before half of total (not buffered)  yes
        first byte / total (s): 0.067335 0.592557
variant e
  ok    qwen3 → mock-local                                       mock-local qwen3
  ok    cloud-large → mock-cloud                                 mock-cloud cloud-large
  ok    downgrade: batch-jobs cloud-large → mock-local qwen3     mock-local qwen3
  ok    spoofed X-Aisa-Model overwritten by forward-auth           mock-local qwen3
  ok    model without a backend is rejected                        400
  ok    denied consumer still gets 429                             429
  ok    streaming: usage chunk arrives                             100
  ok    streaming: first byte before half of total (not buffered)  yes
        first byte / total (s): 0.064412 0.589995
variant d: the internal listener is loopback-only
  ok    apisix:9081 unreachable from the network (000 = refused)   000

== Usage events (http-logger on the routes that call ai-proxy-multi in d and e)
      1   route=d-inner-cloud-large status=200 upstream=mock-cloud:8080 consumer=chat-ui model=cloud-large
      1   route=d-inner-qwen3 status=200 upstream=mock-local:8080 consumer=batch-jobs model=qwen3
      4   route=d-inner-qwen3 status=200 upstream=mock-local:8080 consumer=chat-ui model=qwen3
      1   route=e status=200 upstream=mock-cloud:8080 consumer=chat-ui model=cloud-large
      1   route=e status=200 upstream=mock-local:8080 consumer=batch-jobs model=qwen3
      3   route=e status=200 upstream=mock-local:8080 consumer=chat-ui model=qwen3
      1   route=e status=400 upstream=- consumer=chat-ui model=llama3.2
      1   route=e status=429 upstream=- consumer=- model=-
  ok    no consumer key in the usage events                        0

== Latency: 50 sequential non-streaming requests per variant, median and p95 of total time (ms)
  a  p50 54.6  p95 56.8
  b  p50 54.0  p95 55.9
  d  p50 54.6  p95 56.6
  e  p50 53.9  p95 58.8

== Part 3: fail open (allow_degradation) while aisa is unreachable
variant f1
  FAIL  unknown key + client X-Aisa-Model: cloud-large is not served want 404, got 200
        status 200, served by: mock-cloud cloud-large
variant f2
  ok    unknown key + client X-Aisa-Model: cloud-large is not served 404
        status 404, served by: - -
  ok    fail closed, forward-auth default: status                  403
  ok    fail closed, d with status_on_error: 503                   503

all checks as expected (2 expected failures: variants c and f1)
```
