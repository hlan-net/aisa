# Changelog

All notable changes to this project are documented in this file.

The format is based on Keep a Changelog and follows semantic versioning.

## [Unreleased]

### Added
- **Dev stack** (`dev/`): Docker Compose with dev-mode Vault and Consul (seeded with example consumers, prices, budgets, quota profiles and backend registrations), Redis, APISIX 3.18 in standalone mode, two mock OpenAI-compatible backends and a stub aisa. The mock backend streams and returns deterministic token counts; the stub aisa answers `/v1/decide` from static dev keys and logs every decide call and usage event. `dev/smoke.sh` checks the whole path end to end.
- **Go module** `github.com/hlan-net/aisa` (Go 1.26). CI now runs vet and race tests on amd64 and arm64, golangci-lint, and the dev stack smoke test on both architectures.
- **Design documents**: The architecture, the three aisa ↔ gateway contracts (decision API, usage events, config rendering), Vault and Consul integration, token quotas and money budgets, usage metrics with a dashboard and alerts, and the spikes to run before implementation.
- **Repository layout**: `CLAUDE.md` and `AGENTS.md` for coding agents (including maintainer preferences and a description of the reference environment), `ROADMAP.md`, this changelog, a pull request template, a pre-push hook and CI that activates once `go.mod` exists.

### Changed
- **CI actions**: `actions/setup-go` v6 → v7 and `golangci/golangci-lint-action` v8 → v9 (golangci-lint pinned to v2.14). The other actions were checked against their published tags and are current. Dependabot also watches the dev stack's Dockerfile and Compose images.
