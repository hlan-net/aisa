# Roadmap

## Mission

Keep LLM usage in check without replacing the gateway you already run.
aisa gives every application an identity, a quota and a budget, and keeps the books, using the Vault and Consul you already operate.

---

## Current state: v0.1.0 released

- Architecture and the three aisa ↔ gateway contracts are written down ([docs/concepts/](docs/concepts/))
- Quotas, money budgets and usage metrics are specified ([docs/features/](docs/features/))
- The spikes of v0.1.0 are answered ([docs/process/SPIKES.md](docs/process/SPIKES.md)); S2 in part ([#12](https://github.com/hlan-net/aisa/issues/12))
- Local dev stack with a mock backend and a stub aisa ([dev/](dev/))
- aisa authenticates consumers against Vault with `/v1/decide`, ingests usage events at `/v1/usage`, enforces token quotas from Consul with counters in Redis and exports its metrics; it does not enforce budgets yet ([cmd/aisa/](cmd/aisa/))
- Each `v*` tag publishes a GitHub release and a multi-arch image on GHCR (`ghcr.io/hlan-net/aisa`)

---

## v0.1.0: Spikes and dev environment

Answer the open questions before writing the core. Everything runs locally: dev-mode Vault and Consul, APISIX in standalone mode, Redis and a mock OpenAI-compatible backend.

| PR | Branch | Scope | Status |
|----|--------|-------|--------|
| 1 | `feature/dev-environment` | Docker Compose dev stack, mock backend, stub aisa that logs what it receives | Done ([#7](https://github.com/hlan-net/aisa/pull/7)) |
| 2 | `spike/forward-auth-model` | S8: model name to `/v1/decide`, routing by `X-Aisa-Model` | Done ([#8](https://github.com/hlan-net/aisa/pull/8)): works with an internal hop |
| 3 | `spike/usage-events` | S2 + S6: streaming token counts, `http-logger` → usage event schema | Done ([#13](https://github.com/hlan-net/aisa/pull/13)): S6 yes; S2 exact only when usage is streamed ([#12](https://github.com/hlan-net/aisa/issues/12)), confirmed against a real Ollama |
| 4 | `spike/config-rendering` | S9: consul-template → standalone `apisix.yaml` reload | Done ([#19](https://github.com/hlan-net/aisa/pull/19)): reloads are clean and take 1 to 2 s; the rendered file needs a guard ([#18](https://github.com/hlan-net/aisa/issues/18)) |
| 5 | `spike/footprint` | S7 + S10: resources on arm64, forward-auth latency | Done ([#17](https://github.com/hlan-net/aisa/pull/17)): fits a small node with one nginx worker; `forward-auth` adds 0.3 to 2 ms. Measured on a Raspberry Pi 5 |
| 6 | `claude/sonarqube-pr-scan-66r8m0` | SonarQube scan with Go coverage for PRs and `main` | Done ([#15](https://github.com/hlan-net/aisa/pull/15)): coverage needs `SONAR_TOKEN` |
| 7 | `spike/apisix-chart` | S5: APISIX in standalone mode on Kubernetes, the official chart or plain manifests | Done ([#24](https://github.com/hlan-net/aisa/pull/24)): plain manifests; the chart has no place for the sidecar that renders the config |

## v0.2.0: First version (token quotas)

| PR | Branch | Scope | Status |
|----|--------|-------|--------|
| 1 | `feature/core-skeleton` | `cmd/aisa`, config, health endpoints, `aisa_*` metrics registry, the product `Dockerfile`, CI | Done ([#20](https://github.com/hlan-net/aisa/pull/20)) |
| 2 | `feature/decide-api` | `/v1/decide` with consumer keys from Vault (SHA-256, cached) | Done ([#23](https://github.com/hlan-net/aisa/pull/23)) |
| 3 | `feature/usage-ledger` | Usage event ingestion, dedup by `request_id`, token metrics | Done ([#26](https://github.com/hlan-net/aisa/pull/26)) |
| 4 | `feature/token-quotas` | Quota profiles from Consul KV, sliding windows in Redis | Done ([#28](https://github.com/hlan-net/aisa/pull/28)) |
| 5 | `feature/apisix-adapter` | Template, Helm values, integration test against the mock backend, the checked config render ([#18](https://github.com/hlan-net/aisa/issues/18)), explicit `log_format` ([#5](https://github.com/hlan-net/aisa/issues/5)) and upstream timeouts ([#14](https://github.com/hlan-net/aisa/issues/14)), fail-open routes for backends with `fail_policy = "open"` ([#4](https://github.com/hlan-net/aisa/issues/4)), requests and limits measured on a Raspberry Pi 4 ([#16](https://github.com/hlan-net/aisa/issues/16)) | — |
| 6 | `feature/terraform-module` | Vault mount, policies and roles; Consul ACLs, KV and backend registrations | — |
| 7 | `feature/dashboard` | Grafana dashboard and alert rules | — |
| 8 | `feature/helm-chart` | `deploy/helm/aisa`: aisa with optional Redis, published with the image as an OCI chart on GHCR; `helm lint`, kubeconform and a kind/k3d install test on amd64 and arm64 ([#6](https://github.com/hlan-net/aisa/issues/6)) | — |

## v0.3.0: Money budgets

| Feature | Scope | Status |
|---------|-------|--------|
| Pricing | Prices per model in Consul KV, `aisa_cost_total` | — |
| Monthly budgets | Calendar-month budgets, soft limit, persisted totals | — |
| Downgrade | `on_exhausted: downgrade` → local model via `X-Aisa-Model` | — |

## v0.4.0: Second adapter

Prove the contract with a second gateway. **LiteLLM** is the most likely: its open source proxy supports custom auth and callbacks, which map to the decision API and usage events. The contract is marked stable only after this. Later candidates: Envoy AI Gateway / Agent Router, API7 AISIX.

## Later

- **Vault-issued JWTs** instead of static consumer keys ([docs/concepts/VAULT.md](docs/concepts/VAULT.md))
- Dynamic provider credentials where supported (e.g. Azure OpenAI through Vault)

---

## Upstream contributions

Improvements that belong in the gateways rather than in aisa:
1. APISIX Vault secret manager: KV v2 support
2. APISIX Vault secret manager: Kubernetes auth
3. `ai-proxy`: count streamed tokens when the backend sends no usage, and log partial usage (and 499) when the client disconnects (spikes S2/S6, [#12](https://github.com/hlan-net/aisa/issues/12))
4. `ai-proxy-multi`: select the instance by model or by a request header, which would remove the internal hop the adapter needs today (spike S8)
5. `forward-auth`: clear the `upstream_headers` also when `allow_degradation` lets a request through (spike S8)
6. Anything the spikes uncover, e.g. token usage in log variables for streaming responses

## Principles

- aisa has no knowledge of any specific gateway. Adapter code lives only under `adapters/<name>/`.
- aisa never proxies model traffic itself. That is the gateway's job.
- The Terraform module takes Vault and Consul addresses and mount names as variables and assumes nothing about a specific environment.
