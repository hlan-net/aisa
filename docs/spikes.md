# Spikes: unknowns to verify first

Each spike is a small experiment on a throwaway APISIX pod (standalone mode, one route to an Ollama backend) with a stub aisa, e.g. a 50-line Go HTTP server that logs what it receives. Its result decides a design choice in the other docs. Record the outcome here, with the date.

Most spikes test whether the **APISIX adapter** can meet the contract in [interfaces.md](interfaces.md). A failed spike means extra adapter work (for example a small Lua pre-step), not a change to the core.

| # | Question | Why it matters | How to test | Outcome |
|---|---|---|---|---|
| S2 | Are token counts in the gateway's log data correct for **streaming** responses from Ollama's OpenAI endpoint? | Usage events feed cost and budgets | Compare the logged counts with Ollama's own `eval_count` for streamed and non-streamed requests, with and without `stream_options.include_usage` | — |
| S5 | Does the official `apache/apisix` Helm chart support standalone mode well, or are plain manifests simpler? | Adapter deployment shape | Install the chart with standalone values and check for an etcd dependency | — |
| S6 | Can APISIX `http-logger` produce the usage event schema: token counts, model, TTFT and upstream from the AI plugin variables in `log_format`? | Contract 2 | Configure `log_format` with the `llm_*` variables and receive the events in the stub | — |
| S7 | Resource use on a small arm64 node (e.g. Raspberry Pi 4) of APISIX, aisa and consul-template together | Requests and limits, whether it runs on small clusters | `kubectl top` while running parallel streams | — |
| S8 | Can APISIX `forward-auth` send the **model name** (it is in the body) to `/v1/decide`, and can the route then choose the backend from the returned `X-Aisa-Model` header? | Contract 1, including downgrade | Try `forward-auth` with body forwarding; if that fails, a `serverless-pre-function` that copies `model` to a header. Route to `ai-proxy-multi` by header. | — |
| S9 | Does consul-template render and reload a standalone `apisix.yaml` cleanly, without dropped requests during a reload? | Contract 3 | Render from a test Consul service, flip its health check, and run a request loop during re-renders | — |
| S10 | Latency added by the forward-auth hop | Whether decision caching in the adapter is ever needed | Compare p50/p95 with and without forward-auth on non-streaming requests | — |

Numbering follows the original design notes; S1, S3 and S4 became unnecessary once authentication and config rendering moved into aisa.
