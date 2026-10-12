# TASK R1-W1 — Refactor wave 1: infra (shared cache, host config, hardened gates)

> Executor: Claude Code CLI. Working tree: worktree `/root/oxcost-wt-r1` on
> branch `r1-wave1-infra` (create from main). Model: dashscope/glm-5.3.
> **Language**: English output. Spec: docs/REFACTOR_SPEC.md Part A.

## Mission

Three deliverables: A1 shared compile cache, A2 host config file, A4
hardened pre-merge gates. All offline-testable; no live upstream calls.

## A1 — Shared compile cache

1. Install sccache (`cargo install sccache` or distro package; on this
   ARM box build from source if needed — verify it works with
   `sccache --version` and a test compile).
2. Wire it: `RUSTC_WRAPPER=sccache` in cargo config
   (`/root/.cargo/config.toml`). Keep `target-dir` per-project (sccache
   handles cross-project dedup of dependency artifacts).
3. Measure: record compile time of a clean `cargo build --workspace`
   with and without sccache (second run hits cache) in
   `docs/R1W1_NOTES.md`.
4. Cleanup: remove stale target dirs ONLY under /tmp verification clones
   (do NOT touch /root/oxcost/target while other work may use it).

## A2 — Host config file (--config stub → real)

Implement per spec A2, in crates/oxcost + oxcost-core:

1. TOML schema:
   ```toml
   [server]
   bind = "127.0.0.1:8123"          # default unchanged from current
   api_keys = ["env:OXCOST_API_KEYS"]  # env-reference OR literal list

   [plugins."dev.oxcost.tokenharbor"]
   enabled = false                   # overrides manifest default

   [plugins."dev.oxcost.mimo-rust"]
   enabled = true
   ```
2. Semantics: config `enabled` OVERRIDES manifest default (either
   direction). Missing plugin section → manifest default applies.
   Runtime read at startup (no hot reload — YAGNI, documented).
3. Switch-off semantics remain: off = excluded from routing, catalog,
   probing (lifecycle.rs existing behavior — add tests proving config
   override flows through all three).
4. `--config <path>` CLI arg: parse, validate, error with line numbers
   on bad TOML. No config → current defaults (back-compat).
5. Tests: override precedence matrix (config-off+manifest-on,
   config-on+manifest-off, no-section, bad TOML), each asserting the
   three-way exclusion.

## A4 — Hardened gates script

`scripts/gates.sh` (executable, `set -euo pipefail`):

1. `cargo fmt --check`
2. `cargo clippy --workspace --all-targets -- -D warnings`
3. `cargo clippy --workspace --all-targets -- -W clippy::pedantic` —
   new-code clean OR `#[allow]` with justification comment; do NOT mass-
   rewrite existing code for pedantic in this wave (document counts of
   remaining pedantic lints as baseline in R1W1_NOTES.md).
4. `cargo test --workspace`
5. `cargo audit` (install if missing; `cargo install cargo-audit`) —
   fail on any RUSTSEC with no known unaffected version; record results.
6. `cargo deny check` (install `cargo-deny`; write deny.toml: licenses
   AGPL-compatible only, duplicate-crate warnings, ban list empty for
   now) — existing deps may surface issues: FIX what's cheap (version
   bumps), RECORD what's not (baseline in notes).
7. `gitleaks detect --no-banner` (install binary if missing; else
   document blocker and use `git log -p | grep` fallback patterns from
   the task history) — zero tolerance on secrets.
8. Script exits nonzero on any failure; summary table at the end.

Run the full gates.sh once; include the output table in R1W1_NOTES.md.

## Rules

- Offline only. No secrets committed. Atomic conventional commits.
- Files <800 lines. English everywhere.
- Do NOT touch plugin crates (B-wave scope) except where A2 requires a
  config-plumbing test touching plugin registration.
- Baseline notes doc (docs/R1W1_NOTES.md) is a deliverable: sccache
  numbers, pedantic/audit/deny baselines, decisions.

## Final summary (English)

What landed, sccache measured speedup, gates.sh first-run table,
pedantic/audit/deny baseline counts, deviations (justified), open
items for wave 2.
