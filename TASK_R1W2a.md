# TASK R1-W2a — mimo-rust LIVE test: real account, real conversation (no login)

> Executor: Claude Code CLI. Working dir: `/root/oxcost` directly on main
> (branch `r1w2a-live-mimo`, create from main). Model: dashscope/glm-5.3.
> **Language**: English. Live test ONLY — code changes only if a bug
> blocks the test (then fix + test, minimal commit).

## Mission

Prove the mimo-rust SDK plugin end-to-end against the REAL MiMo upstream
with a REAL existing account (no signup, no login — the account row
already carries session/pass_token). This is wave-2 (A3) live
verification for mimo-rust only. simple-chat/TH are NOT in scope.

## Account source (env-injected, never committed)

The maintainer's Upstash Redis (credentials injected via env by the
orchestrator at dispatch time — do NOT echo them, do NOT write them to
any file):
- `UPSTASH_REDIS_REST_URL=https://known-mudfish-135209.upstash.io`
- `UPSTASH_REDIS_REST_TOKEN` — injected
- Account row: key `mimo2api:accounts:18278724616` (GET only). Fields:
  id, phone, password, enabled, label, user_id, pass_token, device,
  session, state=available.

The plugin's env seeding contract is `MIMO_ACCOUNT_n_*` (see
plugins-in-process/mimo-rust/src/config.rs and DELIVERABLE_M4.md §env
contract). Map the Upstash row into those env vars (this mapping step is
scriptable: `scripts/live-seed-mimo.sh` may fetch the row via REST and
export env vars — no secrets persisted, document the script but run it
with live env).

**CRITICAL CAUTION**: the running Go mimo2api (127.0.0.1:21223) shares
this account pool. ONE account, FEW requests (≤10 upstream calls total),
normal pacing (1.5-2s), tiny prompts. Do not loop, do not stress, do not
touch other accounts. This is a smoke, not a soak.

## Test plan (record everything in docs/LIVE_REPORT_MIMO.md)

1. **Seed**: fetch the account row → construct plugin env (offline
   mapping test first: unit-test the row→env parsing with a redacted
   fixture).
2. **Host up**: start the host (axum) with mimo-rust registered,
   OXCOST_API_KEYS set for local test. Verify `/healthz` shows the
   plugin Serving and `/v1/models` lists the three chat models with
   `mimo-rust/` prefix.
3. **Chat (non-stream)**: `POST /v1/chat/completions` model
   `mimo-rust/mimo-v2.6-flash`, tiny prompt ("Reply with OK"), max
   tokens small. Assert OpenAI-shaped response with non-empty content.
4. **Chat (stream)**: same, `stream:true`. Assert SSE chunk framing and
   `[DONE]`, content arrives.
5. **Parity spot-check**: send the SAME tiny prompt to the Go oracle
   (127.0.0.1:21223, key from /root/mimo2api/data/deploy.env — read at
   runtime, never persist) once; compare status/content shape (not exact
   tokens — temperature variance).
6. **Negative paths**: unknown model → 404 model_not_found; bad host
   key → 401.
7. **Teardown**: stop host; write LIVE_REPORT_MIMO.md — what ran, raw
   evidence (redacted), pass/fail per item, any behavioral gaps found
   (mock tests missed something? document precisely).

If a live test FAILS due to a plugin bug: fix it (minimal), add the
regression test, re-run. Upstream-unrelated flakes (rate limit, WAF):
document, retry once after 30s, then record as environmental.

## Rules

- ≤10 upstream calls total. One account. No login/signup attempts.
- No secrets in any file (env only). Redact tokens/phones in the report
  (first 6 chars).
- Conventional commits; English; report is the primary deliverable.
- Go oracle is read-only reference — do not restart or reconfigure it.

## Final summary (English)

Pass/fail table per test item, evidence pointers, gaps found, verdict:
is mimo-rust LIVE-READY (yes / yes-with-fixes / no)?
