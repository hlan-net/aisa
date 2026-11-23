---
name: Implementation
about: Work that can be implemented as it is written, by a person or a coding agent
labels: enhancement
---

<!--
An issue is Ready (ROADMAP.md, Readiness) when someone who knows the repository but was not in
the discussion can implement it without asking. Fill in every section; write "None" where a
section does not apply. If "Open questions" is not empty, the issue is not Ready: it needs a
design first, and the answers are the maintainer's to give.
-->

## Problem

<!-- Who is affected and what they cannot do. Name the user story (US-n) if there is one. -->

## Current behaviour

<!-- What aisa does today, with references: files, docs, metrics, a failing command. -->

## Decisions

<!--
What is already decided, as facts and not as options: names of keys, fields, paths, metrics and
error codes, defaults, what happens on failure. Say who decided and when if it was a choice
between alternatives. The implementer follows these and does not reopen them.
-->

## Proposed fix

<!-- What to build, in the order to build it. -->

## Scope

- **Touches:** <!-- packages, charts, docs: e.g. internal/budgets, internal/decide, terraform/ -->
- **Out of scope:** <!-- what is left to another issue, with its number if it has one -->
- **Depends on:** <!-- issues or PRs that must be merged first, or None -->

## Contracts

<!-- The decision API, the usage event schema, config rendering, the aisa_* metrics. -->
- [ ] No contract change
- [ ] Contract extended without breaking anything (stays in `/v1/`); `docs/concepts/PROXY_CONTRACT.md` is updated in the same PR
- [ ] Breaking contract change: a new version (`/v2/`), which needs the maintainer's decision first

## Open questions

<!-- Anything the implementer would have to decide. Empty, or the issue is not Ready. -->

## Needs the maintainer

<!--
Steps that need hardware or credentials an implementer does not have: a Raspberry Pi, a real
Ollama, a provider's key in .local/. Say what can be done without them, and never replace such
a step with a mock without saying so. None, if the mock backend and the dev stack are enough.
-->

## Test plan

<!--
The tests to add, by package or script, and the commands that must pass:
go test -race ./..., golangci-lint run, ./dev/smoke.sh, ./deploy/helm/test/charts_test.sh,
./deploy/helm/test/install_test.sh, as far as the change reaches.
-->

## Acceptance criteria

<!-- Each one checkable: a named test that passes, a command and what it prints, a doc section that exists. -->
- [ ] 
- [ ] `CHANGELOG.md` has an entry under `[Unreleased]`
- [ ] Docs updated: <!-- which files -->
- [ ] The row in `ROADMAP.md` is updated
