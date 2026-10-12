# LIVE REPORT — mimo-rust (R1-W2a)

**Date:** 2026-10-12 · **Branch:** `r1w2a-live-mimo` · **Executor:** Claude Code CLI
**Scope:** mimo-rust SDK plugin ONLY, end-to-end against the REAL MiMo
upstream (`aistudio.xiaomimimo.com`) with a REAL pre-existing account.
No signup, no login, no OTP. simple-chat/TokenHarbor out of scope.

**Upstream budget:** 3 calls total (non-stream, stream, Go-oracle parity)
— well under the ≤10 cap. One account, ~2s pacing, tiny prompts.
The Go oracle (127.0.0.1:21223) shares the account pool; its healthz
after the smoke: `available: 1, cooldown: 0, disabled: 0` — untouched.

## 1. What ran

| # | Test item | Result | Evidence |
|---|---|---|---|
| 1 | Seed: Upstash row → plugin env (offline mapping test first) | **PASS** | 3-test python fixture suite + 4-test Rust `seed::tests` (redacted fixtures); live fetch via `scripts/live-seed-mimo.sh` |
| 2 | Host up: mimo-rust registered, `/healthz` Serving, `/v1/models` 3× `mimo-rust/` | **PASS** | healthz: `dev.oxcost.mimo-rust \| Serving`; models: `mimo-v2.6-flash`, `mimo-v2.6-pro`, `mimo-v2.6-pro-ultraspeed-studio` |
| 3 | Chat non-stream, `mimo-rust/mimo-v2.6-flash`, "Reply with exactly: OK" | **PASS** | HTTP 200, `object=chat.completion`, role=assistant, content `"OK"`, usage present |
| 4 | Chat stream (`stream:true`) | **PASS** | HTTP 200, 6 SSE data frames, first frame `chat.completion.chunk`, streamed content `"OK"`, ends `data: [DONE]` |
| 5 | Parity spot-check vs Go oracle (same tiny prompt, once) | **PASS** | Oracle: HTTP 200, `chat.completion`, assistant, `"OK"`, same usage shape (token counts differ — temperature variance, expected) |
| 6 | Negative: unknown model | **PASS** | HTTP 404, `code=model_not_found`, `type=invalid_request_error`, 0 upstream calls |
| 7 | Negative: bad host key | **PASS** | HTTP 401, `code=invalid_api_key`, `type=invalid_request_error`, 0 upstream calls |
| 8 | Teardown | **PASS** | Host process stopped; store artifacts under `/tmp/oxcost-mimo-live/` (session-persistence check below); oracle untouched |

## 2. Raw evidence (redacted)

### 2.1 Seeding (env-only, never persisted)

Account row fetched live from Upstash (`GET mimo2api:accounts:182787…`
via REST, value never echoed, `/tmp/mimo-row.json` deleted immediately
after the shape check). Shape seen (redacted, first 6 chars):

```
id=182787…  phone=182787…  password=tlz798…  enabled=true
label=primar…  user_id=265204…  pass_token=V1:pwR… (347ch)
device={"cUserId":"NyXqHh…","deviceId":"wb_63a1…","pass_ua":"web","uLocale":"zh_CN"}
session={cookie:"cUserId=NyXqHh…; userId=265204…; serviceToken=/vjQa8…;
          xiaomichatbot_serviceToken=…; xiaomichatbot_slh=…; xiaomichatbot_ph=…",
         ph:"7Z5fJQ…", fetched_at:1791802482}
state=available
```

`scripts/live-seed-mimo.sh` exported 10 env vars
(`MIMO_ACCOUNT_1_{ID,PHONE,PASSWORD,EGRESS}` +
`MIMO_RUST_SESSION_1_{USER_ID,PASS_TOKEN,COOKIE,PH,DEVICE}` +
`MIMO_RUST_DB_PATH`).

### 2.2 Host up

```
$ ./target/debug/oxcost --listen 127.0.0.1:8188   (OXCOST_API_KEYS set)
host log:  INFO oxcost: mimo-rust registered
           INFO oxcost::server: probe ok — Serving plugin="dev.oxcost.mimo-rust"
healthz:   dev.oxcost.mimo-rust | Serving | [mimo-v2.6-pro, mimo-v2.6-flash,
                                             mimo-v2.6-pro-ultraspeed-studio]
/v1/models: 3 mimo-rust/ models (MODELS-OK assertion)
```

### 2.3 Non-stream chat (upstream call 1)

```
status: 200
model: mimo-v2.6-flash
content: 'OK'
usage: {'prompt_tokens': 2235, 'completion_tokens': 32, 'total_tokens': 2267}
```

### 2.4 Stream chat (upstream call 2)

```
status: 200
SSE data frames: 6
first frame: chat.completion.chunk
streamed content: 'OK'
ends with [DONE]: True
```

### 2.5 Go oracle parity (upstream call 3, via oracle)

```
oracle status: 200
object: chat.completion   role: assistant   content: 'OK'
usage: {'prompt_tokens': 2235, 'completion_tokens': 114, 'total_tokens': 2349}
```

Status and shape identical; completion token count differs (114 vs 32) —
temperature variance between runs, explicitly out of scope per the task.
Note `prompt_tokens` matches exactly (2235) on both sides: same account,
same upstream accounting.

### 2.6 Negatives

```
unknown model  → HTTP 404  code=model_not_found  type=invalid_request_error
bad host key   → HTTP 401  code=invalid_api_key  type=invalid_request_error
```

### 2.7 Store write-through (post-run, redacted)

```
id=182787… uid=265204… pass_token=V1:pwR… cookie_set=True
ph=7Z5fJQ… device_set=True fetched_at=1791816676 state=available
```

The live session landed and the account stayed `available` — no
cooldown, no risk-control, no auth-expiry retry needed (the stored
cookie+ph served directly; `creds()` never had to renew).

## 3. Behavioral gaps found

1. **Env contract gap (fixed this wave, commit `b979a2d`):** the
   documented `MIMO_ACCOUNT_n_*` family expresses phone/password only —
   it cannot carry the live session (cookie/ph/pass_token/user_id/device)
   the account row ships with. Without a fix, every live test would
   trigger password login (forbidden here) or fail. Fix:
   `seed::seed_session_from_env` maps `MIMO_RUST_SESSION_n_*` onto the
   store columns after normal seeding, reusing `Store::apply_session`'s
   non-destructive COALESCE semantics; host registration gated on
   `MIMO_ACCOUNT_1_PHONE` (tokenharbor pattern). 4 regression tests in
   `plugins-in-process/mimo-rust/src/seed.rs`.
2. **Host wiring was missing (fixed, same commit):** mimo-rust was a
   workspace member but no host crate depended on it — the plugin could
   not be registered by the binary at all. DELIVERABLE_M4.md describes
   `MimoPlugin::new` as constructible but never wired the SDK registry
   path. The M4 "live parity harness behind MIMO_LIVE_E2E=1" mentioned
   in the deliverable does not exist in the tree (referenced only in
   docs) — this live smoke replaces it as the end-to-end proof.
3. **Minor (no action):** the host's `MIMO_RUST_DB_PATH` default is
   `mimo-rust.db` in the *host's working dir* — the script pins it to
   `/tmp/oxcost-mimo-live/mimo-rust.db` explicitly. A plugin-private
   data dir convention (like tokenharbor's `data/tokenharbor.db`) would
   be tidier; cosmetic, not blocking.
4. **No mock-vs-live divergence detected in the chat path itself**: the
   wiremock-tested pipeline (SSE dialect, OpenAI shaping, error mapping,
   pool acquire) behaved live exactly as the 237 offline tests predict.
   The only mock-missed surface was seeding (gap #1) — by construction,
   since mocks never exercise env → store.

## 4. Upstream-call ledger

| Call | Path | Model | Result |
|---|---|---|---|
| 1 | host → mimo-rust → aistudio (non-stream) | mimo-v2.6-flash | 200, "OK" |
| 2 | host → mimo-rust → aistudio (stream) | mimo-v2.6-flash | 200, "OK" |
| 3 | Go oracle (reference) | mimo-v2.6-flash | 200, "OK" |

Total: **3 ≤ 10**. No retries, no rate limits, no WAF interference.

## 5. Verdict

**mimo-rust is LIVE-READY: yes-with-fixes** — the fixes (env-session
seeding + host registration wiring) are already landed, tested, and
committed on this branch (`b979a2d`): offline 237 mimo-rust + 48 oxcost
tests green, clippy `-D warnings` clean, `cargo fmt --check` clean. With
those commits in, the plugin served real traffic correctly on first
contact: both transports, correct error paths, parity with the Go
oracle, and clean pool behavior on a shared live account.
