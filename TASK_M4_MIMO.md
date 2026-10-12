# TASK M4-MIMO — mimo-rust SDK plugin (Go mimo2api → in-process Rust)

> Executor: Claude Code CLI. Working tree: `/root/oxcost-wt-mimo`, branch
> `plugin-mimo-rust` (already created from main). Model: GLM-5.3-Flash.
> **Language**: English output.

## 0. Mission

Port mimo2api (Go) to `plugins-in-process/mimo-rust/` as an OxcostPlugin —
chat (+ models) against MiMo's web backend, inside the host process.

Inputs (read-only, do not modify):
- Port dossier: `/root/mimo-rust-recon/dossier/` — PORT_DESIGN.md (581
  lines, per-subsystem Go→Rust), PARITY_CHECKLIST.md (155 lines, the
  acceptance surface), RISK_REGISTER.md (194 lines, three de-risk
  experiments). **Follow the dossier's de-risk order: run R1 (pool state
  machine scratch port + replay Go's pool tests), R2 (TLS three-client
  replay), R3 (live SSE transcript fixture + parser) BEFORE the main
  port.** Record each experiment's result in the dossier dir (append,
  don't rewrite).
- Go source (read-only): `git worktree` already at /tmp/mimo2api-src
  (branch mimo2api). NEVER modify it or /root/mimo2api.
- Live oracle: http://127.0.0.1:21223 (running mimo2api, healthy; key in
  /root/mimo2api/data/deploy.env — read into env vars at runtime only).
- Contract: docs/PLUGIN_CONTRACT.md; SDK: crates/oxcost-sdk.
- Maintainer-ratified decisions: TLS = reqwest+rustls v1, wreq fallback
  pre-planned (only if R2 proves divergence); upstream failures → 502
  `upstream_error` (revised §4), all `code`s preserved; account pool in
  plugin-private SQLite (Upstash is TEST seeding only).

## 1. Scope (v1 — follow PARITY_CHECKLIST exactly)

1. **Account pool** (R1 first): port Go's pool state machine
   (tryAcquireLocked, health transitions, retry ladder constants) to Rust
   over SQLite; replay Go's pool unit tests as Rust tests — PARITY means
   the same test vectors pass. Accounts seeded from env
   (`MIMO_ACCOUNT_1_PHONE=...` style; real values come from Upstash at
   test time — env only, never committed).
2. **Login chain**: MD5-upper password signing, five hand-tuned header
   sets, two-step SMS flow per dossier §1. Login happens only when the
   pool requires it (lazy); SMS code entry is an operator step — design
   the waiting state so the plugin can pause an account mid-login without
   blocking others (the Go operator flow, ported).
3. **Model surface v1**: chat models only — `mimo-v2.6-flash`,
   `mimo-v2.6-pro`, `mimo-v2.6-pro-ultraspeed-studio` (the three the live
   oracle proves daily). tts/asr/voiceclone: declare in manifest as
   UNIMPLEMENTED-reserved (capability flags false), listed in
   PARITY_CHECKLIST as later milestones. `models()` advertises the three,
   namespaced `mimo-rust/<model>`.
4. **Chat + streaming** (R3 first): SSE dialect per dossier (ThinkSplitter
   edge cases: no-space colons, NUL stripping, partial-marker holdback,
   EOF-before-[DONE]) — parser built against the live-captured fixture,
   then upstream client, then OpenAI chunk mapping incl. web_search
   annotations passthrough per Go citations.go (search-off default;
   webSearch:"off" like TH v1 — annotations only if search explicitly on).
5. **Error mapping**: upstream failures → 502 upstream_error with Go's
   `code`s preserved; client errors → 400 invalid_request_error; unknown
   model → 404 model_not_found; parity-diff vs the live oracle
   (same request → same status+type+code) as the acceptance test.

## 2. Tests

- Pool state machine: replayed Go vectors (offline, SQLite temp file).
- SSE parser: fixture-driven (R3 transcript + edge cases from dossier).
- Error parity: offline table-driven; live parity harness behind
  `MIMO_LIVE_E2E=1` (same request to plugin and to 127.0.0.1:21223,
  compare status/type/code) — not run in gates.
- Gates: build / clippy -D warnings / fmt / test green; files <800 lines;
  no secrets; atomic conventional commits.

## 3. Out of scope

tts/asr/voiceclone implementation, image input, account registration
automation (operator-driven SMS only), LB across accounts beyond Go's own
algorithm (port as-is).

## 4. Deliverable summary (English)

De-risk experiment results (R1/R2/R3 with numbers), trait impl map, pool
state machine notes vs Go, SSE parser coverage, parity test matrix, test
counts, env var contract, deviations from dossier (if any, justified),
answers to M3 open questions #1 (streaming variant) and #2 (Retry-After
on PluginError?).
