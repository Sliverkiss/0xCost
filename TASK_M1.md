# TASK M1 — OxCost Host Skeleton (Rust workspace)

> Executor: Claude Code CLI, invoked by the maintainer's orchestrator (Hermes).
> Working directory: `/root/oxcost` (local-only git repo; NEVER push to any
> remote). Model: `dashscope/glm-5.3` via `--model`.
>
> **Read these first, in order**:
> 1. `CLAUDE.md` (repo root)
> 2. `docs/PLUGIN_CONTRACT.md` — THE authoritative spec. Every design decision
>    below is a consequence of it. If you find a conflict, the contract wins.
>
> **Language**: think and write in English. All code comments, commit messages,
> docs, and your final summary MUST be English.

---

## 1. Mission

Bootstrap the OxCost host framework: a Rust workspace with four crates, a
working `plugin.toml` manifest parser/validator, and the plugin lifecycle
state machine — **everything testable offline, zero plugin processes needed**.
No HTTP server wiring beyond a compilable axum stub; serving and proxying come
in M1.5/M2.

Scope guard: skeleton + contract layer only. Do NOT implement routing/LB/
circuit-breaker logic, sidecar HTTP client, or any upstream adapters.

## 2. Workspace layout

```
oxcost/
├── Cargo.toml                  # [workspace] members = crates/*
├── rust-toolchain.toml         # pin stable channel
├── .gitignore                  # target/, *.log, data/, .env*
├── crates/
│   ├── oxcost-protocol/        # OpenAI wire types (chat completions req/resp,
│   │                           # SSE event types). Data structs + serde only.
│   ├── oxcost-contract/        # plugin.toml model, parse, validate, version
│   │                           # negotiation, capability model
│   ├── oxcost-core/            # PluginRegistry + plugin lifecycle state
│   │                           # machine (no I/O; pure state transitions)
│   └── oxcost/                 # bin crate: main.rs parses CLI (--config),
│                               # loads manifests from a plugins dir, prints
│                               # the registration table, exits 0. axum dep
│                               # declared but unused (server stub fn only).
├── docs/                       # existing; do not touch PLUGIN_CONTRACT.md
└── plugins/
    └── example-remote/         # a sample valid plugin.toml for tests (mode=remote)
```

Version pins: latest stable of `tokio`, `axum`, `serde`, `serde_json`,
`toml`, `thiserror`, `tracing`, `tracing-subscriber`. Workspace-level
`[workspace.dependencies]` for shared versions. Edition 2021.

## 3. Deliverable A — oxcost-contract: plugin.toml

Implement exactly the manifest schema of `docs/PLUGIN_CONTRACT.md` §3:

- Structs: `PluginManifest` with required fields (`manifestVersion`, `id`,
  `name`, `version`, `apiVersion`, `license`) and tables `[entry]`
  (`mode`: `host-spawn|external|remote`, `cmd`, `port`, `url`), `[health]`
  (`path`, `interval_secs`, `timeout_secs`), `[routing]` (`enabled` default
  true, `models`, `unknown_model`: `pass|fallback=<model>|reject`),
  `[capabilities]` (bool flags: chat default true; embeddings, images,
  audio_speech, audio_transcription, audio_translations, video, moderations,
  responses, web_search, files, batches, realtime), `[auth]` (`keys`),
  `[limits]` (`max_inflight`, `cooldown_secs`), `[source]` (`repository`).
- Validation rules (each a typed error, see §5):
  1. `id` matches `^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)+$`
  2. `manifestVersion` ∈ supported set {0.1}; `apiVersion` ∈ {v1}
  3. `routing.models` non-empty, no duplicates; plugin prefix (first path
     segment) == last segment of `id`
  4. `auth.keys` starts with `env:`; reject any value that looks like a
     literal secret (contains `sk-`, or `^[A-Za-z0-9_\-]{32,}$`)
  5. `entry.mode == host-spawn` requires `cmd`; `external` requires `port`;
     `remote` requires `url` (http/https) and forbids `[health]`
  6. `capabilities.batches == true` requires `capabilities.files == true`
  7. `unknown_model` parses: bare `pass`/`reject`, or `fallback=<model>`
- Version negotiation API: `supports(manifest) -> Result<(), Unsupported>`.
- Capability model: enum `Capability` (13 variants) + `CapabilitySet` with
  core/experimental classification per contract §4 (experimental: responses,
  files, batches, realtime).

## 4. Deliverable B — oxcost-core: registry + lifecycle

- `PluginState`: `Registered → Probing → Serving ⇄ CircuitOpen(cooldown) →
  HalfOpen → Serving`; plus `Disabled` reachable from any state; `Removed`
  terminal. Transitions are pure functions returning events — no I/O, fully
  unit-testable. Illegal transitions are type-level impossible where
  reasonable (e.g. `Disabled` plugins cannot serve; per contract §6 the
  disable switch is orthogonal to the circuit breaker and never auto-recovers).
- `PluginRegistry`: insert (id-unique, runs full manifest validation), get,
  list, remove (contract §6: removal deregisters immediately), enable/disable
  toggling `routing.enabled` semantics (Disabled excludes from listing and
  stops probing — probing behavior itself is M1.5, model it as state only).
- No tokio required in this crate. Keep it sync and pure.

## 5. Quality gates (all MUST pass before you report done)

```
cargo build --workspace          # zero errors
cargo clippy --workspace -- -D warnings   # zero warnings
cargo fmt --check                # clean
cargo test  --workspace          # all green, offline
```

Tests REQUIRED (offline, no network):
- contract: ~15 unit tests — id regex, version sets, model prefix rule,
  secret-literal rejection (both patterns), mode/field requirements,
  batches→files dependency, unknown_model parsing, capability defaults,
  experimental classification
- core: ~12 unit tests — legal transition paths, Disabled orthogonality
  (disable while CircuitOpen works; enable re-enters Probing), registry
  duplicate-id rejection, remove semantics, enable/disable listing effect
- bin: 1 integration test — run against `plugins/example-remote/`, assert
  exit 0 and registration table on stdout; plus a fixture with an invalid
  manifest asserting the specific validation error
- protocol: serde round-trip tests for chat completion request/response +
  SSE chunk structs (use canonical OpenAI JSON shapes)

## 6. Working rules

1. Atomic commits, one concern each, Conventional Commits
   (`feat: ...`, `test: ...`, `chore: ...`).
2. Files < 800 lines. Module-per-concern.
3. NEVER commit secrets. The example manifest must use `env:...` auth refs.
4. NEVER push to any remote. Work on branch `m1-skeleton`, keep `main` clean.
5. Do not modify `docs/PLUGIN_CONTRACT.md` or `CLAUDE.md` (if you believe
   something in them is wrong, note it in your final summary instead).
6. No turns limit — run until all gates in §5 pass and §3–§4 are complete.
7. Final summary (English): what was built, crate/file inventory, gate
   outputs (paste the four command results), commit list, and any contract
   issues you noticed (if none, say so explicitly).

## 7. Acceptance (maintainer will verify independently)

- Re-run §5 gates fresh in a clean clone of branch `m1-skeleton`.
- Contract §3 schema and §6 lifecycle semantics correctly reflected.
- No scope creep into M1.5/M2 (no HTTP serving, no sidecar client, no LB).
