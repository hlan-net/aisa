# Roadmap

## Mission

Offer inference to the applications of a Kubernetes cluster as a service of the cluster itself, and keep its use in check.
aisa stands between the applications and the model providers: it gives every application an identity, a quota and a budget, passes its requests on with the holder's credentials, and keeps the books, using the Vault and Consul you already operate. It solves no inference itself and carries no traffic itself: an existing AI gateway is its proxy ([docs/concepts/ARCHITECTURE.md](docs/concepts/ARCHITECTURE.md#what-aisa-is)).

Who it is for, and what must be true for them, is in the user stories ([docs/process/USER_STORIES.md](docs/process/USER_STORIES.md)): US-1, inference that looks Kubernetes-native to the applications and is safe in cost, US-2, provider accounts that stay with their holder, US-3, the holder managing aisa through aisa's own admin interface, and US-4, a cluster that several parts of an organization share, each with a budget of its own.

---

## Current state: v0.3.4 released

- Architecture and the three aisa ↔ proxy contracts are written down ([docs/concepts/](docs/concepts/)), with the Kubernetes objects applications see ([docs/concepts/KUBERNETES_OBJECTS.md](docs/concepts/KUBERNETES_OBJECTS.md))
- Quotas, budgets (in tokens) and usage metrics are specified ([docs/features/](docs/features/))
- The spikes of v0.1.0 are answered ([docs/process/SPIKES.md](docs/process/SPIKES.md)); S2 in part ([#12](https://github.com/hlan-net/aisa/issues/12))
- Local dev stack with a mock backend and a stub aisa ([dev/](dev/)), and aisa on a laptop's cluster with the machine's Ollama ([deploy/helm/local/](deploy/helm/local/))
- aisa authenticates consumers, declared in Consul with their keys' hashes in Vault, with `/v1/decide`, ingests usage events at `/v1/usage`, enforces token quotas and monthly token budgets per consumer and per provider account from Consul with counters in Redis, downgrades to a local model when a budget is used up, resolves a model's other names, such as Ollama's tags, and exports its metrics ([docs/features/quotas-and-budgets.md](docs/features/quotas-and-budgets.md)) yet ([cmd/aisa/](cmd/aisa/))
- aisa issues each consumer's key into a Secret in its application's namespace, as `OPENAI_*` and `AISA_*`, with the default model its declaration or quota profile names, and publishes the models on offer as `Model` objects ([internal/kube/](internal/kube/)); application developers have a guide ([docs/features/client-integration.md](docs/features/client-integration.md))
- The holder can manage consumers, quota profiles, the consumers' monthly budgets, backends and models through aisa's admin interface (`/admin`, off by default; [docs/features/admin-interface.md](docs/features/admin-interface.md))
- Applications see what remains of their quota and budget, and a request id to follow a request by, in headers on their responses (`X-Aisa-Quota-Remaining`, `X-Aisa-Quota-Reset`, `X-Aisa-Budget-Remaining`, `X-Aisa-Request-Id`)
- The APISIX proxy renders its routes from Consul and Vault, including backends whose API is not at `/v1` and credentials that Vault issues, keeps rendering when its Consul token rotates, also in Kubernetes, reports each render to aisa, and lets nothing of a provider's account through to the application ([proxies/apisix/](proxies/apisix/), [#35](https://github.com/hlan-net/aisa/issues/35))
- aisa's Helm chart installs aisa, Redis and the proxy in one release, with the alerts as a `PrometheusRule` ([deploy/helm/aisa/](deploy/helm/aisa/)); CI installs it in kind on amd64 and arm64
- Each `v*` tag publishes a GitHub release, a multi-arch image on GHCR (`ghcr.io/hlan-net/aisa`) and both charts as OCI artifacts (`oci://ghcr.io/hlan-net/charts/aisa`, `oci://ghcr.io/hlan-net/charts/aisa-proxy-apisix`)
- A Terraform module ([terraform/](terraform/)) sets up Vault policies and roles and Consul ACLs, KV entries and backend registrations, and a Grafana dashboard ([dashboards/aisa.json](dashboards/aisa.json)) covers all `aisa_*` metrics

---

## Readiness: from a row to an issue

Every piece of aisa's own work below that is not done has one of four states, in the versions, in the direction and under Later, and the state says who can take the next step. The upstream contributions at the end are work in other projects and have none.

| State | Meaning | Next step |
|---|---|---|
| **Proposed** | An idea. The maintainer has not decided to do it. | The maintainer decides |
| **Needs a design** | Decided, with questions open that an implementer must not answer alone. | The questions are answered, in the issue or a concept doc |
| **Needs an issue** | Decided, and nothing known is open, but no issue describes the work as the template asks. | An issue from the template, or an older issue rewritten with it |
| **Ready** | An issue written with [the template](.github/ISSUE_TEMPLATE/implementation.md), with nothing under its open questions. It can be implemented as it stands, by a person or a coding agent. | A PR |

- **One row, one issue, one PR.** Work too large for one PR gets a table of its own, as the versions below have, with a row for each PR and what it depends on.
- **Only Ready work is handed to an implementer.** An implementer who finds a question the issue does not answer asks in the issue and does not decide it in the code.
- **Needs the maintainer** is a mark beside the state, not a state. It marks a step that takes hardware or credentials an implementer does not have: a Raspberry Pi, a real Ollama, a provider's key. The rest of the work can still be Ready; that step is not replaced by a mock.
- **Done work leaves the tables of open work.** It is in the version it shipped with and in [CHANGELOG.md](CHANGELOG.md).

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
| 5 | `feature/apisix-adapter` | Template, Helm values, integration test against the mock backend, the checked config render ([#18](https://github.com/hlan-net/aisa/issues/18)), explicit `log_format` ([#5](https://github.com/hlan-net/aisa/issues/5)) and upstream timeouts ([#14](https://github.com/hlan-net/aisa/issues/14)), fail-open routes for backends with `fail_policy = "open"` ([#4](https://github.com/hlan-net/aisa/issues/4)), requests and limits measured on a Raspberry Pi 4 ([#16](https://github.com/hlan-net/aisa/issues/16)) | Done in part ([#39](https://github.com/hlan-net/aisa/pull/39)); the Helm values came with PR 8; the empty catalog, an invalid `timeout` and the render report of #18 followed ([#54](https://github.com/hlan-net/aisa/pull/54), [#56](https://github.com/hlan-net/aisa/pull/56)). What is left is [#16](https://github.com/hlan-net/aisa/issues/16), **Needs an issue**: it predates the template and names the old `adapters/` paths. The measuring harness can be written by anyone; the run on a Raspberry Pi 4 needs the maintainer |
| 6 | `feature/terraform-module` | Vault mount, policies and roles; Consul ACLs, KV and backend registrations | Done ([`terraform/`](terraform/)) |
| 7 | `feature/dashboard` | Grafana dashboard and alert rules | Done: the alert rules ship with the chart as a `PrometheusRule` ([#72](https://github.com/hlan-net/aisa/pull/72)), and the Grafana dashboard is in [`dashboards/aisa.json`](dashboards/aisa.json) |
| 8 | `feature/helm-chart` | `deploy/helm/aisa`: aisa with optional Redis, published with the image as an OCI chart on GHCR; `helm lint`, kubeconform and a kind/k3d install test on amd64 and arm64 ([#6](https://github.com/hlan-net/aisa/issues/6)). The chart also installs and removes the proxy, through the proxy's own chart (`proxies/apisix/chart`) | Done ([#45](https://github.com/hlan-net/aisa/pull/45)); the metrics on a port of their own ([#46](https://github.com/hlan-net/aisa/issues/46)) followed ([#51](https://github.com/hlan-net/aisa/pull/51)) |
| 9 | `feature/account-isolation` | Proxy rules for what passes between an application and a provider: an allowlist of forwarded request headers, no provider response headers or error bodies to the client ([#35](https://github.com/hlan-net/aisa/issues/35), US-2); in `PROXY_CONTRACT.md` and the APISIX template | Done ([#67](https://github.com/hlan-net/aisa/pull/67)) |
| 10 | `feature/application-guide` | Instructions for the developer of a consuming application, and `/v1/models` through the gateway ([#34](https://github.com/hlan-net/aisa/issues/34), US-1) | Done ([#48](https://github.com/hlan-net/aisa/pull/48), [#75](https://github.com/hlan-net/aisa/pull/75)) |
| 11 | `feature/consumers-in-consul` | The non-secret part of a consumer (name, quota profile, the namespace and Secret of its application) moves from Vault to Consul KV `aisa/consumers/<name>`; Vault keeps the key hashes | Done ([#58](https://github.com/hlan-net/aisa/pull/58)) |
| 12 | `feature/kubernetes-objects` | aisa talks to the Kubernetes API: the `Model` CRD and its objects from Consul, and each consumer's key generated by aisa and written to a Secret in its application's namespace ([docs/concepts/KUBERNETES_OBJECTS.md](docs/concepts/KUBERNETES_OBJECTS.md), [#32](https://github.com/hlan-net/aisa/issues/32), US-1) | Done ([#66](https://github.com/hlan-net/aisa/pull/66)) |

**Order.** The PRs are numbered by when they were planned, not by when they land. A working in-cluster install comes first: 5 (the proxy), 8 (the chart), then 11 and 12, so that an application gets inference with a Secret and finds the models as objects. 9 followed; 6, 7 and 10 land in v0.2.x, after the first release; budgets come in v0.3.0.

## v0.3.0: Budgets

Budgets are enforced in tokens, like quotas. Prices turn tokens into currency for reports and dashboards only; no limit is set or checked in currency (decided 2026-09-29). The design is in [docs/features/quotas-and-budgets.md](docs/features/quotas-and-budgets.md#budgets-v030).

| Feature | Scope | Status |
|---------|-------|--------|
| Pricing | Prices per model in Consul KV, `aisa_cost_total`, for display | Done |
| Monthly budgets | Calendar-month token budgets, soft limit, persisted totals | Done |
| Downgrade | `on_exhausted: downgrade` → local model via `X-Aisa-Model` | Done |
| Safe defaults and a ceiling | A consumer without limits cannot use a backend with a key; a budget per provider account above the consumers ([#33](https://github.com/hlan-net/aisa/issues/33), US-1) | Done: a consumer without a quota profile is denied, and `aisa/budgets/providers/<key>` caps every key |

## v0.3.x: A paid provider behind aisa

The budgets of v0.3.0 matter only for a paid backend. aisa must be able to front a SaaS provider with a hard ceiling (`aisa/budgets/providers/<key>`, `on_exhausted: downgrade` to a local model), and first stop failing in the cluster it runs in. What that takes is built (the first table); what is left is to see it work against the providers themselves (the second). Done when the Gemini API, the OpenAI API and the Claude API have each been registered with a real key and served a request through the proxy, with its tokens in the ledger and its provider budget bounding what it costs.

| Item | Scope | Status |
|------|-------|--------|
| Consul token rotation | consul-template is reloaded with a rotated Consul token and never exits on one ([#90](https://github.com/hlan-net/aisa/issues/90)) | Done ([#93](https://github.com/hlan-net/aisa/pull/93)) |
| Backend path prefix | A backend whose OpenAI-compatible API is not at `/v1`, such as Google's ([#91](https://github.com/hlan-net/aisa/issues/91)) | Done ([#94](https://github.com/hlan-net/aisa/pull/94)) |
| Minted provider credentials | A provider key that Vault issues, such as Vertex AI's OAuth2 token from the GCP secrets engine ([#92](https://github.com/hlan-net/aisa/issues/92)) | Done ([#95](https://github.com/hlan-net/aisa/pull/95)); a consul-template change that reads a GCP token's expiry is prepared for upstream |
| Model aliases | Other names of a model, such as Ollama tags, resolved in the decision; an unknown name is 404 `model_not_found` ([#89](https://github.com/hlan-net/aisa/issues/89)) | Done ([#100](https://github.com/hlan-net/aisa/pull/100)) |

**Validation against the providers.** All three are reached as `openai-compatible` backends, which is what aisa and its proxy have today, so none of them is expected to need new code in aisa, and finding out is the point: a key in Vault, a backend in Consul's catalog, a price and a provider budget ([`docs/concepts/CONSUL.md`](docs/concepts/CONSUL.md#a-paid-provider-the-gemini-api)). A validation is a test that reads the provider's key from `.local/` and is skipped without it; it sends a few requests and costs accordingly. For each provider it shows: a chat completion through the proxy, streamed and not; the request's tokens in the usage ledger, equal to what the provider reports; the provider budget denying or downgrading to a local model when it is used up; and nothing of the provider's account reaching the application (US-2).

All three rows are **Needs an issue**: the test needs no key to be written, and no issue describes it yet. Each run needs the maintainer: a real key in `.local/`, and it costs money.

| Provider | How it is reached | Status |
|------|-------|--------|
| Gemini API | Google's OpenAI-compatible endpoint, under `/v1beta/openai` (the backend's `path`) | Needs an issue; the run needs the maintainer. Documented with an example; not yet run against the provider |
| OpenAI API | `api.openai.com`, under `/v1`, the default path | Needs an issue; the run needs the maintainer |
| Claude API | Anthropic's OpenAI SDK compatibility layer, `api.anthropic.com` under `/v1` | Needs an issue; the run needs the maintainer. With limits that are the provider's: Anthropic describes the layer as meant for testing and comparing models, not as a long-term or production path. It has no prompt caching, ignores `strict` on tool calls, moves system messages to the start of the conversation and returns none of the model's thinking. Part of this validation is to write these limits, as the run confirms them, into [`docs/features/client-integration.md`](docs/features/client-integration.md), which does not have them yet; the full Claude API is the Anthropic-native passthrough below |

## Direction: not scheduled

What aisa is to become beyond the versions above; none of it has a version yet. The direction of 2026-10-01 is described in [docs/concepts/ARCHITECTURE.md](docs/concepts/ARCHITECTURE.md#state-of-this-direction); later rows came from the user stories and from use. The states are those of [Readiness](#readiness-from-a-row-to-an-issue). Nothing here is Ready yet: the rows that say Needs an issue have their decisions and wait for an issue written with the template.

| Item | What is left | Story | State |
|------|--------------|-------|-------|
| A backend can tell aisa from anyone else | Decided 2026-11-23: who may use a backend is the backend's to enforce, or its holder's, and aisa does not close the way around itself. It gives the backend the means to accept only aisa: a key that only the proxy presents, which is built, and a source in the network that can be recognised, such as the nodes the proxy runs on or an egress address. What is left is the second, and the instructions for a backend without a key, such as a local Ollama ([docs/concepts/ARCHITECTURE.md](docs/concepts/ARCHITECTURE.md#exposure)) | US-1 | Needs an issue |
| Admin: prices, provider budgets, backends | The holder sets these in `/admin` instead of Consul. Consumers, quota profiles and the consumers' budgets are there already. The writes are made with aisa's own Consul identity, as today (decided 2026-11-23) | US-3 | Needs an issue |
| Admin: provider credentials | Written with the user's own Vault token (decided 2026-11-20). Decided 2026-11-23: the token may be kept in the user's browser session and never by aisa; only the actions that Vault does not allow aisa's own identity need it; an elevated request that did not arrive over TLS is refused where that is easy to tell. [#133](https://github.com/hlan-net/aisa/issues/133) is to be rewritten with the template | US-3 | Needs an issue |
| Admin: login | Decided 2026-11-23: for now the administrator supplies a Vault token they fetched themselves from the Vault they trust, and Vault's policies decide what it may change. When the interface is exposed through an ingress, authentication must be in place for its requests to be accepted. The maintainer notes that pasting a token asks for much trust in aisa, so this may change. A review of who needs which right is a quality step of its own, later | US-3 | Needs an issue |
| Admin: a credential's metadata and expiry | What identifies a credential at its provider and when it ends, in the custom metadata of the KV v2 secret (accepted 2026-11-23); a warning before it expires; a credential that Vault maintains shown as such. What has expired should be marked as expired, in Consul or in Vault | US-3 | Needs an issue |
| Group budgets | A consumer names a group (a business area, an environment), and a group has a monthly budget between the consumer's and the provider account's. Decided 2026-11-23 ([#140](https://github.com/hlan-net/aisa/issues/140)): one group per consumer; the budget counts priced models, as a consumer's does, with no group quota beside it; `X-Aisa-Budget-Remaining` shows the smallest of the consumer's and the group's; the group has metrics of its own. It becomes a version with a row for each PR | US-4 | Needs an issue |
| A group's own administrator | Someone who answers for a group sees and changes that group's consumers and budgets only. Decided 2026-11-23: there is no administrator who can do everything; whoever appoints a group's administrators must hold that group's administrator role. After the login above | US-4 | Needs a design |
| Provider adapters | What is specific to a kind of provider, beyond `openai-compatible`. A provider key as a reference to any Vault path came with [#92](https://github.com/hlan-net/aisa/issues/92) | US-2, US-3 | Needs a design |
| Anthropic-native passthrough | `/v1/messages`, preserving prompt caching (`cache_control`) for long system prompts. Not part of v0.3.x, where the Claude API is reached through its OpenAI compatibility layer: the native API needs an adapter of its own. It authenticates with `x-api-key` and `anthropic-version` instead of a bearer token; it reports usage in other fields, with cache reads and writes that are priced apart; the proxy's gateway has no driver for it (APISIX 3.20 ships none, and the template renders only `openai-compatible` backends); and applications would speak the Anthropic API to aisa, a second client-facing contract beside the OpenAI-compatible one. Decided 2026-11-23: it is to be offered, on the condition that it does not get in the way of the other use | US-1, US-2 | Needs a design |
| Rendering without consul-template | A renderer in Go next to the proxy, which also logs in to Vault itself, in place of the template and the Vault Agent. It must follow leases as consul-template does. Decided 2026-11-23: it is the steadier solution, and it gets a higher priority if it can be shown to save a significant amount of memory. The first step is that measurement: what consul-template and the Vault Agent use next to the proxy today | — | Needs a design |
| Abandoned requests | A request whose client has gone is generated to the end and charged. Decided 2026-11-23: it stays charged, since everything that is metered as use outside Kubernetes uses up the budget. Find which hop ignores the abort, stop the backend where the gateway allows, and show it in the usage event and the metrics. [#139](https://github.com/hlan-net/aisa/issues/139) is to be rewritten with the template. The last check, against a real Ollama, needs the maintainer | US-1 | Needs an issue |
| Consumer self-declaration | A namespace-scoped `Consumer` CRD or a Deployment annotation (`aisa.hlan.net/consumer`), for registration from an application's chart. Open: which of the two, and how it stands with the principle that Kubernetes objects are never where aisa reads from | US-1 | Needs a design |
| Enriched Level 1 discovery | `capabilities` (e.g. tools, vision) and `context_length` in `GET /v1/models`, so that pods with `automountServiceAccountToken: false` need no access to the `Model` objects | US-1 | Needs an issue |
| Consumer quota inspection | An endpoint where a consumer reads its own quota. The response headers `X-Aisa-Quota-Remaining` and `X-Aisa-Quota-Reset` came in v0.3.2 | US-1 | Needs an issue |
| Local or paid in `Model` | A `Model` object says whether the model is local or paid, so that an application can prefer a free one (decided 2026-11-23; [docs/concepts/KUBERNETES_OBJECTS.md](docs/concepts/KUBERNETES_OBJECTS.md#open-questions), question 3) | US-1 | Needs an issue |
| Key deprecation warning | `X-Aisa-Key-Deprecated: <expiry>` during the rotation grace window, and `AISA_KEY_ROTATED_AT` in the Secret | US-1 | Needs an issue |

Shipped from this direction, and so no longer in the table: the Kubernetes objects and consumers in Consul (v0.2.0 PRs 11 and 12); the admin interface with consumers and quota profiles (v0.2.1) and the consumers' budgets (v0.3.2, [#86](https://github.com/hlan-net/aisa/issues/86)); `AISA_BASE_URL` and `AISA_API_KEY` in the Secret (v0.2.1); the default model in the Secret as `OPENAI_MODEL`, `LLM_MODEL` and `AISA_MODEL` (v0.3.2, [#119](https://github.com/hlan-net/aisa/pull/119)); the quota headers and `X-Aisa-Request-Id` on responses (v0.3.2, [#122](https://github.com/hlan-net/aisa/pull/122)).

## v0.4.0: Second proxy

**State: Needs an issue.** Decided 2026-11-23: it is to be built. Without a real second proxy the chance to support more than one closes, as everything else is built and tested against the first. It needs a table with a row for each PR. The case itself, a gateway that already exists and belongs to someone else, is still one no user story asks for.

Prove the contract with a second gateway. **LiteLLM** is the most likely: its open source proxy supports custom auth and callbacks, which map to the decision API and usage events. The contract is marked stable only after this, so the rules that the user stories add to it ([#35](https://github.com/hlan-net/aisa/issues/35), and the client-facing paths of [#34](https://github.com/hlan-net/aisa/issues/34)) must be in the contract before the second proxy is written; otherwise it proves an incomplete contract. Later candidates: Envoy AI Gateway / Agent Router, API7 AISIX.

## Later

Both are wanted (2026-11-23) and are **Needs a design**.

- **Vault-issued JWTs** for people instead of static consumer keys ([docs/concepts/VAULT.md](docs/concepts/VAULT.md)). Applications get theirs as a Secret from aisa (v0.2.0 PR 12) and never log in to Vault
- Dynamic provider credentials where supported (e.g. Azure OpenAI through Vault). The maintainer expects them to suit batch runs and doubts them for continuous use, so the design starts from batch use

---

## Upstream contributions

Improvements that belong in the gateways rather than in aisa:
1. APISIX Vault secret manager: KV v2 support
2. APISIX Vault secret manager: Kubernetes auth
3. `ai-proxy`: count streamed tokens when the backend sends no usage, and log partial usage (and 499) when the client disconnects (spikes S2/S6, [#12](https://github.com/hlan-net/aisa/issues/12))
4. `ai-proxy-multi`: select the instance by model or by a request header, which would remove the internal hop the proxy needs today (spike S8)
5. `forward-auth`: clear the `upstream_headers` also when `allow_degradation` lets a request through (spike S8)
6. Anything the spikes uncover, e.g. token usage in log variables for streaming responses
7. `ai-proxy-multi`: separate connect and read timeouts, and a maximum above 600 s or a configurable one. With one timeout, a request to a backend that has just gone away waits as long as a slow model may take; and a non-streamed answer of more than 10 minutes cannot succeed ([#14](https://github.com/hlan-net/aisa/issues/14), [#52](https://github.com/hlan-net/aisa/issues/52)). Spike S11 measured what the plugin's own options do about it: a retry on another instance helps only fast failures, and active checks only with two or more instances; the connect timeout has to come from the plugin. A patch is ready to submit: `timeout` may be `{connect, send, read}`, with the read timeout above 600 s; with a 1 s connect timeout every request to a model with a gone instance succeeded within 1.1 s, against half of them failing after the full timeout without it (SPIKES.md, S11)

## Principles

- aisa has no knowledge of any specific gateway. Code for a specific proxy lives only under `proxies/<name>/`.
- aisa never proxies model traffic itself. That is the proxy's job, and the proxy is an existing AI gateway: aisa installs it, configures it and removes it, and at run time the proxy depends on aisa as little as it can.
- Consul holds everything that is not a secret, Vault holds the secrets, and Kubernetes objects are what aisa makes of them, never where aisa reads from.
- The Terraform module takes Vault and Consul addresses and mount names as variables and assumes nothing about a specific environment.
- An application sees an OpenAI-compatible Service and nothing of Vault, Consul or the provider accounts behind it.
