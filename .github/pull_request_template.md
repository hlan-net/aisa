## Summary

<!-- What does this PR do? Why? -->

## Changes

- 

## Contracts

<!-- Does this PR change the decision API, the usage event schema, config rendering or the aisa_* metrics? -->
- [ ] No contract change
- [ ] Contract extended without breaking anything (stays in `/v1/`), `docs/concepts/PROXY_CONTRACT.md` updated
- [ ] Breaking contract change: new version (`/v2/`) and `docs/concepts/PROXY_CONTRACT.md` updated

## Test plan

- [ ] `go test -race ./...` passes locally
- [ ] Integration tested against the dev stack (mock backend, dev-mode Vault and Consul), if relevant

## Checklist

- [ ] No secrets and no environment-specific values (hostnames, IP addresses, tenant IDs)
- [ ] Gateway-specific code only under `proxies/<name>/`
- [ ] `CHANGELOG.md` entry under `[Unreleased]` for user-visible changes
- [ ] Docs updated
