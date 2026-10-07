# Roadmap

## Mission

Offer inference to the applications of a Kubernetes cluster as a service of the cluster itself, and keep its use in check.
aisa stands between the applications and the model providers: it gives every application an identity, a quota and a budget, passes its requests on with the holder's credentials, and keeps the books, using the Vault and Consul you already operate. It solves no inference itself and carries no traffic itself: an existing AI gateway is its proxy ([docs/concepts/ARCHITECTURE.md](docs/concepts/ARCHITECTURE.md#what-aisa-is)).

Who it is for, and what must be true for them, is in the user stories ([docs/process/USER_STORIES.md](docs/process/USER_STORIES.md)): US-1, inference that looks Kubernetes-native to the applications and is safe in cost, US-2, provider accounts that stay with their holder, and US-3, the holder managing aisa through aisa's own admin interface.

---

## Current state: v0.1.0 released

- Architecture and the three aisa ↔ gateway contracts are written down ([docs/concepts/](docs/concepts/))
- Quotas, budgets (in tokens) and usage metrics are specified ([docs/features/](docs/features/))
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
| 8 | `feature/helm-chart` | `deploy/helm/aisa`: aisa with optional Redis, published with the image as an OCI chart on GHCR; `helm lint`, kubeconform and a kind/k3d install test on amd64 and arm64 ([#6](https://github.com/hlan-net/aisa/issues/6)). The chart is also to install and remove the proxy, which is installed separately today | — |
| 9 | `feature/account-isolation` | Proxy rules for what passes between an application and a provider: an allowlist of forwarded request headers, no provider response headers or error bodies to the client ([#35](https://github.com/hlan-net/aisa/issues/35), US-2); in `PROXY_CONTRACT.md` and the APISIX template | — |
| 10 | `feature/application-guide` | Instructions for the developer of a consuming application, and `/v1/models` through the gateway ([#34](https://github.com/hlan-net/aisa/issues/34), US-1) | — |
| 11 | `feature/consumers-in-consul` | The non-secret part of a consumer (name, quota profile, the namespace and Secret of its application) moves from Vault to Consul KV `aisa/consumers/<name>`; Vault keeps the key hashes | — |
| 12 | `feature/kubernetes-objects` | aisa talks to the Kubernetes API: the `Model` CRD and its objects from Consul, and each consumer's key generated by aisa and written to a Secret in its application's namespace ([docs/concepts/KUBERNETES_OBJECTS.md](docs/concepts/KUBERNETES_OBJECTS.md), [#32](https://github.com/hlan-net/aisa/issues/32), US-1) | — |

**Order.** The PRs are numbered by when they were planned, not by when they land. A working in-cluster install comes first: 5 (the proxy), 8 (the chart), then 11 and 12, so that an application gets inference with a Secret and finds the models as objects. 6, 7, 9 and 10 follow; budgets come in v0.3.0.

## v0.3.0: Budgets

Budgets are enforced in tokens, like quotas. Prices turn tokens into currency for reports and dashboards only; no limit is set or checked in currency (decided 2026-09-29). The design is in [docs/features/quotas-and-budgets.md](docs/features/quotas-and-budgets.md#budgets-v030).

| Feature | Scope | Status |
|---------|-------|--------|
| Pricing | Prices per model in Consul KV, `aisa_cost_total`, for display | — |
| Monthly budgets | Calendar-month token budgets, soft limit, persisted totals | — |
| Downgrade | `on_exhausted: downgrade` → local model via `X-Aisa-Model` | — |
| Safe defaults and a ceiling | A consumer without limits cannot use a backend with a key; a budget per provider account above the consumers ([#33](https://github.com/hlan-net/aisa/issues/33), US-1) | — |

## Direction: decided, not scheduled

Decided on 2026-10-01 and described in [docs/concepts/ARCHITECTURE.md](docs/concepts/ARCHITECTURE.md#state-of-this-direction); none of it has a version yet. The Kubernetes objects and consumers in Consul are now v0.2.0 PRs 11 and 12.

| Item | Scope | Story |
|------|-------|-------|
| No way around the proxy | Applications cannot reach a backend directly | US-1 |
| Admin interface | An admin API and a UI on it; login through Vault; credentials with traceable metadata and expiry warnings | US-3 |
| Provider adapters | What is specific to a kind of provider, beyond `openai-compatible`; a provider key as a reference to any Vault path, so that secrets Vault maintains can be used | US-2, US-3 |
| Rendering without consul-template | A renderer in Go next to the proxy, which also logs in to Vault itself, in place of the template and the Vault Agent. It must follow leases as consul-template does | — |

## v0.4.0: Second proxy

Under review: with aisa bringing its own proxy, a second one proves the contract for a gateway that already exists and belongs to someone else, a case no user story asks for yet.

Prove the contract with a second gateway. **LiteLLM** is the most likely: its open source proxy supports custom auth and callbacks, which map to the decision API and usage events. The contract is marked stable only after this, so the rules that the user stories add to it ([#35](https://github.com/hlan-net/aisa/issues/35), and the client-facing paths of [#34](https://github.com/hlan-net/aisa/issues/34)) must be in the contract before the second proxy is written; otherwise it proves an incomplete contract. Later candidates: Envoy AI Gateway / Agent Router, API7 AISIX.

## Later

- **Vault-issued JWTs** for people instead of static consumer keys ([docs/concepts/VAULT.md](docs/concepts/VAULT.md)). Applications get theirs as a Secret from aisa (v0.2.0 PR 12) and never log in to Vault
- Dynamic provider credentials where supported (e.g. Azure OpenAI through Vault)

---

## Upstream contributions

Improvements that belong in the gateways rather than in aisa:
1. APISIX Vault secret manager: KV v2 support
2. APISIX Vault secret manager: Kubernetes auth
3. `ai-proxy`: count streamed tokens when the backend sends no usage, and log partial usage (and 499) when the client disconnects (spikes S2/S6, [#12](https://github.com/hlan-net/aisa/issues/12))
4. `ai-proxy-multi`: select the instance by model or by a request header, which would remove the internal hop the proxy needs today (spike S8)
5. `forward-auth`: clear the `upstream_headers` also when `allow_degradation` lets a request through (spike S8)
6. Anything the spikes uncover, e.g. token usage in log variables for streaming responses

## Principles

- aisa has no knowledge of any specific gateway. Code for a specific proxy lives only under `proxies/<name>/`.
- aisa never proxies model traffic itself. That is the proxy's job, and the proxy is an existing AI gateway: aisa installs it, configures it and removes it, and at run time the proxy depends on aisa as little as it can.
- Consul holds everything that is not a secret, Vault holds the secrets, and Kubernetes objects are what aisa makes of them, never where aisa reads from.
- The Terraform module takes Vault and Consul addresses and mount names as variables and assumes nothing about a specific environment.
- An application sees an OpenAI-compatible Service and nothing of Vault, Consul or the provider accounts behind it.
