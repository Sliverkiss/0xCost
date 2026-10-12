# TASK M4-SC — simple-chat SDK plugin (Go gateway → in-process Rust)

> Executor: Claude Code CLI. Working tree: `/root/oxcost-wt-sc`, branch
> `plugin-simple-chat` (already created from main). Model: GLM-5.3-Flash.
> **Language**: English output.

## 0. Mission

Port simple-chat (Go) to `plugins-in-process/simple-chat/` as an
OxcostPlugin — `deepseek-flash` web chat with reasoning + opt-in web
search, inside the host process.

Inputs (read-only, do not modify):
- Port dossier: `/root/sc-recon/dossier/` — PORT_DESIGN.md (342 lines),
  RISK_REGISTER.md (103 lines), PARITY_CHECKLIST.md (48 items, 8 sections;
  PC-41 = Upstash schema compatibility). **De-risk order: R2 (watchdog
  cut-vs-EOF typed classification) and R3 (park-latch/quarantine
  semantics) offline BEFORE the main port; R5 (two-request live TLS
  smoke) at integration time.**
- Go source (read-only): `/root/sc-recon/upstream` (cloned at recon).
  NEVER modify it or the GitHub repo.
- Contract: docs/PLUGIN_CONTRACT.md (§4 revised strings); SDK:
  crates/oxcost-sdk.

## 1. Binding maintainer decisions (do not revisit)

- **Account pool: plugin-private SQLite.** Upstash Redis is a TEST-TIME
  seed source only (PC-41 read-only import; env-injected creds
  `UPSTASH_REDIS_REST_URL` / `UPSTASH_REDIS_REST_TOKEN`, GET only, never
  SET/DEL).
- **Exclusive ownership cutover** (R1 CRITICAL): the plugin assumes it is
  the ONLY process operating these upstream accounts. The Go binary will
  be STOPPED at plugin cutover (maintainer action, not yours). Document
  the assumption in the plugin README; seed CLI prints a concurrency
  warning when importing (per dossier's 6-step order).
- TLS: reqwest + rustls plain (Go passed the WAF with stock TLS; rquest
  is the escape hatch, deliberately NOT pre-added).
- Upstream failures → 502 `upstream_error` (revised §4), codes preserved.

## 2. Scope (v1 — follow PARITY_CHECKLIST items exactly)

1. **Account store** (R3 first): SQLite schema mirroring Go's Account
   shape (mobile/email/password/region/device_id/channel + park fields +
   session_token), sticky fail-closed park latch, unknown-deadline
   quarantine, monotonic deadlines — per RISK_REGISTER R3 design.
2. **Upstream client**: per dossier §3 (endpoints, headers, login flow,
   device profile). One login per account at a time; device profile
   stable per account (persisted in SQLite).
3. **Chat**: stream & non-stream; `reasoning_content` default-on with
   per-request opt-out; `image_url` content parts; SSE watchdog with
   typed cut-vs-EOF classification (R2 — stream cancellation maps to a
   typed error, NEVER silent EOF); retry ladder per Go; honest error
   termination.
4. **Web search**: opt-in `"search":{"type":"enabled"}` with structured
   citations passthrough. Standalone `/v1/web_search`: answer M3 open
   question #1/OQ-1 — implement as capability `web_search` on the SDK
   manifest IF the trait hook is cheap passthrough; otherwise declare
   reserved-unsupported and note the follow-up. Justify the choice.
5. **Model**: exactly `simple-chat/deepseek-flash` (single, like Go).
6. **Error mapping**: table-driven per dossier §6 → revised §4 strings.

## 3. Tests

- Park latch / quarantine state machine: offline table-driven (R3
  vectors).
- Watchdog classification: offline, including cut-vs-EOF fixtures (R2).
- PC-41 seed importer: fixture records offline + live-optional behind
  `SC_LIVE_UPSTASH=1` (GET-only), NOT run in gates.
- SSE mapping: fixture-driven (reasoning_content, citations, error
  frames, [DONE]).
- Gates: build / clippy -D warnings / fmt / test green; files <800
  lines; no secrets committed; atomic conventional commits.

## 4. Out of scope

Account registration automation, multi-model, admin UI, metrics
dashboards, dylib loading.

## 5. Deliverable summary (English)

Trait impl map, SQLite schema vs Go Account JSON (round-trip proof via
PC-41 fixtures), watchdog classifier design, search capability decision
(OQ-1) with justification, test counts, env var contract, cutover
checklist for the maintainer (steps to stop Go + start plugin + verify),
deviations from dossier (justified), answers to M3 open questions #1
(streaming variant) and #2 (Retry-After on PluginError).
