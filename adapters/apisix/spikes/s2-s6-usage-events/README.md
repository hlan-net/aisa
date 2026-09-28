# Spikes S2 + S6: usage events from APISIX

The questions and outcomes are recorded in [`docs/process/SPIKES.md`](../../../../docs/process/SPIKES.md#s2--s6-usage-events-from-apisix). This directory holds what they are based on, so they can be re-run against a newer APISIX or a real Ollama.

```bash
./adapters/apisix/spikes/s2-s6-usage-events/run.sh      # against the mock backends
OLLAMA_URL=http://gpu-box.internal:11434 MODEL=qwen3 \
  ./adapters/apisix/spikes/s2-s6-usage-events/ollama.sh # against a real Ollama
docker compose -f dev/compose.yaml -f adapters/apisix/spikes/s2-s6-usage-events/compose.override.yaml down -v
```

| File | Contents |
|---|---|
| [`apisix.yaml`](apisix.yaml) | Two routes with the `log_format` proposed for the adapter: `/u/` to `mock-local`, `/n/` to `mock-nousage` |
| [`compose.override.yaml`](compose.override.yaml) | Adds `mock-nousage`, a mock backend that never streams usage |
| [`run.sh`](run.sh) | One request per scenario; checks its usage event against the mock's deterministic counts. `GAP` checks assert today's known-wrong behaviour, so a change shows up. Exits non-zero on any unexpected result |
| [`ollama.sh`](ollama.sh) | Compares Ollama's native counts (`prompt_eval_count`, `eval_count`) with its OpenAI endpoint and with the APISIX usage event, streamed and not. Set `GATEWAY_OLLAMA_URL` when the containers reach Ollama by another address |

## The proposed `log_format`

| Event field | APISIX variable | Note |
|---|---|---|
| `ts` | `$time_iso8601` | when the request was logged |
| `request_id` | `$apisix_request_id` | `forward-auth` sends the same value to `/v1/decide` via `extra_headers: {X-Request-Id: $apisix_request_id}` |
| `consumer` | `$http_x_aisa_consumer` | set by `forward-auth` from aisa's answer |
| `model` / `requested_model` | `$llm_model` / `$request_llm_model` | |
| `backend` | `$balancer_ip` | `ai-proxy-multi` stores the chosen instance's name there |
| `prompt_tokens` / `completion_tokens` | `$llm_prompt_tokens` / `$llm_completion_tokens` | numbers, or `"0"` when no usage was seen |
| `status` | `$status` | |
| `latency_ms` | `$apisix_upstream_response_time` | milliseconds at the backend; `$request_time` is in seconds and log formats cannot convert |
| `ttft_ms` | `$llm_time_to_first_token` | |
| `stream` | `$llm_stream` | the string `"true"` or `"false"` |

## Output of the recorded run (2026-09-28, APISIX 3.18.0, amd64)

```
== S6: the usage event schema, non-streaming (mock: 3 prompt, 16 completion tokens)
  ok    status                                                         200
  ok    prompt/completion tokens                                       3/16
  ok    tokens are JSON numbers                                        number
  ok    consumer (from X-Aisa-Consumer)                                chat-ui
  ok    model / requested_model                                        qwen3/qwen3
  ok    backend (ai-proxy-multi instance name)                         mock-local
  ok    stream                                                         false
  ok    ts is ISO 8601                                                 true
  ok    latency_ms and ttft_ms are numbers                             number/number
  ok    ttft_ms ≥ the mock's 50 ms TTFT                              true
  ok    request_id matches the one /v1/decide received                 c3923247d39d684630a20339b82c3310

== S2: token counts for streamed responses
  ok    stream, client asks for usage: tokens                          3/16
  ok    stream: ttft_ms < latency_ms                                   true
  ok    stream, client does not ask for usage: tokens                  3/16
  ok      ... APISIX added include_usage: client still gets a usage chunk 3/16
  ok    stream with max_tokens 3: tokens                               3/3
  ok    GAP backend sends no streamed usage: tokens logged as          0/0
  ok    GAP   ... as strings, not numbers                              string
  ok      ... although the client received tokens                      true
  ok    same backend, non-streaming: tokens                            3/16
  ok    GAP client disconnects mid-stream: tokens logged as            0/0
  ok      ... although the client had received more than 10 tokens     true
  ok      ... status                                                   200

== Events for requests that never reach a backend, or fail there
  ok    backend answers 404: status / tokens                           404 0/0
  ok    aisa denies (429): status / tokens / backend                   429 0/0 null

all checks as expected
```

`ollama.sh` was run only against the mock (with `OLLAMA_URL=http://127.0.0.1:8082 GATEWAY_OLLAMA_URL=http://mock-local:8080`) to check the script; the real-Ollama run is still to be done.
