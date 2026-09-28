# Roadmap

## Mission

Keep LLM usage in check without replacing the gateway you already run.
aisa gives every application an identity, a quota and a budget, and keeps the books, using the Vault and Consul you already operate.

---

## Current state: design phase

- Architecture and the three aisa ↔ gateway contracts are written down ([docs/concepts/](docs/concepts/))
- Quotas, money budgets and usage metrics are specified ([docs/features/](docs/features/))
- Open questions are listed as spikes ([docs/process/SPIKES.md](docs/process/SPIKES.md))
- Local dev stack with a mock backend and a stub aisa ([dev/](dev/))

---

## v0.1.0: Spikes and dev environment

Answer the open questions before writing the core. Everything runs locally: dev-mode Vault and Consul, APISIX in standalone mode, Redis and a mock OpenAI-compatible backend.

| PR | Branch | Scope | Status |
|----|--------|-------|--------|
| 1 | `feature/dev-environment` | Docker Compose dev stack, mock backend, stub aisa that logs what it receives | In review |
| 2 | `spike/forward-auth-model` | S8: model name to `/v1/decide`, routing by `X-Aisa-Model` | In review: works with an internal hop |
| 3 | `spike/usage-events` | S2 + S6: streaming token counts, `http-logger` → usage event schema | — |
| 4 | `spike/config-rendering` | S9: consul-template → standalone `apisix.yaml` reload | — |
| 5 | `spike/footprint` | S7 + S10: resources on arm64, forward-auth latency | — |

## v0.2.0: First version (token quotas)

| PR | Branch | Scope | Status |
|----|--------|-------|--------|
| 1 | `feature/core-skeleton` | `cmd/aisa`, config, health endpoints, `aisa_*` metrics registry, CI | — |
| 2 | `feature/decide-api` | `/v1/decide` with consumer keys from Vault (SHA-256, cached) and fail policies | — |
| 3 | `feature/usage-ledger` | Usage event ingestion, dedup by `request_id`, token metrics | — |
| 4 | `feature/token-quotas` | Quota profiles from Consul KV, sliding windows in Redis | — |
| 5 | `feature/apisix-adapter` | Template, Helm values, integration test against the mock backend | — |
| 6 | `feature/terraform-module` | Vault mount, policies and roles; Consul ACLs, KV and backend registrations | — |
| 7 | `feature/dashboard` | Grafana dashboard and alert rules | — |

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
3. `ai-proxy-multi`: select the instance by model or by a request header, which would remove the internal hop the adapter needs today (spike S8)
4. `forward-auth`: clear the `upstream_headers` also when `allow_degradation` lets a request through (spike S8)
5. Anything the spikes uncover, e.g. token usage in log variables for streaming responses

## Principles

- aisa has no knowledge of any specific gateway. Adapter code lives only under `adapters/<name>/`.
- aisa never proxies model traffic itself. That is the gateway's job.
- The Terraform module takes Vault and Consul addresses and mount names as variables and assumes nothing about a specific environment.
