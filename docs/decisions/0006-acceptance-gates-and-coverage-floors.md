# 0006 — Acceptance runs through Makefile gates with per-package coverage floors

Date: 2026-10-09
Status: accepted

## Context

Acceptance criteria written in prose ("coverage ≥ 90% for adapter", "docker
image builds non-root") drift: nobody re-runs them, and a claim in a summary is
indistinguishable from a measurement. The project also has a hard hygiene rule:
real credentials must never leave the git-ignored `data/` directory.

## Decision

Every acceptance claim becomes a command:

- `make test`, `make test-race` — the mock suite, with the race detector.
- `make cover-check` — per-package coverage floors from `docs/TDD.md` §6.2,
  failing with the package, the measured value and the floor.
- `make fmt`, `make vet` — formatting and static checks, whole repo.
- `make hygiene` — §6.7: no phone number, password, passToken or serviceToken
  anywhere outside `data/` and `references/`.
- `make test-docker` — DOCKER-001..006 against a real docker daemon, skipping
  explicitly when docker is absent.
- `MIMO2API_LIVE=1 go test ./internal/live` — the live protocol suite.

The gates apply to the **whole** repository, not only to the packages a change
touched: a package sitting below its floor is a finding wherever it lives.

## Consequences

- A claim can be re-verified by anyone in one command, and a delegated agent's
  "6/6 PASS" is checkable rather than trusted.
- Adding a package means adding its floor to the Makefile — the gate stays
  exhaustive by construction.
- Deferred: pre-existing unformatted files are fixed rather than excused, and
  build artifacts (`cover.out`, `.cover/`) stay out of git.

## Evidence

- `Makefile`, `.gitignore`, `docs/TDD.md` §6 status note, `docker/docker_test.sh`.
- Coverage after this decision: adapter 94.5%, pool 95.5%, session 92.3%,
  mimo 83.4%, api 84.7%, accounts 84.8%, config 90.7%.
