# Dev stack

A local environment for the spikes in [`docs/process/SPIKES.md`](../docs/process/SPIKES.md) and for integration tests. Everything runs in Docker Compose on linux/amd64 and linux/arm64. It needs no credentials and no network access beyond pulling images.

```bash
docker compose -f dev/compose.yaml up -d --build --wait   # start, wait until seeded and healthy
./dev/smoke.sh                                              # end-to-end check (needs curl and jq)
docker compose -f dev/compose.yaml logs -f stub-aisa        # watch decide calls and usage events
docker compose -f dev/compose.yaml down -v                  # stop and remove everything
```

## Services

| Service | Host port | What it is |
|---|---|---|
| `apisix` | 9080 (gateway), 9091 (Prometheus metrics) | `apache/apisix` in standalone mode, routes from [`apisix/apisix.yaml`](apisix/apisix.yaml) |
| `stub-aisa` | 8081 | Stands in for aisa: `POST /v1/decide`, `POST /v1/usage`, `GET`/`DELETE /debug/requests` |
| `mock-local` | 8082 | Mock OpenAI-compatible backend serving `qwen3` and `llama3.2` (a local Ollama stand-in) |
| `mock-cloud` | 8083 | Mock OpenAI-compatible backend serving `cloud-large` (a paid provider stand-in) |
| `vault` | 8200 | Dev-mode Vault, root token `dev-root`, KV v2 at `secret/` |
| `consul` | 8500 | Dev-mode Consul agent, no ACLs |
| `redis` | 6379 | Redis for hot counters |
| `seed-vault`, `seed-consul` | — | Write the example data below, then stay up so `--wait` covers seeding |

All ports bind to `127.0.0.1`. Tokens and keys are dev-only placeholders.

## Request flow

```
curl ─▶ apisix :9080 /v1/chat/completions
          ├─ forward-auth ── POST /v1/decide (with the request body) ──▶ stub-aisa
          ├─ ai-proxy-multi ──────────────────────────────────────────▶ mock-local
          └─ http-logger ─── POST /v1/usage ───────────────────────────▶ stub-aisa
```

```bash
curl -s localhost:9080/v1/chat/completions \
  -H 'Authorization: Bearer dev-key-chat-ui' -H 'Content-Type: application/json' \
  -d '{"model":"qwen3","messages":[{"role":"user","content":"hello there"}]}'

curl -s 'localhost:8081/debug/requests?kind=decide' | jq   # what forward-auth sent
curl -s 'localhost:8081/debug/requests?kind=usage'  | jq   # what http-logger sent
```

## Seeded data

The data follows the paths in [`VAULT.md`](../docs/concepts/VAULT.md) and [`CONSUL.md`](../docs/concepts/CONSUL.md).

| Where | Path | Contents |
|---|---|---|
| Vault | `secret/aisa/consumers/chat-ui`, `…/batch-jobs` | `key_sha256` of `dev-key-chat-ui` / `dev-key-batch-jobs`, `quota_profile` |
| Vault | `secret/aisa/providers/cloud` | `api_key=dev-provider-key` |
| Consul KV | `aisa/pricing/<model>` | `qwen3` and `llama3.2` free, `cloud-large` priced |
| Consul KV | `aisa/budgets/<consumer>` | `chat-ui` rejects when exhausted, `batch-jobs` downgrades to `qwen3` |
| Consul KV | `aisa/quotas/<profile>` | `interactive`, `batch` |
| Consul catalog | service `aisa-backend` | `mock-local` and `mock-cloud` with `models` meta and HTTP health checks |

## Stub aisa

The stub does not read Vault or Consul; its decisions come from environment variables in [`compose.yaml`](compose.yaml):

| Variable | Format | Dev value |
|---|---|---|
| `STUB_KEYS` | `key=consumer,…` | `dev-key-chat-ui=chat-ui`, `dev-key-batch-jobs=batch-jobs`, `dev-key-blocked=blocked` |
| `STUB_REWRITES` | `consumer:from=to,…` | `batch-jobs:cloud-large=qwen3` (simulates a budget downgrade) |
| `STUB_DENY` | `consumer,…` | `blocked` (gets 429) |
| `STUB_CAPACITY` | positive integer | unset: the last 1000 requests are kept |

`/v1/decide` returns `401` for an unknown key, `400` when no model is found, `429` for a denied consumer, and otherwise `200` with `X-Aisa-Consumer`, `X-Aisa-Model` and `X-Aisa-Budget-Remaining`. The model is read from the `X-Aisa-Requested-Model` header if present, else from the JSON body. `/v1/usage` accepts one JSON object or an array of objects, and rejects anything else with 400. Everything received is logged to stdout as JSON lines and kept (the last `STUB_CAPACITY` requests, 1000 by default) at `/debug/requests?kind=decide|usage`. Credentials (`Authorization`, `Proxy-Authorization`, `Cookie`, API-key headers and fields) are redacted in the records and logs, both in request headers and in JSON bodies; a body that is not JSON is recorded only by its size.

## Mock backend

`GET /v1/models` and `POST /v1/chat/completions`, streaming or not, plus a minimal part of Ollama's native API (`GET /api/version`, non-streaming `POST /api/chat` with `prompt_eval_count` and `eval_count`) so scripts written for a real Ollama run against it. Token counts are deterministic:

- `prompt_tokens` = the number of whitespace-separated words in all message contents (text parts only)
- `completion_tokens` = `MOCK_DEFAULT_COMPLETION_TOKENS` (default 16), lowered by `max_completion_tokens` or `max_tokens` (then `finish_reason` is `length`), or set exactly with the `X-Mock-Completion-Tokens` request header

Streams send one chunk per token. A final usage chunk is sent according to `MOCK_STREAM_USAGE`: `auto` (only with `stream_options.include_usage`, like OpenAI), `always` or `never`. `MOCK_TTFT` and `MOCK_TOKEN_DELAY` add latency (Go durations such as `50ms`). The backend's name is returned in the `X-Mock-Backend` header and in `system_fingerprint`, because gateways may drop upstream response headers.

## Notes

- APISIX re-resolves service names every second (`dns_resolver_valid: 1`), so rebuilding a tool container does not leave it talking to a stale address.
- The default `http-logger` format includes the client's request headers, **including `Authorization`** ([#5](https://github.com/hlan-net/aisa/issues/5)). The baseline route therefore sets an explicit `log_format` until spike S6 defines the usage event schema, and the stub redacts credential fields (`authorization`, `cookie`, API keys) from everything it logs or keeps.
- The tools image ([`Dockerfile`](Dockerfile)) cross-compiles on the build platform, so building it for arm64 on an amd64 machine (or the other way round) needs no emulation.
