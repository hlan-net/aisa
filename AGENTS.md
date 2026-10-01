# AGENTS.md

aisa — AI Service [Access, Admin, Authority] — offers inference to the applications of a Kubernetes cluster as a service of the cluster itself, with Vault and Consul as its sources of truth. It stands between the applications and the model providers; an existing AI gateway (APISIX first) is its proxy, which aisa installs and configures. It is not a gateway and never proxies model traffic. Go, Apache 2.0. **Early development (v0.2.0)**: aisa (`cmd/aisa`, `internal/`) has config, health endpoints, metrics and the decision API with consumers from Vault next to the dev stack in `dev/` (mock backend, stub aisa); start from `docs/concepts/ARCHITECTURE.md`, `docs/concepts/ADAPTER_CONTRACT.md`, `docs/process/SPIKES.md` and `ROADMAP.md` v0.2.0.

`CLAUDE.md` covers the same ground in more detail; keep the two reconciled when changing one.

## Build / test

```bash
go build ./...
go vet ./...
go test -race ./...
golangci-lint run
```

Dev stack: `docker compose -f dev/compose.yaml up -d --build --wait && ./dev/smoke.sh` (see `dev/README.md`).

Pre-push hook: `./scripts/install-git-hooks.sh`; bypass with `SKIP_PRE_PUSH_TESTS=1 git push`.

## Architecture rules (non-obvious)

- The core (`cmd/`, `internal/`) knows no gateway. Gateway-specific code lives only in `adapters/<name>/`.
- Gateways integrate through three contracts: `POST /v1/decide` (forward-auth), usage events (access log sink) and config rendering (consul-template). Changing a contract means a new version and an update to `ADAPTER_CONTRACT.md` in the same PR.
- Consul holds everything that is not a secret (backends, prices, budgets, quota profiles); Vault holds the secrets (provider keys; today also the consumers); Redis holds hot counters. Kubernetes objects are what aisa creates from these, never where it reads from.
- Terms (`docs/concepts/ARCHITECTURE.md`, What aisa is): aisa (control), proxy (carries the traffic), adapter (specific to a kind of provider). The paths `adapters/apisix/` and `ADAPTER_CONTRACT.md` still use the older name for the proxy and are to be renamed.
- Quota and budget rules live only in aisa, never in gateway plugins.
- `aisa_*` metrics are primary; nothing may depend only on gateway-native metrics.

## Conventions worth knowing

- linux/amd64 and linux/arm64 are both first-class; nothing may assume amd64.
- Public repo: no real hostnames, IP addresses, tenant IDs or paths to other repos. No secrets in Git; local test credentials go in `.local/` (git-ignored).
- Docs in English. `docs/concepts/` UPPER_SNAKE, `docs/features/` lower-kebab.
- Feature branch + PR into `main`; every user-visible change adds a `CHANGELOG.md` entry under `[Unreleased]`.

## Tests

- No live network in unit tests; integration tests use a mock OpenAI-compatible backend and dev-mode Vault and Consul.
- Tests needing real credentials read `.local/` and skip when it is missing.

## Maintainer and environment

- The maintainer converses in Finnish; repo content is in English. Answer questions with analysis only; merge, delete branches or publish only when asked.
- ARM-first: the reference deployment is k3s on Raspberry Pi (arm64) with Traefik, Vault HA (Kubernetes auth, Agent injector, KV v2, Consul secrets engine), Consul with ACLs, kube-prometheus-stack, and Ollama outside the cluster. Details in `CLAUDE.md` → Reference environment.
- Start implementing from `ROADMAP.md` v0.1.0 (dev stack and spikes).
