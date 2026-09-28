# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

aisa — **AI Service [Access, Admin, Authority]** — is a Vault- and Consul-native governance layer for LLM traffic. It sits beside an existing AI gateway and adds identities, token quotas, money budgets and normalized usage metrics. It is **not** a gateway: it never proxies model traffic itself. Apache 2.0, public repository `hlan-net/aisa`.

**Status: design phase.** There is no code yet. The design lives in `docs/`, and the next steps are the spikes in `docs/process/SPIKES.md`.

## Where things are

| Path | Contents |
|---|---|
| `docs/concepts/` | The core design: `ARCHITECTURE.md`, `ADAPTER_CONTRACT.md` (the three aisa ↔ gateway contracts), `VAULT.md`, `CONSUL.md` |
| `docs/features/` | User-facing capabilities: `quotas-and-budgets.md`, `usage-metrics.md` |
| `docs/process/` | How work is done: `SPIKES.md` (open questions and their outcomes) |
| `ROADMAP.md` | Mission, current state and versioned milestones with PR tables |
| `CHANGELOG.md` | Keep a Changelog, semantic versioning |

Planned code layout (from `docs/concepts/ARCHITECTURE.md`): `cmd/aisa/`, `internal/…`, `adapters/<gateway>/`, `deploy/helm/`, `terraform/`, `dashboards/`.

## Build commands

Once `go.mod` exists:

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
go test ./internal/budget -run TestMonthlyRollover   # a single test
golangci-lint run
```

### Local dev scripts

```bash
./scripts/install-git-hooks.sh   # installs pre-push hook (go vet + go test)
./scripts/check-versions.sh      # Go module and toolchain updates available
```

Skip the pre-push hook with `SKIP_PRE_PUSH_TESTS=1 git push`.

## Architecture

Read `docs/concepts/ARCHITECTURE.md` and `docs/concepts/ADAPTER_CONTRACT.md` before any structural change. The rules that must hold:

- **Core vs adapters.** The core (`cmd/`, `internal/`) has no knowledge of any specific gateway. Everything gateway-specific lives under `adapters/<name>/`. If a change to the core mentions APISIX (or any other gateway) by name, it is in the wrong place.
- **Three contracts connect a gateway:** the decision API (`POST /v1/decide`, forward-auth pattern), usage events (normalized JSON from an access log sink) and config rendering (consul-template). A change to a contract is versioned (`/v1/` → `/v2/`) and documented in `ADAPTER_CONTRACT.md` in the same PR.
- **Sources of truth:** identities and provider keys in Vault; backends, prices, budgets and quota profiles in Consul; hot counters in Redis, persisted to Consul KV once a minute. Gateways never talk to Vault or Consul directly.
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
