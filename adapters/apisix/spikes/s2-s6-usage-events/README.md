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
| [`ollama.sh`](ollama.sh) | Compares Ollama's native counts (`prompt_eval_count`, `eval_count`) with its OpenAI endpoint and with the APISIX usage event, streamed and not. Set `GATEWAY_OLLAMA_URL` when the containers reach Ollama by another address, e.g. `http://172.17.0.1:11434` (Docker's default bridge gateway) for an Ollama on the Docker host that listens on all interfaces |

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

## Output against a real Ollama (2026-09-28, APISIX 3.18.0, arm64)

Ollama 0.31.1 with CPU inference on the Docker host, a Raspberry Pi 5. Each model was run with the default prompt; `llama3.2:3b` also with a prompt whose answer reaches the limit.

```
Ollama 0.31.1, model llama3.2:3b, limit 32 tokens
path                                                   prompt  completion
native /api/chat                                           36           6
direct /v1, non-streaming                                  36           6
direct /v1, streaming, include_usage                       36           6
direct /v1, streaming, no include_usage                     -           -
APISIX usage event, non-streaming                          36           6
APISIX usage event, streaming, include_usage               36           6
APISIX usage event, streaming, no include_usage            36           6
```

| Model | Limit | Native | Direct `/v1` (non-streamed, streamed with `include_usage`) | Direct `/v1` streamed, no `include_usage` | APISIX usage events (all three) |
|---|---|---|---|---|---|
| `llama3.2:3b` | 32 | 36 / 6 | 36 / 6 | no usage | 36 / 6 |
| `llama3.2:3b`, long answer | 24 | 35 / 24 | 35 / 24 | no usage | 35 / 24 |
| `deepseek-r1:1.5b` | 48 | 14 / 48 | 14 / 48 | no usage | 14 / 48 |
| `qwen3` | 32 | 21 / 32 | 21 / 32 | no usage | 21 / 32 |

Two more observations from the same session:

- **Timeout.** With the default `timeout` of `ai-proxy-multi` (30 s), the non-streamed requests to `deepseek-r1:1.5b` and `qwen3` ended in 504, and `ollama.sh` stopped there. [`apisix.yaml`](apisix.yaml) therefore sets `timeout: 600000`, the plugin's maximum ([#14](https://github.com/hlan-net/aisa/issues/14)). The event of such a request:

  ```json
  {"backend":"mock-local","completion_tokens":"0","consumer":"chat-ui","latency_ms":30001,"model":"deepseek-r1:1.5b","prompt_tokens":"0","requested_model":"deepseek-r1:1.5b","status":504,"stream":"false","ttft_ms":"0"}
  ```

  A streamed request is not cut off while data keeps arriving: with the default timeout, a stream of 250 tokens to `llama3.2:3b` took 68 s and was logged as 35 / 250.

- **Client disconnect.** A streamed request to `llama3.2:3b` through APISIX with `curl --max-time 40` received 149 content chunks and no usage chunk. Its event had `"prompt_tokens":"0","completion_tokens":"0","status":200`, like the `GAP` check against the mock.

`backend` reads `mock-local` in these events because `ollama.sh` changes only the endpoint of that instance, not its name. The host served other inference requests at the same time, so the latencies are not a measure of the hardware.
