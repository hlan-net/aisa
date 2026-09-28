# Changelog

All notable changes to this project are documented in this file.

The format is based on Keep a Changelog and follows semantic versioning.

## [Unreleased]

### Added
- **SonarQube scan** (`.github/workflows/sonar.yml`, `sonar-project.properties`): pull requests and `main` get static analysis and Go test coverage in SonarQube Cloud (or a self-hosted SonarQube Server via `SONAR_HOST_URL`). The scan is skipped when `SONAR_TOKEN` is not available, as on fork and Dependabot PRs.
- **Spike S8** (`adapters/apisix/spikes/s8-forward-auth-model/`): APISIX can pass the model to `/v1/decide` and route by `X-Aisa-Model`, including downgrade, with a pre-step, `forward-auth` and an internal route per model. Outcome in `docs/process/SPIKES.md`.
- **Dev stack** (`dev/`): Docker Compose with dev-mode Vault and Consul (seeded with example consumers, prices, budgets, quota profiles and backend registrations), Redis, APISIX 3.18 in standalone mode, two mock OpenAI-compatible backends and a stub aisa. The mock backend streams and returns deterministic token counts; the stub aisa answers `/v1/decide` from static dev keys and logs every decide call and usage event. `dev/smoke.sh` checks the whole path end to end, including that no consumer key reaches the usage sink (the baseline route logs explicit fields only, and the stub redacts credentials).
- **Go module** `github.com/hlan-net/aisa` (Go 1.26). CI now runs vet and race tests on amd64 and arm64, golangci-lint, and the dev stack smoke test on both architectures.
- **Design documents**: The architecture, the three aisa ↔ gateway contracts (decision API, usage events, config rendering), Vault and Consul integration, token quotas and money budgets, usage metrics with a dashboard and alerts, and the spikes to run before implementation.
- **Repository layout**: `CLAUDE.md` and `AGENTS.md` for coding agents (including maintainer preferences and a description of the reference environment), `ROADMAP.md`, this changelog, a pull request template, a pre-push hook and CI that activates once `go.mod` exists.

### Changed
- **Decision API** (unstable `/v1`, not yet implemented): the requested model is passed in `X-Aisa-Requested-Model` or the JSON body, a missing model is a 400, and adapters must strip client-supplied `X-Aisa-*` headers, route by `X-Aisa-Model` and answer 503 when aisa is unreachable. The dev stack's APISIX config files can be swapped with `APISIX_CONFIG` and `APISIX_ROUTES`.
- **CI actions**: `actions/setup-go` v6 → v7 and `golangci/golangci-lint-action` v8 → v9 (golangci-lint pinned to v2.14). The other actions were checked against their published tags and are current. Dependabot also watches the dev stack's Dockerfile and Compose images.
