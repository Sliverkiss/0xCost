# R1W1 notes — infra wave baselines & decisions

Wave-1 deliverables: A1 shared compile cache (sccache), A2 host config
file, A4 hardened pre-merge gates. This file records the measured
baselines and the decisions behind them (spec: docs/REFACTOR_SPEC.md
Part A).

## A1 — sccache

- `sccache 0.18.0` installed via `cargo install sccache` (ARM64 build
  from source; the pinned 0.10.0 release failed to compile on this
  toolchain, 0.18.0 is current and works).
- Wired globally in `/root/.cargo/config.toml` as `rustc-wrapper =
  "sccache"`; per-project `target-dir` kept (sccache dedups dependency
  artifacts across projects by content). Cache dir:
  `~/.cache/sccache`, 10 GiB cap, ~600 MiB after priming.
- Measured on a clean clone in /tmp (2-core ARM box, dev profile,
  `cargo build --workspace`):

  | run | time | cache |
  |---|---|---|
  | cold, no sccache (RUSTC_WRAPPER=) | 262 s | — |
  | cold, sccache first run (primes cache) | 150 s | 0% hits |
  | cold target dir, cache warm (second run) | 109 s | Rust hits ~30% |

  Note the warm-run Rust hit rate is ~30% because the plugin crates'
  own units recompile (sccache only caches `rustc` invocations it can
  key; incremental/linkage work stays local). Practical effect during
  the refactor: parallel worktrees share dependency artifacts, so a
  fresh worktree avoids a full cold build (~2.4× faster in the measured
  case, more when several worktrees are interleaved).

- Cleanup: the /tmp bench clone (`/tmp/r1w1-bench`) was removed after
  measurement. `/root/oxcost/target` deliberately untouched.

## A2 — host config file

- New module `crates/oxcost/src/config.rs`: TOML schema
  (`[server].bind`, `[server].api_keys` entries are literals or
  `env:VAR` references, `[plugins."<id>"].enabled`), `--config <path>`
  CLI arg (was a forward-compat stub, now real), parse errors carry
  TOML line numbers, startup-only read (no hot reload — YAGNI,
  restart to apply).
- Precedence: config `enabled` overrides manifest
  `routing.enabled` in both directions; missing section keeps the
  manifest default. Overrides are applied to registry manifests after
  validation (an override cannot smuggle an invalid manifest) and
  before instance construction, so the §6 three-way exclusion (no
  routing, no catalog, no probing) flows through existing paths.
- `probe_all` now skips disabled plugins (previously it probed
  everything; the §6 "off = invisible" probe leg was missing).
- listen precedence: `--listen` flag > `OXCOST_LISTEN` env > config
  `bind` > dev default `127.0.0.1:8080`. api_keys: `OXCOST_API_KEYS`
  env > config file (env stays the operator override). No config →
  previous behavior unchanged (back-compat verified by tests).
- `oxcost-core`: `PluginRegistry::list_mut()` and
  `PluginInstance::set_routing_enabled()` added as the override seams.
- Tests: `crates/oxcost/src/config.rs` unit tests (schema, precedence,
  env refs, bad TOML lines), `tests/config.rs` integration matrix
  (config-off+manifest-on, config-on+manifest-off, no-section both
  ways, foreign-section ignored, probe-frozen/serve legs),
  `tests/cli.rs` end-to-end (--config bad TOML exit 2 with line, file
  missing exit 2, valid config passes --check).

## A4 — gates

- `scripts/gates.sh`: fmt → clippy -D warnings → clippy pedantic
  (baseline-gated) → test → cargo audit → cargo deny check → gitleaks.
  Nonzero exit on any failure; summary table at the end.
- Tools installed: `cargo-audit`, `cargo-deny 0.20.2`,
  `gitleaks 8.28.0` (arm64 release binary in /usr/local/bin).
- `deny.toml`: licenses allow-list = permissive set + AGPL-3.0 (the
  workspace's own license; all members declare it). `wildcards = deny`
  with `allow-wildcard-paths = true`, plus `publish = false` on the
  workspace (accurate: local deployment only, never published) so
  internal path deps don't trip the wildcard lint. Ban list empty.
- gitleaks: 5 historical findings, all individually reviewed as false
  positives (Supabase anon key — public browser constant documented in
  the tokenharbor README; test/e2e/README placeholders). Recorded in
  `.gitleaksignore` with per-line rationale. Zero tolerance for
  anything new.

### Baselines (recorded for regression gating)

- clippy pedantic baseline: 973
  (existing code; wave-1 fixed the lints its own new code introduced.
  Top categories: doc-markdown 295, missing-errors-doc 128,
  must_use 179. Mass rewrite is out of scope this wave — spec A4.)
- cargo audit: 0 RUSTSEC advisories across 297 locked dependencies.
- cargo deny duplicates (warnings, not failures): 6 crates —
  base64, getrandom, hashbrown, reqwest, syn, windows-sys.
  (version-bump consolidation deferred to a later wave; recorded here
  as the baseline.)
- gates.sh first run (2026-10-12, after all fixes):

  | GATE | STATE | NOTES |
  |---|---|---|
  | fmt | PASS | |
  | clippy(default) | PASS | -D warnings |
  | clippy(pedantic) | PASS | baseline 973, now 973 |
  | test | PASS | all crates |
  | audit | PASS | 0 advisories |
  | deny | PASS | 6 dup warnings (baseline) |
  | gitleaks | PASS | 71 commits, 0 leaks |

## Deviations

- Task schema showed `bind = "127.0.0.1:8123"` as the example; the
  actual current default is `127.0.0.1:8080` and stays unchanged
  (the task comment itself says "default unchanged from current").
- `OXCOST_LISTEN` env support added while wiring precedence (the env
  var was previously dead — parsed nowhere); it is the documented
  operator override level between the config file and the `--listen`
  flag. No behavior change when unset.
- cargo-deny's `unresolved-workspace-dependency` diagnostics turned
  out to be downstream noise from the wildcard lint (they fire when
  the workspace dep can't be labeled during a wildcard error);
  enabling `allow-wildcard-paths` + `publish = false` resolved both.

## Open items for wave 2

- A3 live verification (per-plugin live reports) — blocked on this
  wave's merge only, not on any code here.
- Duplicate-crate consolidation (6 crates) for a cleaner deny tree.
- Pedantic debt burn-down (973 lints; biggest wins: doc_markdown
  backticks and missing_errors_doc sections, both mechanical).
- Hot config reload stays out of scope (YAGNI) — revisit if ops asks.
