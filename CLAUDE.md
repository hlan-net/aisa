# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

aisa — **AI Service [Access, Admin, Authority]** — offers inference to the applications of a Kubernetes cluster as a service of the cluster itself, with Vault and Consul as its sources of truth. It stands between the applications (consumers) and the model providers: it decides whether a request may pass, passes it on with the holder's credentials and keeps the books (identities, token quotas, budgets in tokens, normalized usage metrics). It is **not** a gateway: it never proxies model traffic itself. An existing AI gateway is its **proxy**, which aisa installs, configures and removes. Apache 2.0, public repository `hlan-net/aisa`.

**Status: early development (v0.2.0).** The design lives in `docs/`, and its open questions are answered in `docs/process/SPIKES.md`. aisa has config, health endpoints, the metrics registry, the decision API with consumers from Vault (`internal/vault`, `internal/consumers`, `internal/decide`), the usage ledger (`internal/ledger`) and token quotas from Consul with counters in Redis (`internal/consul`, `internal/quotas`); the next steps are the PRs of `ROADMAP.md` v0.2.0.

## Where things are

| Path | Contents |
|---|---|
| `docs/concepts/` | The core design: `ARCHITECTURE.md`, `PROXY_CONTRACT.md` (the three aisa ↔ proxy contracts), `VAULT.md`, `CONSUL.md`, `KUBERNETES_OBJECTS.md` (what applications see: `Model` objects and their Secret) |
| `docs/features/` | User-facing capabilities: `quotas-and-budgets.md`, `usage-metrics.md` |
| `docs/process/` | How work is done: `SPIKES.md` (open questions and their outcomes), `USER_STORIES.md` (who aisa is built for, US-1 to US-3, with acceptance criteria and gaps) |
| `cmd/aisa/`, `internal/` | aisa itself: `config` (environment variables), `server` (HTTP, health), `metrics` (the `aisa_*` registry), `vault`, `consumers`, `decide`, `ledger`, `consul`, `quotas`, `version` |
| `Dockerfile` | The product image: a static binary in a distroless image, amd64 and arm64 |
| `deploy/helm/aisa/` | aisa's chart: aisa, an optional Redis, and the proxy's chart (`proxies/apisix/chart`) as a dependency |
| `deploy/helm/test/` | `charts_test.sh` (lint, kubeconform) and `install_test.sh` (kind: Vault, Consul with ACLs, both charts, requests through the proxy) |
| `dev/` | Docker Compose dev stack (Vault, Consul, Redis, APISIX standalone, mock backends, stub aisa) and `smoke.sh`; see `dev/README.md` |
| `ROADMAP.md` | Mission, current state and versioned milestones with PR tables |
| `CHANGELOG.md` | Keep a Changelog, semantic versioning |

Planned code layout (from `docs/concepts/ARCHITECTURE.md`): `cmd/aisa/`, `internal/…`, `proxies/<name>/`, `deploy/helm/`, `terraform/`, `dashboards/`.

## Build commands


```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
go test ./dev/stubaisa -run TestDecide   # a single test
golangci-lint run
```

### Local dev scripts

```bash
./scripts/install-git-hooks.sh   # installs pre-push hook (go vet + go test)
./scripts/check-versions.sh      # Go module and toolchain updates available
```

### Dev stack

```bash
docker compose -f dev/compose.yaml up -d --build --wait
./dev/smoke.sh                   # end-to-end check through APISIX (needs curl and jq)
docker compose -f dev/compose.yaml down -v
```

### Charts

```bash
./deploy/helm/test/charts_test.sh    # helm lint and kubeconform for both charts (needs helm, kubeconform)
./deploy/helm/test/install_test.sh   # install into the current kubectl context, e.g. kind (see the script)
```

CI runs the unit tests, the dev stack smoke test and the kind install test on both amd64 and arm64 runners.

Skip the pre-push hook with `SKIP_PRE_PUSH_TESTS=1 git push`.

## Architecture

Read `docs/concepts/ARCHITECTURE.md` and `docs/concepts/PROXY_CONTRACT.md` before any structural change. The rules that must hold:

- **Three roles.** A consumer sends inference requests, a provider answers them, and aisa is the Kubernetes-native service between them. `docs/concepts/ARCHITECTURE.md` (What aisa is) has the terms: **aisa** (control), **proxy** (carries the traffic; APISIX first), **adapter** (what is specific to a kind of provider), **holder** (operates aisa). "Adapter" never means the proxy: what makes a gateway work as aisa's proxy lives in `proxies/<name>/`.
- **Core vs proxy.** The core (`cmd/`, `internal/`) has no knowledge of any specific gateway. Everything gateway-specific lives under `proxies/<name>/`. If a change to the core mentions APISIX (or any other gateway) by name, it is in the wrong place.
- **Three contracts connect a gateway:** the decision API (`POST /v1/decide`, forward-auth pattern), usage events (normalized JSON from an access log sink) and config rendering (consul-template). A change to a contract is versioned (`/v1/` → `/v2/`) and documented in `PROXY_CONTRACT.md` in the same PR.
- **Sources of truth:** everything that is not a secret in Consul (backends, prices, budgets, quota profiles), secrets in Vault (provider keys; today also the consumers, whose non-secret part is to move to Consul); hot counters in Redis, persisted to Consul KV once a minute. Kubernetes objects are what aisa creates from these for the consumers, never where aisa reads from. The holder may work through aisa's admin interface instead of Consul and Vault directly.
- **Rules live only in aisa.** No quota or budget logic in gateway plugins. A gateway-side cache of decisions is allowed as an optimisation.
- **Metrics:** aisa's own `aisa_*` metrics are the primary ones. Dashboards and alerts must not depend only on gateway-native metrics.

## Conventions

- **Language:** Go, standard `gofmt` formatting, `golangci-lint` clean. Errors are wrapped with context (`fmt.Errorf("…: %w", err)`); no panics outside `main`.
- **Platforms:** linux/amd64 and linux/arm64 are both first-class. Development and a reference deployment run on arm64 (Raspberry Pi), so nothing may assume amd64.
- **Documentation:** English. Concept docs in `docs/concepts/` use UPPER_SNAKE names; feature docs in `docs/features/` use lower-kebab names.
- **No environment-specific values** in this public repo: no real hostnames, IP addresses, tenant IDs or paths to other repositories. Examples use neutral names (`gpu-box.internal`, `chat-ui`, `batch-jobs`).
- **Secrets:** never in Git. Local test credentials go in `.local/` (git-ignored, chmod 600).
- **Changelog:** every user-visible change adds an entry under `## [Unreleased]` in `CHANGELOG.md`.

## Workflow

- Work on a feature branch (`feature/…`, `fix/…`, `docs/…`) and open a PR into `main`; never commit directly to `main`.
- Fill in `.github/pull_request_template.md`.
- Run the tests locally before pushing; the pre-push hook does this.
- Design gaps found during review become GitHub issues with problem, current behaviour, proposed fix, test plan and acceptance criteria, not notes in chat.

## Tests

- Avoid live network in unit tests. Integration tests run against a mock OpenAI-compatible backend and dev-mode Vault and Consul.
- A test that needs real credentials reads them from `.local/` and is skipped when they are missing.

## Working with the maintainer

- The maintainer converses in **Finnish**; everything committed to the repo (code, comments, docs, commit messages, PRs) is in **English**.
- Distinguish questions from instructions: answer a question with analysis only, and change files only when asked to.
- Merge a PR, delete branches or publish anything only when explicitly asked.
- Before building something new, check whether an existing open source project already does it, and prefer extending it (the reason aisa sits beside gateways instead of being one).
- Treat numbers from reviews, AI-generated analyses and search results critically: separate what a source says from inference, and check units and orders of magnitude before relying on them.
- Hardware and targets are **ARM-first** (Raspberry Pi); nothing may assume x86.

## Reference environment

The first deployment target, described without environment-specific values. Design choices should work here without special cases:

- **Kubernetes:** k3s on Raspberry Pi nodes (arm64: Pi 4 workers with 4–8 GB, Pi 5 control plane), Traefik as the ingress controller, NFS-backed default StorageClass.
- **Vault:** HA with Consul storage, Kubernetes auth and the Vault Agent injector enabled, KV v2 at `secret/`, the Consul secrets engine mounted at `consul/`, OIDC login through an external identity provider.
- **Consul:** ACLs enabled. Today it is used mainly as a KV store and Terraform state backend, not as a service mesh; services outside the cluster are registered statically with Terraform rather than through agents.
- **Monitoring:** kube-prometheus-stack (Prometheus Operator, Grafana, Alertmanager); Grafana dashboards are managed with Terraform.
- **Model backends:** Ollama on a separate machine outside the cluster today (OpenAI-compatible endpoint); a dedicated inference box (e.g. a Mac mini) is planned. Cloud providers are optional.
- **Clients:** a chat UI (Open WebUI), a batch service that summarises news feeds, and CLI tools.

The environment's own deployment files (Helm values, tfvars, ingress hostnames) live in a separate private repository and consume aisa's releases.

## Handoff notes

The design was done on the maintainer's machine and moved here in full; nothing outside this repository is needed to start implementing. Start from `ROADMAP.md` v0.1.0: the dev stack and the spikes. Spike S7 (footprint on a small arm64 node) and tests against a real Ollama need the maintainer's hardware; everything else runs against the mock backend.
