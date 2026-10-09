# mimo2api — Test Design Document (TDD)

**Project:** mimo2api — OpenAI-compatible API gateway for Xiaomi MiMo AI Studio
**Verifies:** `docs/SDD.md` (Go + Docker design, approved 2026-10-07)
**Test framework:** Go native `testing` + `httptest` — zero non-stdlib test dependencies
**Date:** 2026-10-08

---

## 1. Test strategy overview

Three layers. Each layer has a distinct job; no layer duplicates another's coverage.

### 1.1 Unit tests (in-package, no network, no filesystem unless tmpdir)

Cover **pure logic and single-package invariants**:

- `internal/adapter` — thinking-marker splitter (3-state machine over byte chunks),
  conversation fingerprint, OpenAI chunk shaping, usage mapping, system-message splice,
  model alias normalization. Pure functions; no I/O at all.
- `internal/session` — fingerprint store semantics: continuation match, replay guard,
  TTL expiry, LRU eviction. Driven by a fake clock — no real sleeping.
- `internal/pool` — state machine transitions, cooldown ladder math, semaphore
  accounting. Fake clock + fake probes (probe outcome injected, not dialed).
- `internal/config` — env parsing, defaults, validation.
- `internal/accounts` — load/save round-trip, atomic write, permissions, duplicate
  rejection. Uses `t.TempDir()`.
- `internal/mimo` — SSE byte parser (fixture-fed `io.Reader`s, no server), XSSI
  prefix parser, login/renewal response decoding.

### 1.2 Integration tests (in-package, `httptest.Server` mock upstream + recorded SSE fixtures)

Cover **wire behavior**: the full request path `api → session → pool → mimo → upstream`
against a programmable mock MiMo upstream, and `mimo.Client` login/STS flows against a
mock Xiaomi passport. This is the layer that pins:

- exact OpenAI SSE chunk JSON shape (§5.4),
- upstream payload correctness (msgId/conversationId/previousDialogueId, §2.4),
- stickiness end-to-end (delta-only sends, §4.2/§5.3),
- retry / commit-boundary / error-mapping behavior (§7).

Mock upstream (`testutil.MockUpstream`, §2.2) is an `httptest.Server` that: routes by
account identity (distinct `xiaomichatbot_ph` per scripted account), serves recorded SSE
fixtures as `text/event-stream`, can be scripted to return 429/401/451/461/5xx, stall,
abort mid-stream, or return an empty stream — and records every request it receives
(msgId, conversationId, previousDialogueId, model, full payload) for assertions.

### 1.3 Live verification (real aistudio, real test account, gated)

Cover **protocol drift detection only** — things mocks cannot prove: the real passport
login flow, real SSE byte format quirks, real 429 semantics, real conversation GC.
Gated by env `MIMO2API_LIVE=1`; every LIVE-* test begins with
`if os.Getenv("MIMO2API_LIVE") != "1" { t.Skip("live tests gated by MIMO2API_LIVE=1") }`.
Sequential (`no t.Parallel`), rate-limit aware (sleep between upstream calls), one pass
per run. Credentials come exclusively from `data/accounts.json` (SDD §8) — never from
test source. LIVE-* tests mirror SDD §8.2 steps 1–6 and resolve Appendix B Q1–Q4.

### 1.4 Layer boundaries (what each layer does NOT do)

| Layer | Does not |
|---|---|
| Unit | dial network, start servers, sleep in real time |
| Integration | touch aistudio.xiaomimimo.com, account.xiaomi.com, or any real credential |
| Live | run in default CI (`go test ./...` must pass without it), assert performance |

### 1.5 Global test rules

- All integration/unit suites run under `go test -race ./...` — the pool and session
  store are mutex-guarded shared state; the race detector is a required gate.
- Table-driven tests wherever the input space is a matrix (error mapping, splitter,
  model normalization).
- No test sleeps to advance time. Anything time-dependent takes a `Clock` interface;
  tests inject `testutil.FakeClock` and call `Advance()`.
- Fixture bytes are committed under `testdata/` and never generated at runtime.

---

## 2. Test file map

One `_test.go` per package plus a shared `internal/testutil` package. Go convention:
`exported_test.go` when a test needs to reach into package internals.

| File | Package | Contains |
|---|---|---|
| `internal/adapter/adapter_test.go` | adapter | SSE-006..012 (splitter), API-003/004/017 (chunk shaping, usage mapping, system splice), API-013 (model normalization), turn-plan construction. (STICKY-001 fingerprint retired — see §3.1.) |
| ~~`internal/session/session_test.go`~~ | — | **deleted** (ADR 0017) — the store, STICKY-002..013 and SESS-011/012/053 went with it |
| `internal/pool/pool_test.go` | pool | POOL-001..016 (selection, cooldown, probes, semaphores; mock probe results injected) |
| `internal/mimo/sse_test.go` | mimo | SSE-001..005 (byte parser over fixtures) |
| `internal/mimo/login_test.go` | mimo | LOGIN-001..009 (passport flow vs httptest mock) |
| `internal/mimo/renew_test.go` | mimo | RENEW-001..009 (STS flow, 401-driven refresh, 6h sweep) |
| `internal/mimo/device_test.go` | mimo | DEV-001..007 (device identity presented/harvested/replayed; rotated passToken) |
| `internal/api/device_persist_test.go` | api | DEVPL-001 (stored device reaches Auth2; harvested identity hits disk) |
| `internal/mimo/client_test.go` | mimo | SSE-013 (empty-response retry), upstream payload shaping, fastchat routing, header set |
| `internal/api/api_test.go` | api | API-001..025 (endpoint wiring, full-stack against testutil.MockUpstream) |
| `internal/api/auth_test.go` | api | AUTHMID-001..008 (auth middleware) |
| ~~`internal/api/sticky_test.go`~~ | — | **deleted** (ADR 0017); STICKY-010's rule lives on as DISCONNECT-001 in `nostick_test.go` |
| ~~`internal/session/identity_test.go`~~ | — | **deleted** (ADR 0017) — the identity ladder needs the store it keyed |
| ~~`internal/api/session_test.go`~~ | — | **deleted** (ADR 0017) — the ladder end-to-end, the header and the debug line are gone |
| `internal/api/nostick_test.go` | api | NOSTICK-001/002 (fresh `conversationId` per request, chaining only within a request's replay) + DISCONNECT-001 (a client that hangs up returns the lease) — rewritten for ADR 0017 |
| `internal/pool/attribution_test.go` | pool | POOLATTR-001..007, 009..011 (per-class ladders and counters, class recorded with the cooldown, no upstream Retry-After) |
| `internal/api/failure_class_test.go` | api | POOLATTR-003, 011 (the classifier table: stall/EOF/reset/dial/deadline are transport, an upstream answer never is) |
| `internal/api/poolattr_e2e_test.go` | api | POOLATTR-004, 012, 013 (a live mid-stream stall cools on the transport ladder; 5xx and transport stay apart; audio calls are attributed) |
| `internal/pool/risk_test.go` | pool | RISK-001..007 (451/461 recorded and correlated, never permanent, operator undo, unknown account) |
| `internal/api/risk_e2e_test.go` | api | RISK-001, 004, 006 (the 451/461 path end to end: fail-closed disable, evidence recorded, cross-egress correlation) |
| `internal/api/pool_admin_test.go` | api | POOLVIEW-001..006 (`/v1/admin/pool` auth, masking, no-credential scan, reason/class fields; `/healthz` unchanged) |
| `internal/egress/egress_test.go` | egress | EGRESS-001..005 (label charset, env-name derivation, a missing endpoint fails and names the variable, errors never echo an endpoint's credentials, per-label resolution) |
| `internal/egress/startup_test.go` | egress | EGRESS-009..011 (accounts.json + environment together: a label without an endpoint fails, a malformed label is rejected at load, relabelling is reversible) |
| `internal/api/egress_e2e_test.go` | api | EGRESS-006..008 (a labelled account's chat and catalog calls really leave through its proxy, an unlabelled account's do not, a lease carries its egress) |
| `internal/api/errors_test.go` | api | ERRMAP-001..012 (table-driven, §7.4) |
| `internal/accounts/accounts_test.go` | accounts | ACC-001..007 |
| `internal/config/config_test.go` | config | CONF-001..007 |
| `deploy/toolforge/` (PROVENANCE.md) | — | tool calling, vendored: the middleware's own `pytest` suite (`make test-toolforge`) plus the live acceptance in §2.2. It sits in front of chat and calls the ordinary surface, so no gateway code is involved and there is nothing here to unit-test (ADR 0012) |
| `internal/adapter/redirect_test.go` | adapter | REDIR-UNIT (redirect chains, cycles, `Usable`, the floor merge keeping a superseded row superseded) |
| `internal/api/models_redirect_test.go` | api | MODELS-014/015, CHAT-REDIR-001, AUDIO-REDIR-001 |
| `internal/api/refresh_redirects_test.go` | api | REFRESH-001/002 (a retirement learned from the upstream's own refusal) |
| `internal/testutil/testutil.go` (+ `testdata/`) | testutil | shared helpers (§2.2) |
| `internal/live/live_test.go` | live | LIVE-001..011 (LIVE-004/005/010 and LIVE-SESS-001/002 retired with the streaming path and the store, ADR 0017) — separate package so it is never run implicitly; gated on `MIMO2API_LIVE=1` |
| `docker/docker_test.sh` (invoked via `go test` wrapper or Makefile target) | — | DOCKER-001..006 — shell-driven, skipped when `docker` binary absent |

### 2.1.1 The deployment's acceptance (ADR 0016)

One container, one published port, non-streaming chat — so the acceptance is
end-to-end against the running stack:

| Check | What it proves |
|---|---|
| `/healthz` on `:21223` | the published port answers for the gateway itself: one container, no relay |
| `/v1/models` on `:21223` | the real catalog comes back on the published address (7 models live) |
| `/v1/audio/speech` on `:21223` | binary responses work on the same address (`audio/wav`, RIFF/WAVE header) and multipart uploads survive on `/v1/audio/transcriptions` |
| wrong key → 401 | the published port is authenticated |
| `stream: true` → `400 stream_unsupported` | the deployed default is non-streaming (ADR 0016) and the refusal names the fix — checked live, together with **no upstream call** in the unit case STREAM-001 |
| `stream: false` → 200 | plain chat is served on the published address |
| tools in the body → `X-Mimo-Warnings` | the boundary is explicit: the gateway never pretends to support tools |
| `ss -ltn \| grep 21223` → `127.0.0.1` | the port is loopback-only; the reverse proxy is the only public path |

### 2.1.2 Retired: the tool layer's acceptance (ADR 0012, closed by ADR 0016)

Historical, kept because it records what was verified while the middleware ran:
`make test-toolforge` in a container; a tools request to `:8081` returning
`finish_reason: tool_calls`; the `role:"tool"` round trip; the relay for
`/v1/models`, `/v1/audio/*` and multipart uploads; `/healthz` on `:8081`
reporting the gateway's health; wrong key → 401 on both hops
(`docs/live-findings.md`, LIVE-TOOLS-001). The middleware stays vendored in
`deploy/toolforge/` — nothing builds or runs it.

### 2.1.3 Retired with the streaming path and the session store (ADR 0017)

The client-facing SSE path and the conversation store were deleted, not switched
off (ADR 0017), so the cases whose subject was that machinery are retired here
rather than silently dropped. The **upstream** SSE parser cases (SSE-001..013) stay
in force: MiMo still answers every chat POST with `event:message` frames, and the
thinking splitter, the usage merge and the fixture parser are load-bearing.

| Retired | What it pinned | Replaced by |
|---|---|---|
| API-002 | the SSE frame sequence of a successful answer | API-001 (the same answer as one JSON body) |
| API-010 (delta form) | reasoning delivered as `reasoning_content` deltas | API-010 rewritten: `reasoning_content` on the message; the thought-off case asserts it is stripped (the non-streaming path used to include it unconditionally — the conversion caught a real leak) |
| API-019 | the SSE keepalive ticker and mid-stream stalls | — (a keepalive has no meaning without a stream) |
| API-021 (frame form) | an upstream mid-body failure delivered as an error-text delta, then `finish_reason:stop`, then `[DONE]` | API-021 rewritten: the same upstream failure surfaces as an HTTP error envelope, never a 200 with a blank or truncated answer |
| DEBUG-001 | the routing trace of a sticky continuation | DEBUG-002 (the full-replay trace every request now logs) |
| STICKY-002..013 | store semantics: keyed continuation match, replay guard, TTL/LRU/sliding expiry, persistence round-trip, frontier verification, fork accounting, reuse verification, the mint-and-store path | NOSTICK-001/002 (asserted at the wire: fresh `conversationId` per request, chaining only inside one request's replay) and DISCONNECT-001 for the released-lease rule |
| SESS-001..062 (`docs/specs/session-identity.md`) | the identity ladder (`X-Mimo-Session-Id` → body → derived key), isolation between sessions, migration, response header, debug line | — (there is no session identity surface: the header, the store and the spec are retired together) |
| NOSTICK-001..004 | "non-streaming never touches the store", proven through the store's counters | NOSTICK-001/002 — the same invariants, now observable only in the upstream payloads, which is where they always mattered |
| CONF-015 | `MIMO2API_ALLOW_STREAM` default and override | — (the variable is gone; `stream:true` is refused unconditionally) |
| CONF-* (sticky/keepalive/fork rows) | `MIMO2API_STICKY_TTL_S`, `_STICKY_MAX`, `_KEEPALIVE_INTERVAL_MS`, `_SESSION_WAIT_MS`, `_SESSION_FORK_POLICY` parsing | — (the variables are gone; `MIMO2API_SESSION_REFRESH_HOURS` stays — that is the credential keepalive, ADR 0014) |
| STREAM-001 | the deployed default was non-streaming, checked with no upstream call | kept, retitled: the refusal is now structural, so the case asserts `400 stream_unsupported` **and** zero upstream calls with no configuration involved |
| LIVE-004 | the live streaming frame contract | — (LIVE-003 covers the live JSON shape) |
| LIVE-005 | live stickiness (a second turn resuming stored state) | LIVE-011 (replay depth: a fact planted at turn 1 is recalled at turn 6 because the history is replayed) |
| LIVE-010 | a conversation surviving the store's GC | — (there is no store to garbage-collect) |
| DISCONNECT-001 (new) | — | replaces STICKY-010: a client that hangs up mid-turn returns the lease, cools nothing, records no failure |

### 2.1 Recorded SSE fixtures (`internal/testutil/testdata/`)

Exact-byte fixtures, committed. The MiMo format uses colon **without** space
(`event:message`, SDD §2.5); fixture files preserve that byte-exactly. The thinking
markers are `<think>` + NUL (`<think>\x00`) and `</think>` + NUL (`</think>\x00`)
— marker followed by a NUL byte (SDD §2.5, PROTOCOL.md:79-84).

| Fixture | Content | Used by |
|---|---|---|
| `sse_basic.txt` | dialogId → 3 `message` text deltas → `usage` → `finish [DONE]` | API-001/002, SSE-001 |
| `sse_crlf.txt` | `sse_basic` with `\r\n` line endings | SSE-002 |
| `sse_nospace.txt` | `event:x` (no space after colon) variants | SSE-003 |
| `sse_thinking.txt` | message deltas embedding `<think>\x00 … </think>\x00` then plain text | SSE-006, API-010 |
| `sse_thinking_split.txt` | same, but marker bytes straddling chunk boundaries (`<thi` + `nk>\x00`) | SSE-007 |
| `sse_thinking_unterminated.txt` | `<think>\x00` never closed; stream ends | SSE-009 |
| `sse_multi_think.txt` | two separate think blocks interleaved with text | SSE-011 |
| `sse_multi_usage.txt` | `usage` emitted 3×, last one has the largest counts | SSE-012, API-004 |
| `sse_error.txt` | `event:error` mid-stream after 2 deltas | API-021, SSE-005 |
| `sse_sensitive.txt` | `event:sensitive_query` | ERRMAP-009 |
| `sse_empty.txt` | dialogId → finish `[DONE]`, zero message deltas | SSE-013 |
| `sse_fastchat.txt` | fastchat-shaped stream with repeated `usage` events | SSE-012, LIVE-009 analogue |
| `sse_stall.txt` | (served with a deliberate mid-stream delay) | API-019, SSE-014 |
| `xssi_login_ok.txt` | `&&&START&&&{...}` serviceLoginAuth2 success body | LOGIN-002/003 |
| `xssi_login_captcha.txt` | `&&&START&&&{...captchaUrl...}` | LOGIN-004 |
| `xssi_login_otp.txt` | `&&&START&&&{...notificationUrl...}` | LOGIN-005 |
| `xssi_sts_ok.txt` | `&&&START&&&{"code":0,"location":"..."}` | RENEW-002 |
| `xssi_sts_70016.txt` | `&&&START&&&{"code":70016,...}` | RENEW-003 |
| `openai_stream_expected.txt` | golden bytes of the OpenAI chunk sequence for `sse_basic` input | API-002/003 |

### 2.2 Shared testutil package

| Helper | Purpose |
|---|---|
| `testutil.FakeClock` | implements the `Clock` interface (`Now()`, `NewTicker`); tests call `Advance(d)`; no real sleeping anywhere |
| `testutil.MockUpstream` | `httptest.Server` builder. `AddAccount(ph, cookie)` registers a scripted account; `Script(ph, script)` queues responses per account (SSE fixture / status code / stall / abort / empty); `Requests(ph)` returns recorded upstream requests (msgId, conversationId, previousDialogueId, modelConfig, query, headers) in order; `URL` for client config |
| `testutil.MockPassport` | `httptest.Server` mocking `account.xiaomi.com` + STS follow: `/pass/serviceLoginAuth2`, login-context 401 w/ `loginUrl`, STS location handler planting `Set-Cookie` |
| `testutil.LoadFixture(t, name)` | reads `testdata/<name>`, fails test with file name on missing/short read |
| `testutil.ParseOpenAISSE(r)` | parses mimo2api's own SSE output into typed chunks (`[]OpenAIChunk` + `[DONE]` marker + raw comment frames) so shape assertions are structural, not string-matching |
| `testutil.NewTestServer(t, opts)` | boots the full `internal/api` handler with mock upstream, N test accounts, short TTLs/timeouts, fake clock wired; returns base URL + handles |
| `testutil.RequireStatus/jsonEq` | assertion helpers producing diffs on failure |

---

## 3. Priority-ordered test list

Priority: **P0** = protocol-correctness, must be green before any integration wiring is
trusted. **P1** = pool/session lifecycle robustness. **P2** = packaging/ops. Within each
group, tests are listed in the order they should be written (which is also dependency
order).

### 3.1 Adapter — thinking splitter, fingerprint, chunk shaping (P0)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| SSE-006 | Basic think-block split | splitter initialized | feed deltas `<think>\x00Let me…</think>\x00Answer` in one chunk | reasoning events carry `Let me…` as `reasoning_content`; `Answer` as `content`; NUL bytes never emitted | §2.5 |
| SSE-007 | Markers split across chunk boundaries | splitter initialized | feed byte splits: `…text<thi` \| `nk>\x00th…` and `…</th` \| `ink>\x00ans…` | marker recognized; no literal `<think>`/NUL leaks into output; state machine survives arbitrary 1-byte splits (property-tested over all split points of a fixture) | §2.5 |
| SSE-008 | NUL stripping outside markers | splitter initialized | content containing stray `\x00` not part of a marker | NUL stripped from emitted content | §2.5 |
| SSE-009 | Unterminated think block | splitter initialized | stream with `<think>\x00…` and no closer, then EOF | all remaining text counts as `reasoning_content` | §2.5 |
| SSE-010 | No markers | splitter initialized | plain text deltas | all `content`, zero `reasoning_content` events | §2.5 |
| SSE-011 | Multiple think blocks | splitter initialized | two `<think>`/`</think>` pairs interleaved with text | output alternates reasoning/content correctly; order preserved | §2.5 |
| ~~STICKY-001~~ | ~~Fingerprint computation~~ **RETIRED** (ADR 0009): the 5-message / 200-rune window is exactly the lossiness the milestone removes. Replaced by `SESS-012` (an edited early turn must force a replay) and by `adapter.FrontierHash`'s own cases. | — | — | — | §4.3.1 |
| API-003 | Exact chunk JSON shape | chunk shaper | shape a delta chunk | JSON keys exactly `{id, object:"chat.completion.chunk", created, model, choices:[{index:0, delta, finish_reason}]}` — no extra keys; one `id` for the whole stream; first chunk `delta` is `{"role":"assistant"}` only | §5.4 |
| API-004 | Usage mapping | usage event(s) | map last `usage` event | `prompt/completion/total_tokens` from last event; `nativeUsage.prompt_tokens_details.cached_tokens` → `prompt_tokens_details.cached_tokens`, `reasoning_tokens` likewise; missing usage → all zeros, never invented | §5.4 |
| API-017 | System-message splice | — | messages: [system A, system B, user U] | first query = `A\n\nB\n\n---\n\nU`; systems never dropped | §5.3 |
| API-013 | Model normalization & routing | — | ids: `mimo-v2.6-pro-studio`, `mimo-v2.6-pro-ultraspeed-studio`, `auto`, unknown `foo` | `-studio` stripped → pro; ultraspeed → fastchat route flag; `auto`/missing → default model; unknown → model-not-found error naming available ids | §2.6, §5.2 |

### 3.2 Session store (P1) — **RETIRED (ADR 0017)**

The whole family below is retired: `internal/session` is deleted. Kept as the
record of what the store promised while it ran; see §2.1.3 for the replacements.


| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| STICKY-002 | Continuation match (keyed) | store holds the entry under session key K with frontier hash H(msgs 1..2), msg_count=2 | `Lookup(K, history)` where the first 2 non-system messages hash to H and the count is 3 | hit; returns `{accountID, conversationID, previousDialogID}` | §4.3.1 |
| STICKY-003 | Replay guard | same entry | lookup with identical history (count still 2) | miss — identical history is regeneration, not continuation | §4.2 |
| ~~STICKY-004~~ | ~~Multiple matches → newest wins~~ **RETIRED** (ADR 0009): with a keyed index there is no ambiguity to resolve. Replaced by "a lookup is scoped to its key" — one session never sees another's entry | two sessions stored | `Lookup` under the other key | miss | §4.3.1 |
| STICKY-006 | TTL expiry | entry with `updated_at = now`, TTL 30 min (fake clock) | `Advance(31m)`, lookup | miss; entry purged by sweeper | §4.3 |
| STICKY-012 | Idle-based (sliding) TTL | entry at age 29 min | `Advance(2m)` after a lookup-refresh, then lookup again | hit — activity slides `updated_at` forward | §4.3 |
| STICKY-007 | LRU-500 eviction | 501 inserts, cap 500 | insert entry #501 | oldest-`updated_at` entry evicted; 500 newest remain; evicted key's lookups miss | §4.3 |
| STICKY-013 | Persistence round-trip | store with entries | save to tmpdir, load into new store | all entries + indexes restored | §4.3 |

### 3.3 Account pool (P1)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| POOL-001 | Round-robin fairness | 3 available accounts A,B,C | 30 sequential acquire/release | each account acquired 10±1 times; strict rotation order visible | §4.2 |
| POOL-002 | Cooldown on 429 | account available | report upstream 429 | state=cooldown, `cooldown_until = now+60s`, excluded from Acquire | §4.5, §4.6 |
| POOL-003 | Backoff ladder 60s→10min | account in cooldown, consecutive 429 counter k | report consecutive 429s (fake clock advances past each expiry) | cooldowns 60s, 120s, 240s, 480s, 600s, 600s… (cap 10 min) | §4.5 |
| POOL-004 | Success resets ladder | account at 4-consecutive-failure ladder state | cooldown expires, probe OK, one successful request, then a 429 | new cooldown is 60s again | §4.5 |
| POOL-005 | 5xx/transport cooldown | account available | report 502 | cooldown from 30s, same doubling ladder | §4.5 |
| POOL-006 | Recovery probe gate | account cooldown expired (fake clock) | selection attempt before probe run / after probe returns `code:0` for `GET /open-apis/user/mi/get` | before probe: still excluded; after OK probe: available | §4.5 |
| POOL-007 | Probe 401 → renew or disable | account cooling, probe returns 401 | probe cycle | exactly one STS renewal attempted; success → available; failure → disabled | §4.5 |
| POOL-008 | All-cooling fail-closed | all 3 accounts in cooldown, nearest `cooldown_until` = now+42s | Acquire | error mapped to 429 `pool_parked` with `Retry-After: 42`; no account is selected | §4.2, §7.4 |
| POOL-009 | Per-account in-flight semaphore | 1 account, `max_inflight=2` | 3rd concurrent Acquire | third acquire does not oversubscribe — waits (see POOL-010) or fails per queue_wait; upstream never sees 3 concurrent for one account | §4.4 |
| POOL-010 | Queue wait timeout → pool_busy | all slots of all accounts busy; `queue_wait` set to 200ms (test override) | Acquire with context | times out → error mapped to 429 `pool_busy` + `Retry-After` present; waiter released cleanly | §4.4, §7.4 |
| POOL-011 | Empty/all-disabled pool | zero accounts / all `disabled` | Acquire | 503 `no_accounts` error | §7.4 |
| POOL-012 | 451/461 handling | account available | report 451 / report 461 | 451 → disabled, reason "account restricted"; 461 → disabled + permanent flag; permanent accounts never probed | §4.1, §4.6 |
| POOL-013 | Cooldown persists across restart | account at 120s cooldown | persist state, construct new pool from saved accounts file | cooldown_until restored; restart cannot bypass limit window | §7.5 |
| POOL-014 | Duplicate ids rejected at load | accounts file with two enabled entries sharing `id` | load | loud error, load fails (not silently joined) | §4.7 |
| POOL-015 | Selection score prefers idle | 2 accounts: A warm (inflight 2/2 history, low EWMA), B idle (0 inflight, higher EWMA) | Acquire | B selected — `(inflight/capacity)*100 + ewma + warm_penalty` favors B within `best*1.15+1` band | §4.4 |
| POOL-016 | Context cancel releases slot | account at max in-flight with a waiting acquire | cancel the holder's context | semaphore slot released; waiter acquires; no leak (repeat 100× under `-race`) | §3, §4.4 |

### 3.4 Non-streaming: no stickiness (P0) — now the only path (ADR 0017)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| NOSTICK-001 | Identical history ≠ same conversation | 2 test accounts, mock upstream recording | two non-streaming requests with byte-identical message history | different accounts used (round-robin) and different upstream `conversationId`s | §4.2 |
| NOSTICK-002 | Fresh conversationId every time | mock upstream recording | 5 non-streaming requests (mixed histories) | 5 distinct 32-hex conversationIds; each valid uuid4-no-dashes format | §4.2, §2.4 |
| NOSTICK-003 | Store untouched | session store instrumented | non-streaming requests | store size unchanged before/after; no lookup performed (assert via store counter) | §4.2 |
| NOSTICK-004 | No cross-request chaining | mock upstream recording | two requests sharing history, each with 2 user messages | both requests' first upstream turn has `previousDialogueId: null`; chaining happens only within a request's own replay | §5.3 |

### 3.5 Streaming stickiness — end-to-end (P0) — **RETIRED (ADR 0017)**

Retired with the streaming path: non-streaming is no longer a policy for one
request shape, it is the only shape there is.


| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| STICKY-011 | Miss path: mint + store | empty store, streaming request (2 user msgs) | request completes | fresh 32-hex conversationId; full chain replay (2 upstream calls, 2nd chained via 1st's dialogId); entry stored with fingerprint + msg_count + previousDialogID | §5.3 |
| STICKY-005 | Hit path: same account + delta-only | one completed streaming turn (store warm) | second request = same history + 1 new user message | same `accountID` and same upstream `conversationId` (assert on mock upstream records); exactly 1 new upstream call carrying only the delta message; `previousDialogueId` == first turn's dialogId; answer references turn-1 context | §4.2, §5.3 |
| STICKY-008 | Pre-first-byte failure → switch | sticky account scripted to 502 before any SSE byte | streaming request on warm store | sticky entry discarded; retry on a different account with fresh conversationId and full replay; client gets a clean 200 stream; failed account cools down | §4.3, §7.2 |
| STICKY-009 | Post-first-byte failure commit boundary | sticky account streams 2 deltas then aborts mid-stream | streaming request on warm store | no account switch, no replay, no restart of deltas; stream terminates with error content delta + `finish_reason:"stop"` + `[DONE]`; stale sticky entry deleted | §4.3, §7.2 |
| STICKY-010 | Client disconnect keeps entry | warm store, stream in progress, dialogId already received | cancel client request mid-stream | upstream request aborted (mock sees ctx cancel); sticky entry kept (context advanced); counted as neither success nor failure; pool slot released | §7.3 |

### 3.5.1 Session identity & isolation (SESS-*, P0) — **RETIRED (ADR 0017)**

Spec: `docs/specs/session-identity.md`; design summary: SDD §4.3.1. These cases
are written before the code (RED by construction — the feature does not exist
yet) and replace the parts of §3.2/§3.4/§3.5 that assume content-derived-only
identity. Every case maps to a spec requirement.

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| SESS-001 | Explicit header wins | store warm under a derived id | request with `X-Mimo-Session-Id: s-abc` extending that history | chains; effective id = `s-abc`; `source=explicit` in the debug line | §4.3.1 |
| SESS-002 | Body field accepted | as above | request with body `session_id: "s-abc"` instead of the header | same as SESS-001 | §4.3.1 |
| SESS-003 | `user` is not an identity source | no header/field | request with `user: "s-abc"` | identity falls through to the derived key; two different `user` values on the same history resolve to the same session; two requests differing only in `user` do not create two sessions | §4.3.1, REQ-2 |
| SESS-004 | Derived key is credential-scoped | two API keys, byte-identical first turn | one request per key | two distinct sessions; two distinct upstream `conversationId`s; no shared entry | §4.3.1, INV-2 |
| SESS-005 | Deterministic precedence | header and body field both present and different | one request | header wins; no error; the losing value is logged at debug only | §4.3.1 |
| SESS-010 | Cross-credential lookup impossible | entry created under key A | key B sends the same history with a forged `X-Mimo-Session-Id` equal to A's | miss → replay path; A's entry untouched (assert store contents + mock upstream records) | §4.3.1, INV-2 |
| SESS-011 | Frontier mismatch ⇒ replay | entry whose stored frontier differs mid-history | request extending a *different* history (same key) | verification fails → replay into a new conversation; session id kept; `decision=replay` | §4.3.1, INV-1 |
| SESS-012 | History rewrite ⇒ replay | warm entry | request with an edited early message and the same tail | miss/replay (today's 5-message window would match — this is the regression that motivates the case) | §4.3.1 |
| SESS-013 | Regeneration guard | warm entry, count n | identical history (count n) | replay; frontier not advanced (TDD STICKY-003 semantics preserved) | §4.2 |
| SESS-020 | Fork policy `fork` | warm session, first request in flight | second concurrent request, `MIMO2API_SESSION_FORK_POLICY=fork` | second request gets a **new** session id + fresh `conversationId`; two upstream chains exist; headers differ (`X-Mimo-Session-Mode: fork`) | §4.3.1, INV-4 |
| SESS-021 | Default wait-then-fork | as above, default policy, `MIMO2API_SESSION_WAIT_MS=200` | second concurrent request while the first is slow | waits, then chains once the first completes (no fork, no interleaved dialogue); with the first held longer than the bound, the second forks instead | §4.3.1 |
| SESS-022 | Original chain intact after a fork | fork happened (SESS-020) | third request on the original session id | chains onto the original frontier, not the fork's | §4.3.1, INV-4 |
| SESS-030 | Non-stream is one-shot | warm session + store instrumented | `stream:false` request extending that history | store reads and writes both zero (counter), fresh `conversationId`, full replay, no `X-Mimo-Session-Id` header in the response; the warm session's frontier is unchanged | §4.3.1, REQ-10 |
| SESS-031 | Session id ignored on a one-shot | as above | `stream:false` with `X-Mimo-Session-Id` (and with `session_id`) pointing at the warm session | request succeeds one-shot; the header is ignored (debug note, no error); the chain is not advanced and not forked | §4.3.1, REQ-10 |
| SESS-040 | Account migration | session pinned to account A, A marked disabled | extending request | new `conversationId` on account B, full replay, frontier reset to the new turn; `decision=migrate`; no chain across accounts | §4.3.1, REQ-11 |
| SESS-050 | Expiry ⇒ miss | entry idle past `MIMO2API_STICKY_TTL_S` (fake clock) | extending request | miss → replay; no error; client sees 200 (STICKY-006 semantics preserved) | §4.3 |
| SESS-051 | LRU eviction | cap reached | insert one more session | oldest-`updated_at` session evicted; its next request replays (STICKY-007 preserved) | §4.3 |
| SESS-052 | Restart is a documented cold start | a live session, then a fresh process (no snapshot) | extending request after the restart | replay on the fresh process (never an error, never a stale chain); a client that re-pins its own `X-Mimo-Session-Id` re-establishes the session and chains from the new frontier | §4.3.1, REQ-14 |
| SESS-053 | Lookup is keyed, not scanned | 500 live sessions | one request, store instrumented with a comparison counter | comparisons bounded by the target frontier, independent of session count (fails a full-scan implementation) | §4.3.1, REQ-12 |
| SESS-060 | One debug line per decision | `MIMO2API_LOG_LEVEL=debug` | one request per decision kind (chain/replay/fork/migrate) | exactly one line each, containing session id, source, decision, frontier and history sizes | §4.3.1, REQ-15 |
| SESS-061 | Effective id returned | any request | inspect response headers | `X-Mimo-Session-Id` present and equal to the id that the next turn chains on (round-trip: pin it and get a chain) | §2.4.1, REQ-16 |
| SESS-062 | No content, no credentials | any of the above | grep every session log line | no message content, no API key, no cookie/passToken substring appears | §4.3.1, INV-3 |

Failing fast is the point of SESS-053/060/062: they fail silently in
operations if left untested, so they are P0 alongside the functional cases.

### 3.5.2 Account pool: attribution, risk-control form, observability, egress (spec `docs/specs/account-pool.md`)

Wave order agreed with the owner: ① attribution → ② risk control → ③ observability → ④ egress.

**① Failure attribution (POOLATTR-*)** — SDD §4.5.1

| ID | Description | Precondition | Stimulus | Expected result | Test |
|---|---|---|---|---|---|
| POOLATTR-001 | A transport failure starts its **own** ladder | account with 3 consecutive `upstream-5xx` failures (5xx ladder at 120s) | `ReportTransport(id)` | cooldown = 15s (transport base), not 120s; transport count 1, 5xx count still 3 | `TestTransportFailureHasOwnLadder` |
| POOLATTR-002 | Counters are per class | fresh account | one of each: 5xx, transport, 429 | each counter 1; no class reached 2 (no cross-class escalation) | `TestFailureCountersArePerClass` |
| POOLATTR-003 | Transport shapes classify as transport | — | stall, `io.ErrUnexpectedEOF`, `ECONNRESET`, `EPIPE`, dial failure, deadline | every one → `ClassTransport`; a 5xx is never transport | `TestClassifyPoolFailure`, `TestIsTransportErrRejectsUpstreamAnswers` |
| POOLATTR-004 | A live stall cools on the transport ladder | SSE idle-stall mid-stream (150ms watchdog) | streaming chat request | transport counter ≥1, **5xx counter 0**, recorded cooldown class `transport` with a timestamp | `TestStallCoolsAccountAsTransport` |
| POOLATTR-005 | The existing ladders are unchanged | fresh account | three `Report429` / three `Report5xx` | 60→120→240 and 30→60→120 (POOL-003/005 keep passing) | `TestExistingLaddersUnchanged` |
| POOLATTR-006 | The transport ladder has its own cap | fresh account | six `ReportTransport` | 15→30→60→120→240→300, capped at 5min (never the shared 10min) | `TestTransportLadderAndCap` |
| POOLATTR-007 | Service-busy stays free | available account, scripted busy frame | chat request | one retry; account still available; no counter touched (BUSY-001/002 unchanged) | `TestUpstreamBusyRetriedThenSucceeds` |
| POOLATTR-008 | Success clears every class | counters set in all three classes | `ReportSuccess` | all counters 0; the next failure starts at that class's base | `TestSuccessClearsEveryClass` |
| POOLATTR-009 | Renewal clears counters, not a live cooldown | account cooling (time-owned) with counters set | `ResetFailures` (the renewal path) | counters 0; `cooldown_until` unchanged to the nanosecond | `TestRenewalResetClearsCountersNotCooldown` |
| POOLATTR-010 | The cooldown records its cause | any cooldown | enter cooldown, then fail again | class + upstream status + timestamp recorded, latest cause wins | `TestCooldownRecordsItsCause` |
| POOLATTR-011 | Upstream `Retry-After` is never read | 429 | `ReportFailure(429)` | cooldown = the ladder's 60s; the pool's API accepts no upstream duration (owner decision, spec §10.2) | `TestLadderIgnoresUpstreamRetryAfter` |
| POOLATTR-012 | 5xx and transport stay apart end to end | one account, scripted 5xx then a stall | two chat requests | 5xx counter 1 with recorded class `upstream-5xx`/500; transport counter untouched by the 5xx | `TestFiveXxAndTransportStayOnSeparateLadders` |
| POOLATTR-013 | Audio calls are attributed too | TTS 5xx; then a task-level ("failed") synthesis | two audio requests | 5xx cools the account (previously an audio 5xx cooled nothing); a business failure cools nothing and stays `available` | `TestAudioFailureAttribution` |

**② Risk control (RISK-*)** — SDD §4.5.2

| ID | Description | Precondition | Stimulus | Expected result | Test |
|---|---|---|---|---|---|
| RISK-001 | 461 is recorded, not just acted on | available account | upstream 461 on chat | disabled (fail closed, no retry) **and** an event recorded with account, egress label, model, channel, status, body digest, timestamp (the digest, never the body) | `TestRiskControlRecordsEvent` (pool), `TestRiskControl461DisablesAndRecords` (e2e) |
| RISK-002 | 461 sets no permanent flag | as above | inspect the account | `permanent == false`; the account is recoverable via `Enable` | `TestRiskControlIsNotPermanent` |
| RISK-003 | Dead passToken is still permanent | account with a dead `pass_token` | request | disabled **with** `permanent == true` (account-scoped evidence exists) | `TestDeadPassTokenStaysPermanent` |
| RISK-004 | Same-egress correlation | two accounts labelled `hk1` | both hit 461 inside `MIMO2API_POOL_STATS_WINDOW_S` | the count for that egress is 2; an account on another egress is not counted; signals older than the window stop correlating but stay recorded | `TestRiskControlCorrelatesByEgress` (pool), `TestRiskControlCorrelatesSameEgressEndToEnd` (e2e) |
| RISK-005 | 451 follows the same path | available account | upstream 451 | disabled, event recorded, no permanent flag | `TestRestricted451FollowsSamePath` |
| RISK-006 | Operator can undo, evidence stays | account disabled by a risk-control event | `Enable(id)` | back to `available`; its recorded events remain (evidence is not erased) | `TestRiskControlIsOperatorRecoverable`, `TestRiskControlOperatorRecoveryKeepsEvidence` |
| RISK-007 | An unknown account still records | pool without that account | `ReportRiskControl("ghost", …)` | the event is recorded on the default egress; no panic; count 1 | `TestRiskControlUnknownAccount` |

**③ Observability (POOLVIEW-*)** — SDD §4.9

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| POOLVIEW-001 | Accounting invariant | mixed pool (available/cooldown/disabled) | read the view | `available + cooldown + disabled == total`; per-account states match the summary counts | INV-1 |
| POOLVIEW-002 | `/healthz` is unchanged | any pool | `GET /healthz` | byte-identical shape to today's four aggregates (the Docker healthcheck parses it) | REQ-13 |
| POOLVIEW-003 | The detail view is authenticated | valid API key configured | `GET /v1/admin/pool` without credentials / with a bad key | 401 both times; with the key → 200 | REQ-13 |
| POOLVIEW-004 | No credential is exposed | account with cookie/ph/password and a labelled egress | serialize the view | `input` scan finds no cookie value, no `ph`, no password, no egress endpoint/credentials | REQ-14, INV-4 |
| POOLVIEW-005 | Ids are masked and stable | two accounts | read twice | masked ids differ per account, are stable across reads, and contain no phone digits | REQ-14 |
| POOLVIEW-006 | Reasons are visible | one cooling account, one disabled account | read the view | cooldown class + end, disabled reason + permanent flag, in-flight/capacity, EWMA latency present | REQ-12, REQ-5 |

**④ Per-account egress (EGRESS-*)** — SDD §4.8

| ID | Description | Precondition | Stimulus | Expected result | Test |
|---|---|---|---|---|---|
| EGRESS-001 | Label syntax is validated | startup | `HK1`, `hk 1`, `hk_1`, 17 chars | rejected with the allowed pattern; `hk1`, `sg-2`, 16 chars accepted | `TestLabelValidation` |
| EGRESS-002 | The env name is derivable from the label | — | `hk1`, `sg-2` | `MIMO2API_EGRESS_HK1`, `MIMO2API_EGRESS_SG_2` | `TestEnvName` |
| EGRESS-003 | A missing endpoint fails loudly and names it | `egress: "hk1"`, no `MIMO2API_EGRESS_HK1` | startup resolution | error names the variable **and** the label and says what to change; no silent fallback to `direct` | `TestResolveMissingEndpointNamesLabel`, `TestStoreLabelWithoutEndpointFailsStartup` |
| EGRESS-004 | Errors never echo an endpoint | one resolvable label, one malformed endpoint carrying credentials | resolve | the failure names `MIMO2API_EGRESS_SG2`; the credential string never appears | `TestResolveErrorsNeverEchoTheEndpoint` |
| EGRESS-005 | Resolution is per label | two labels | `Resolve` | one endpoint each, schemes honoured; no labels ⇒ no requirement | `TestResolvePerLabel` |
| EGRESS-006 | Traffic really leaves through the egress | account labelled `hk1`, a forwarding proxy as its endpoint | chat, then `/v1/models` | both requests (`/open-apis/bot/chat`, `/open-apis/bot/config`) arrived at the proxy | `TestLabelledAccountTrafficLeavesThroughEgress` |
| EGRESS-007 | A label is per account, not global | `hk1` configured, no account labelled | chat request | nothing arrives at the proxy | `TestUnlabelledAccountDoesNotUseALabel` |
| EGRESS-008 | A lease carries its egress | labelled account | `Acquire` | `lease.Egress == "hk1"`; `EgressOf` agrees (the API layer keys the client on this) | `TestLeaseCarriesItsEgress` |
| EGRESS-009 | A malformed label never reaches the pool | `egress: "HK1"` in accounts.json | `accounts.Load` | load fails, naming the entry | `TestStoreRejectsMalformedLabel` |
| EGRESS-010 | Relabelling is reversible | `hk1` → `sg2` → no label | rewrite + reload | each state resolves and behaves the same otherwise | `TestRelabellingIsReversible` |

**Live** — gated by `MIMO2API_LIVE=1`, recorded in `docs/live-findings.md`:

| ID | Description | Expected result |
|---|---|---|
| LIVE-POOL-001 | The diagnostic view against the real account | per-account state matches what the requests actually did (in-flight returns to 0, cooldowns have reasons, no credential in the body) |
| LIVE-POOL-002 | Passive risk-control observation | recorded **if and when** a 451/461 occurs; "no observation yet" is an acceptable result, because inducing one is forbidden (REQ-9) |
| LIVE-EGRESS-001 | One account behind an operator-supplied egress | a chat turn and a TTS synthesis complete through the labelled endpoint; without an egress supplied by the owner this stays unverified and is reported as such |

### 3.5.3 Model redirects (`redirectTo`)

| ID | Description | Precondition | Stimulus | Expected result | Test |
|---|---|---|---|---|---|
| MODELS-014 | A superseded id is not advertised | account catalog carries `redirectTo` rows (live: 18 of 25) | `GET /v1/models` | superseded ids absent, usable ones present, and no row carries `redirect_to` | `TestModelsListOmitsSupersededIDs` |
| MODELS-015 | Asking for a superseded id by name names its successor | as above | `GET /v1/models/mimo-v2-flash` | 200; the id asked for, plus `redirect_to: mimo-v2.6-flash` and the successor's real capabilities | `TestModelByIDResolvesSupersededID` |
| CHAT-REDIR-001 | A superseded chat id reaches the upstream as its successor | warmed map | chat naming `mimo-v2-flash` | the upstream sees `mimo-v2.6-flash`; the response echoes the id the client asked for | `TestChatResolvesSupersededModel` |
| AUDIO-REDIR-001 | The audio surface is where the upstream is strictest | warmed map | `/v1/audio/speech` naming `mimo-v2-tts` | the synthesis carries `modelCode: mimo-v2.5-tts`; the caller gets audio | `TestSpeechResolvesSupersededModel` |
| REFRESH-001 | A retirement between refreshes is learned from the refusal | map deliberately cold; upstream answers the busy frame | chat naming a superseded id | first request carries the asked id, the retry carries the successor learned from the catalog | `TestChatLearnsModelFromBusyRefusal` |
| REFRESH-002 | Same on the audio surface | map cold; first synthesis answers 400 `model: [x] not support` | `/v1/audio/speech` | the retry carries the successor's `modelCode`; the caller gets audio | `TestSpeechLearnsModelFromRefusal` |
| REDIR-UNIT | Chains, cycles and the static floor | — | `ResolveRedirect`, `Usable`, `Merge` | two-hop chains resolve, cycles terminate, a superseded id stays superseded through the floor merge | `internal/adapter/redirect_test.go` |

### 3.5.4 Tool layer (ADR 0012)

| ID | Description | Precondition | Stimulus | Expected result | Test |
|---|---|---|---|---|---|

### 3.6 OpenAI API surface (P0/P1)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| API-001 | Non-stream happy path | mock upstream serves `sse_basic.txt` | POST /v1/chat/completions `stream:false` | 200; body shape: `id` `chatcmpl-<uuid>`, `object:"chat.completion"`, `created`, `model` echoed as requested, `choices[0].message{role:"assistant",content}`, `finish_reason:"stop"`, `usage` populated; `reasoning_content` absent when empty | §5.4 |
| API-002 | Stream happy path (golden) | mock upstream serves `sse_basic.txt` | POST `stream:true` | `Content-Type: text/event-stream`; chunk sequence: role-only delta → content deltas → `delta:{}` + `finish_reason:"stop"` → usage chunk with `choices:[]` → `data: [DONE]`; matches `openai_stream_expected.txt` golden (id/timestamps normalized) | §5.4 |
| API-005 | GET /v1/models | server up | GET /v1/models | OpenAI `list` shape: `object:"list"`, `data[]` with `{id, object:"model", created, owned_by}`; contains the §2.6 static catalog incl. ultraspeed | §5.1, §2.6 |
| API-006 | GET /v1/models/{id} | server up | known id / alias with `-studio` suffix / unknown id | known → 200 detail; alias → 200 after normalization; unknown → 404 error listing available ids | §5.1 |
| API-008 | Tools stripping + warning header | — | request with `tools`, `tool_choice`, `functions` | 200; fields never appear in recorded upstream payload; response carries `X-Mimo-Warnings` naming all dropped fields | §5.2 |
| STREAM-001 | Streaming refused on a non-streaming deployment | server built with `AllowStream=false` (the deployed default) | POST `stream:true`, then the same request with `stream:false` | streaming → `400` with `code:"stream_unsupported"` and the fix (`"stream": false`) in the message, **0 upstream calls**; then non-streaming → `200` with exactly 1 upstream call | §5.7, ADR 0016 |
| API-009 | max_tokens ignored | — | request with `max_tokens` + `max_completion_tokens` | 200; not forwarded; warning header mentions them | §5.2 |
| API-010 | enable_thinking → reasoning deltas | mock upstream serves `sse_thinking.txt` | `stream:true`, `enable_thinking:true` | reasoning deltas emitted as `reasoning_content` **before** content deltas, incrementally (not one fat chunk); with thinking off, reasoning is stripped | §5.2, §5.4 |
| API-011 | reasoning:{type:enabled} alias | same fixture | `reasoning:{"type":"enabled"}` | same behavior as API-010 | §5.2 |
| API-012 | Content parts | — | `content` as `[{type:"text","text":…},…]`; then a part with `type:"image_url"` | text parts joined and accepted; image part → 400 | §5.2 |
| API-014 | No user message | — | messages = [system] only | 400 `invalid_request_error` | §7.4 |
| API-015 | Replay cap | — | 25 user messages, cold store | 400 with guidance (cap = 20 configurable) | §5.3 |
| API-016 | Client-side char caps | — | query of 45,001 chars to ultraspeed model; 90,001 chars to normal model | 400 `context_too_long` both; **zero** upstream requests recorded | §2.7 |
| API-018 | /healthz | pool with mixed states | GET /healthz (no auth) | 200; JSON pool summary with state counts only; no cookies/tokens/phones anywhere in body | §5.1 |
| API-019 | Keepalive comments | mock upstream serving `sse_stall.txt` (silent > keepalive interval, test sets 50ms) | streaming request | client wire receives `: keepalive\n\n` comment frames between upstream silence and the next event; comments never appear inside `data:` payloads | §2.7, §5.4 |
| API-020 | Error before first byte (stream) | all upstream attempts fail pre-byte (e.g. all accounts cooling) | streaming request | plain HTTP error JSON (correct status + §7.4 shape), **not** an SSE body | §7.4 |
| API-021 | Error after first byte (stream) | mock upstream serves `sse_error.txt` after 2 deltas | streaming request | final chunk with `delta.content` containing the error text, then `delta:{}` + `finish_reason:"stop"`, then `[DONE]`; stream never hangs | §7.3, §7.4 |
| API-022 | temperature/top_p passthrough | mock upstream recording | request with `temperature:0.3, top_p:0.9` / without | present inside upstream `modelConfig` when given, absent otherwise | §5.2 |
| API-024 | Client disconnect propagation | mock upstream with a hanging SSE response | cancel client request mid-stream | mock upstream observes request-context cancellation; pool slot released (pool counters return to zero) | §3, §7.3 |
| API-025 | Graceful shutdown drain | server with one in-flight stream | send SIGTERM-equivalent (`srv.Shutdown`) mid-stream | in-flight stream completes to `[DONE]` before listener close; new requests refused | §3 |

### 3.7 Auth middleware (P0)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| AUTHMID-001 | Missing Authorization | API_KEYS=k1 | no header | 401 `{"error":{"message":…,"type":"invalid_request_error","code":"invalid_api_key"}}` | §5.1 |
| AUTHMID-002 | Wrong key | API_KEYS=k1 | `Bearer wrong` | 401 same shape | §5.1 |
| AUTHMID-003 | Valid key | API_KEYS=k1 | `Bearer k1` | request proceeds | §5.1 |
| AUTHMID-004 | Multiple keys | API_KEYS=k1,k2 | both keys | both accepted | §5.1 |
| AUTHMID-005 | Open mode | API_KEYS empty | no header | request proceeds (bind-local contract is config-level, tested in CONF) | §5.1 |
| AUTHMID-006 | All /v1/* protected | keys set | no-header calls to /v1/chat/completions, /v1/models, /v1/models/x | all 401 | §5.1 |
| AUTHMID-007 | /healthz open | keys set | GET /healthz no header | 200 | §5.1 |
| AUTHMID-008 | Non-Bearer scheme | keys set | `Basic xyz` | 401 same shape | §5.1 |

(Constant-time comparison itself is an implementation invariant — verified in code
review to use `crypto/subtle`; not behaviorally assertable.)

### 3.8 Upstream MiMo client (P0/P1)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| SSE-001 | SSE parser basics | fixture `sse_basic.txt` as body reader | parse | events in order: dialogId, message×3, usage, finish; `data` JSON decoded; colon-without-space format handled | §2.5 |
| SSE-002 | CRLF tolerance | fixture `sse_crlf.txt` | parse | identical event sequence as SSE-001 | §2.5 |
| SSE-003 | `event:x` no-space tolerance | fixture `sse_nospace.txt` | parse | identical event sequence | §2.5 |
| SSE-005 | Event dispatch semantics | fixtures: error, sensitive, web_search/doc/tip variants, multi-usage | parse + dispatch | `error` fails turn; `sensitive_query`/`sensitive_title` → content-filter failure; `web_search`/`doc`/`tip_ratio`/`tip_truncate` ignored; `finish` `[DONE]` ends turn; `dialogId` captured for chaining | §2.5 |
| SSE-012 | Usage last-wins | fixture `sse_multi_usage.txt` | parse | final usage = last event's numbers | §2.5 |
| SSE-013 | Empty response → retry | mock upstream: first attempt serves `sse_empty.txt`, second serves `sse_basic.txt`; both recorded | chat call | exactly one retry; retry carries **fresh msgId** and **fresh conversationId**; client receives full answer; no pool health action | §4.6, §7.1 |
| SSE-014 | Idle-stall watchdog | mock upstream stalls mid-stream; `SSE_IDLE_TIMEOUT_S` overridden to 200ms in test | chat call | upstream aborted at ~200ms; error surfaced per §7.3; account cooldown-from-30s applied | §7.3 |
| LOGIN-001 | Login context probe | MockPassport serving 401 + `loginUrl` on `/open-apis/user/mi/get` | begin login | `callback, sid, qs, _sign, serviceParam, _group` parsed from loginUrl | §2.2 |
| LOGIN-002 | Password auth | MockPassport `/pass/serviceLoginAuth2` returns `xssi_login_ok.txt` | login(phone, password) | POST is form-encoded with `sid, callback, qs, _sign, serviceParam, user, hash, _json=true`; `hash == MD5(password).hex().toUpperCase()`; `code:0` + `location` accepted | §2.2 |
| LOGIN-003 | XSSI prefix parsing | bodies prefixed `&&&START&&&` | decode | prefix stripped, JSON parsed; garbage before/after prefix rejected | §2.2 |
| LOGIN-004 | Captcha demand | MockPassport returns `xssi_login_captcha.txt` | login | account → `disabled` with human-readable reason mentioning captcha; no retry loop, no auto-solve | §2.2, §4.1 |
| LOGIN-005 | OTP/new-device demand | MockPassport returns `xssi_login_otp.txt` | login | account → `disabled` with reason mentioning device verification | §2.2, §4.1 |
| LOGIN-006 | STS exchange | MockPassport STS endpoint at `location` plants cookies | follow location | `xiaomichatbot_serviceToken`, `xiaomichatbot_ph`, `userId` captured; derived cookie string assembled | §2.2 |
| LOGIN-007 | Persist after login | successful login | inspect accounts store | `userId`, `passToken`, cookie string, `ph`, `session.fetched_at` persisted | §2.2, §4.7 |
| LOGIN-008 | Location host allowlist | `location` points to `https://evil.example/sts` | follow location | refused — only aistudio / account.xiaomi.com hosts followed (open-redirect guard) | §2.2 |
| LOGIN-009 | Non-zero code, no location | `&&&START&&&{"code":70003,...}` | login | login failure surfaced as account error; no crash, no partial state | §2.2 |
| DEV-001 | Device identity presented on Auth2 | stored `device` (deviceId/pass_ua/uLocale/cUserId) + MockPassport recording request headers | `client.WithDevice(dev).Login(...)` | the `serviceLoginAuth2` POST carries every stored device cookie — without it the passport sees new hardware every login | §2.2 |
| DEV-002 | No device stored → none invented | account without `device` | `client.Login(...)` | Auth2 sends no device cookie (the pre-fix behaviour stays available and tests stay honest) | §2.2 |
| DEV-003 | Device identity harvested from the login | MockPassport plants `deviceId/pass_ua/uLocale` on Auth2 | `Login(...)` | `Session.Device` carries the three planted values and **no** session cookie (`serviceToken`, `ph`, `passToken`) | §2.2 |
| DEV-004 | Verification chain stays on one device | OTP fixture + stored device | `Login(...)` → `ErrOTPPending` | `/identity/list` presents a deviceId and the OTP challenge carries the same one, so the submitted code cannot drift onto another device. Precedence pinned: a deviceId the passport issues in `Set-Cookie` wins over the replayed value (browser semantics) | §2.2 |
| DEV-005 | Renewal presents userId + passToken + device | stored device | `WithDevice(dev).Renew(userID, passToken)` | the `serviceLogin` hop carries all three | §2.3 |
| DEV-006 | Rotated passToken wins | MockPassport STS plants a different passToken | `Renew(...)` | `Session.PassToken` = the rotated value; with no rotation the stored one is kept | §2.3 |
| DEV-007 | Device merge semantics | `MergeDevice(stored, fresh)` | pure call | fresh values win, values the server stayed silent about survive, both-empty → nil (JSON-clean account), the stored map is never aliased | §2.2 |
| DEV-008 | Login page is primed before Auth2 | mock passport records login-page navigations | `client.Login(...)` | exactly one navigation, with Chrome's document headers (`sec-fetch-mode: navigate`, `Upgrade-Insecure-Requests: 1`, HTML Accept), and `serviceLoginAuth2` carries the anchor it issued (`deviceId`, `pass_ua`, `uLocale`, `JSESSIONID`) | §2.2 step 0 |
| DEV-009 | Stored device stays sticky | stored `deviceId`/`uLocale` + page that mints only when none is presented | `WithDevice(stored).Login(...)` | the stored `deviceId=wb_stored-1` is presented on Auth2 and the page's own cookies come along; the client fabricates no identity of its own | §2.2 |
| DEV-010 | Auth2 sends the form-POST header set | login | `Login(...)` | `Content-Type` form, `sec-fetch-mode: navigate`, `sec-fetch-site: same-origin`, `sec-fetch-dest: document`, `Upgrade-Insecure-Requests: 1`, `Origin`/`Referer`/`sec-ch-ua*`/`Accept-Language`/`User-Agent` present, HTML Accept | §2.4 |
| DEV-011 | Identity hops send the SPA XHR set | OTP-demanding login | `Login(...)` | every `/identity/*` call carries `X-Requested-With`, `sec-fetch-mode: cors`, `sec-ch-ua`, JSON Accept | §2.4 |
| DEV-012 | An aborted login still persists its device | api stack, Auth2 answers the OTP demand | `POST /v1/chat/completions` | the identity harvested before the demand is written to the store **and** the account file, so the next attempt presents the same device and the operator's code still applies to it (ADR 0013) | §2.2, §4.7 |
| REC-001 | A revoked credential is recovered in the background | api stack: account with a dead passToken, no session; renewal answers `70016`, Auth2 answers login-OK | `RecoverCredentialAsync` twice | the first call starts one attempt, the second is refused while it is in flight (one IP, two password logins is what risk control reacts to); the rotated passToken + session are persisted and the account is selectable again | §2.3, §7.1 |
| REC-002 | A human gate ends the automatic attempt | Auth2 answers the OTP demand, client built with `DisableOTPAuto` | `RecoverCredentialAsync` | the account is disabled (not permanently) with a reason, the harvested device identity is persisted, **no session** is written and no challenge is started | §2.2, §7.1 |
| REC-003 | A failed attempt backs off | Auth2 answers 5xx | `RecoverCredentialAsync` twice | the second call inside the cooldown is refused, `attempts` stays 1 and the failure is recorded for the operator | §7.1 |
| REC-004 | Dead-credential classification | `IsDeadCredential` on 70016 / captcha / 20024 / transport / protocol errors | pure call | only a credential-side failure routes the account to the re-login; transport and protocol errors stay on the sweep cadence | §2.3 |
| REC-005 | The discovering request does not block on a login | account whose renewal reports `70016` | `creds(ctx, id)` | returns `ErrAccountAction` telling the caller to retry, and schedules the recovery instead of logging in inline | §7.1 |
| DEVPL-001 | Device identity is persisted end to end | api stack: account with `device`, no session; Auth2 fixture = login OK | `POST /v1/chat/completions` | the stored device reaches Auth2; the harvested identity is written to the store **and to the account file** (`"device"` block), so a restart keeps the device | §2.2, §4.7 |
| OTP-001 | code=0 OTP demand (live shape) | MockPassport auth2 returns live-captured `xssi_login_otp_zero.txt` (code=0, securityStatus=128, empty location, notificationUrl) | login | `ErrOTPPending` with auto-started challenge (id, method=Phone, masked notify); identity/list → verifyPhone → sendPhoneTicket all hit | §2.2 amend |
| OTP-002 | identity/list flag=8 → Email | MockPassport identity flag 8 | login | method=Email; verifyEmail/sendEmailTicket hit | §2.2 amend |
| OTP-003 | data-wrapped flag | identity/list returns `{"data":{"flag":8,...}}` | login | data merged; method=Email; notify from data | §2.2 amend |
| OTP-004 | send-ticket header | identity calls recorded with headers | login | send{Method}Ticket is POST with form `retry=0 icode= _json=true` AND `X-Requested-With: XMLHttpRequest` (mandatory — email send 66108s without it) | §2.2 amend |
| OTP-005 | verify-submit form | identity calls recorded | submit(code) | verify{Method} POST form `_flag, ticket=<code>, trust=true, _json=true` + X-Requested-With | §2.2 amend |
| OTP-006 | wrong code | passport verify answers `{"code":70002}` | submit(wrong) | error names the passport code; challenge retained (retry works) | §2.2 amend |
| OTP-007 | successful full flow | OTP-001 env | login + submit | SubmitOTP returns full Session (userId, passToken, STS cookie, ph) | §2.2 amend |
| OTP-008 | challenge expiry | challenge ExpiresAt forced past | submit | refused with "expired" | §2.2 amend |
| OTP-009 | OTP auto disabled | `DisableOTPAuto` + otp_zero fixture | login | `ErrAccountAction` (manual re-login); zero identity calls | §2.2 amend |
| OTP-010 | notificationUrl host guard | notificationUrl → evil host | start | refused (allowlist mention) | §2.2 amend |
| UC-001 | cross-region consent chain | MockPassport consent chain (`/pass2/redirect` → `userinfo/start?bizType=UserCross` → `/fe/service/userCross`) | login | chain walked by hand; `POST /pass2/userCrossAgreement/update` with `userId,status=1,needClose=false` + X-Requested-With; `sid`/`context` taken from the consent `callback`; identity/list uses the NEW context (not the stale one) | §2.2 amend 2026-10-09 |
| UC-002 | consent auto-agree disabled | `DisableCrossRegionAgree` + consent chain | login | `ErrAccountAction` (cause `ErrCrossRegionConsent`); zero consent POSTs, zero identity calls | §2.2 amend |
| UC-003 | consent refused (21301) | consent POST → `{"code":21301,"desc":"认证失败"}` | login | error names code=21301; zero identity calls | §2.2 amend |
| UC-004 | consent answers without location | consent POST → `{"code":0}` | login | error "no location"; zero identity calls | §2.2 amend |
| UC-005 | chain host guard | notificationUrl → non-allowlisted host | login | refused (allowlist mention), zero requests to that host | §2.2 amend |
| UC-006 | `authStartParams` indirection | `/pass2/userinfo/check?callback=<enc authStart?sid=…&context=…>` | unit | sid/context extracted through one level of `callback=` indirection | §2.2 amend |
| UC-007 | `isConsentChain` | hop locations with/without `bizType=UserCross` | unit | consent detected by hop Location, not by the final page | §2.2 amend |
| OTP-CHAIN-001 | landing chain walked to the end | MockPassport live-shaped landing chain (`/identity/result/check` → 302 → `/pass/serviceLoginAuth2/end` → 302 → `/sts`) | login + submit | zero `/pass/serviceLogin` calls (no fallback); cookie carries `cUserId`, `userId`, both serviceToken spellings, quoted `slh`/`ph`; ph unquoted in Session.PH | §2.2 amend 2026-10-09 |
| OTP-011 | empty code | submit("   ") | submit | rejected without hitting passport | §2.2 amend |
| ADOTP-001 | admin full flow | full stack, no cached session, auth2 → otp_zero | chat → pending → wrong code → correct code → chat | account disabled while pending; pending lists id/method/notify/account; wrong code 400 `otp_rejected` retained; correct code 200, session persisted, account available; follow-up chat 200 | §2.2 amend, §5.1 |
| ADOTP-002 | submit validation | unknown id / malformed / empty body | submit | 404 / 400 / 400 | §2.2 amend |
| ADOTP-003 | pending auth | no key / wrong key | pending | 401 | §2.2 amend |
| ADOTP-004 | expiry sweep | challenge expired behind the store | pending | vanished from list | §2.2 amend |
| RENEW-001 | Renewal probe | session cached, MockPassport 401 w/ loginUrl | validate session | 401 body's `loginUrl` extracted for renewal | §2.3 |
| RENEW-002 | STS from passToken | MockPassport returns `xssi_sts_ok.txt` for `loginUrl&_json=true` | renew | GET sent with `Cookie: userId=<id>; passToken=<rt>` (+ the stored device identity, DEV-005); `location` followed; fresh cookies cached; a passToken the chain plants wins, a silent chain keeps the stored one (DEV-006/006b) | §2.3 |
| RENEW-003 | passToken dead (70016) | MockPassport returns `xssi_sts_70016.txt` | renew | account → `disabled` (needs re-login); no infinite retry | §2.3, §4.1 |
| RENEW-005 | Transparent 401 refresh-retry | mock upstream: bot/chat → 401, then (after renewal) 200 SSE | streaming request via full stack | client sees clean 200 stream; exactly one refresh + one retry; refresh occurs only pre-first-byte | §2.3, §7.1 |
| RENEW-006 | Refresh failure → disabled + mapped error | bot/chat → 401; renewal also fails (70016) | request | account disabled; client receives 401 `upstream_auth_expired` (ERRMAP-007 shape) | §7.1, §7.4 |
| RENEW-007 | 6h sweep refreshes only invalid | fake clock; 2 accounts: one valid session, one invalid | advance ticker 6h | invalid account renewed; valid account's passport **not** contacted (assert zero renewal calls for it) | §2.3 |
| RENEW-008 | Mid-stream 401 = commit boundary | upstream 401s after deltas delivered | streaming request | no refresh, no retry post-first-byte; error frame per API-021 | §7.2 |
| RENEW-009 | Session validation business-code check | mock returns HTTP 200 with body `{"code":<nonzero>}` | validate | treated as invalid (200 ≠ healthy; business `code==0` required) | §2.3 |

### 3.9 Error mapping — every §7.4 row (P0, table-driven)

Driven from a single table in `errors_test.go`: `(condition) → (HTTP, type, code, headers)`.
Rows assert both status/shape and the triggering path.

| ID | Condition | Expected |
|---|---|---|
| ERRMAP-001 | Bad API key | 401 / `invalid_request_error` / `invalid_api_key` |
| ERRMAP-002 | Malformed request / no user message | 400 / `invalid_request_error` / no code |
| ERRMAP-003 | All accounts cooldown/parked | 429 / `rate_limit_error` / `pool_parked` + `Retry-After` = ceil(nearest recovery − now) |
| ERRMAP-004 | All busy, queue wait exceeded | 429 / `rate_limit_error` / `pool_busy` + `Retry-After` |
| ERRMAP-005 | All disabled / empty pool | 503 / `api_error` / `no_accounts` |
| ERRMAP-006 | Upstream 429 surfaced | 429 / `rate_limit_error` / `upstream_rate_limited` |
| ERRMAP-007 | Upstream 401 after refresh retry | 401 / `invalid_request_error` / `upstream_auth_expired` |
| ERRMAP-008 | Upstream 5xx / transport | 502 / `api_error` / `upstream_failure` |
| ERRMAP-009 | Content filter (`sensitive_query`) | 400 / `invalid_request_error` / `content_filter` |
| ERRMAP-010 | Query exceeds channel char cap | 400 / `invalid_request_error` / `context_too_long` (cross-ref API-016) |
| TOOLONG-001 | The platform's mid-stream too-long refusal becomes a client error | `MapError` with `ErrUpstreamStream{"Sorry, the text you sent is too long!…"}` | 400 `context_too_long` whose message tells the caller to send fewer or shorter messages — not a 502 carrying English prose |
| TOOLONG-002 | The same refusal arriving as an HTTP error body | `MapError` with `UpstreamError{502, "query is too long"}` | 400 `context_too_long` |
| TOOLONG-003 | An unrelated upstream failure keeps its meaning | `MapError` with a busy-upstream stream error | stays 502, no `context_too_long` (the translation must not swallow real upstream trouble) |
| TOOLONG-004 | The old 90,000 guess no longer rejects served work | 95,000-char single message on `mimo-v2.6-pro` | 200 with exactly one upstream hit (measured served by the platform) |
| TOOLONG-005 | The fastchat cap is real and stays local | 50,001 chars on `mimo-v2.6-pro-ultraspeed-studio` | 400, zero upstream hits (cross-ref ERRMAP-010) |
| TOOLONG-006 | Operator override wins | `TestServerOptions{MaxQueryChars: 1234}` + a 2,000-char message | 400, zero upstream hits |
| TOOLONG-007 | A too-long refusal is not an account fault | scripted too-long error frame mid-stream | 400 and the account stays `available` (no cooldown) |
| ERRMAP-011 | Retry-After privacy invariant | across all 429 rows: `Retry-After` reflects only local recovery deadlines; response bodies/headers never contain phone/userId/cookie/passToken |
| ERRMAP-012 | Streaming post-first-byte error shape | error-text content delta → `delta:{}` + `finish_reason:"stop"` → `[DONE]` (never a bare hang, never restarted deltas) |

### 3.10 Accounts store & config (P1)

**Backends (ADR 0018).** The store is a thin in-memory set over a `Backend`
(`internal/accounts/backend.go`); Redis is one implementation of it.

| # | Case | Precondition | Stimulus | Expectation | SDD § |
|---|---|---|---|---|---|
| STORE-001 | File backend: missing file is an empty set | path under a directory that does not exist | `FileBackend.Load` | no error, zero accounts, nothing created on disk | §4.7 |
| STORE-002 | File backend: invalid JSON names the file | readable file with `{oops` | `Load` | error naming the path (ACC-007 rule kept) | §4.7 |
| STORE-003 | File backend: save creates the directory, mode 0600, round-trips | empty store, nested path | `ReplaceAll` then a fresh `Load` | directory created on demand, file 0600, record identical | §4.7 |
| STORE-004 | A no-op update creates nothing | loaded file backend | `Update` of an unknown id | no error, no file written | §4.7 |
| STORE-005 | Redis backend: round trip + upsert | fake RESP server | `SaveAccount` ×2 (one replaced), `Load` | both records returned with every field, no duplicate row | §4.7 |
| STORE-006 | Redis backend: bad rows never brick the pool | index with an unparseable key and a key with no data | `Load` | the good rows are returned, bad ones skipped with a log | §4.7 |
| STORE-007 | Redis backend: a broken pipe heals | live connection closed under the store | `SaveAccount` again | one re-dial, command succeeds | §4.7 |
| STORE-008 | Redis backend: auth failure is fatal | fake server requiring a password | `OpenUpstash` with the wrong one | error at Open, no store | §4.7 |
| STORE-009 | Plaintext Redis off-loopback is refused | `DS_REDIS_INSECURE` unset | `NewRedisBackend(redis://10.1.2.3:6379)` | refused, message names encryption | §4.7 |
| STORE-010 | Configured Redis is the source of truth | `DS_REDIS_HOST` set, server unreachable | startup | fail fast — never a silent file fallback | §4.7 |
| STORE-011 | The store loads the backend once and writes through | counting backend | 3 reads, then `Update` | exactly 1 load (startup), 1 save (the mutation) | §4.7 |
| STORE-012 | A failed backend write keeps the live value | backend whose save fails | `Update` | error reported **and** the in-memory value is the new one | §4.7 |
| CONF-016 | Port resolution | — | `MIMO2API_PORT` / `PORT` / neither / malformed | explicit wins, then the platform's `PORT`, then 3000; a malformed value errors naming its variable | §6 |
| CONF-017 | Upstash pair is read unprefixed | — | `DS_REDIS_HOST`/`DS_REDIS_TOKEN` set/unset | present → both returned; absent → file backend | §4.7 |
| CONF-018 | Accounts path defaults to a relative path | — | nothing set / explicit path | `data/accounts.json`, overridden by `MIMO2API_ACCOUNTS_FILE` | §4.7 |
| CONF-019 | Accessors mirror the parsed config | full env | `Load` | every accessor answers its parsed value (a table, so a new knob with no case shows up here) | §6 |

| ID | Description | Expected | SDD § |
|---|---|---|---|
| ACC-001 | Load round-trip | save → load → deep-equal accounts (incl. runtime state fields) | §4.7 |
| ACC-002 | Atomic write | file always complete after save (write-temp + rename + fsync); no partial JSON observable even on simulated crash between writes | §4.7 |
| ACC-003 | Permissions | file mode 0600 enforced on create and on re-save of a pre-existing 0644 file | §4.7 |
| ACC-004 | Hot reload | modify file while running → pool picks up new accounts / removals without restart | §4.7 |
| ACC-005 | Duplicate ids | rejected loudly at load (pairs with POOL-014 at pool level) | §4.7 |
| ACC-006 | Disabled duplicates logged, load continues | disabled duplicate entries logged, earlier entry superseded, load succeeds | §4.7 |
| ACC-007 | Corrupt file | invalid JSON → startup error naming the file; no silent empty pool | §4.7 |
| CONF-001 | Defaults | unset env → all §6 defaults (port 8080, TTL 1800s, sticky max 500, inflight 2, queue wait 15s, keepalive 15000ms, timezone Asia/Shanghai…) | §6 |
| CONF-002 | Env overrides | every `MIMO2API_*` var parsed and applied | §6 |
| CONF-003 | Empty API_KEYS → local bind | `MIMO2API_HOST` forced to 127.0.0.1 | §5.1, §6 |
| CONF-004 | API_KEYS parsing | comma-separated, whitespace-trimmed, empty entries dropped | §5.1 |
| CONF-005 | Invalid values | non-numeric port/timeouts → startup error with var name | §6 |
| CONF-006 | Immutable after parse | config struct has no exported mutators; unexported fields + constructor only (compile-level guarantee) | §3 |

### 3.11 Docker & delivery (P2)

| ID | Description | Precondition | Stimulus | Expected result | SDD § |
|---|---|---|---|---|---|
| DOCKER-001 | Image builds | docker available (else skip) | `docker build` | exit 0; multi-stage build; final image is distroless-nonroot; binary static (`CGO_ENABLED=0` in build log) | §6 |
| DOCKER-002 | healthcheck subcommand | server running on 8080 / server stopped | `mimo2api healthcheck` | running → exit 0; stopped → exit 1; works with zero runtime deps (no curl/wget in image) | §6 |
| DOCKER-003 | Accounts volume mount | `./data/accounts.json` with one test-shaped account (fake creds) | `docker run -v ./data:/data` | container reads `/data/accounts.json`; after a scripted session-refresh, file on host updated with runtime state, mode 0600 | §4.7, §6 |
| DOCKER-004 | Non-root runtime | image built | `docker run … id` / inspect `User` | runs as nonroot (distroless `nonroot` = uid 65532, observed via `docker top`); `/data` writable via volume | §6 |
| DOCKER-005 | compose config valid | docker compose available | `docker compose config` | exit 0; healthcheck block, volume mount, env vars as per §6 compose file | §6 |
| DOCKER-006 | Container serves | image built | run + `GET /healthz` on mapped port | 200 within start_period; healthcheck transitions to healthy | §6 |

### 3.12 Live verification (P2, gated)

See §5 of this document for the full protocol. IDs LIVE-001..010 map 1:1 to SDD §8.2
steps 1–6 and Appendix B Q1–Q4.

---

## 4. TDD implementation order (red-green sequence)

Strict order — each wave's tests are written **first** (red), then implemented until
green, then the next wave starts. Waves are ordered so no test ever needs a component
that a later wave builds.

### Wave 1 — `internal/adapter` (pure, zero I/O) — unlocks everything

Write, in order: SSE-006 → SSE-007 → SSE-008/009/010/011 (splitter) → STICKY-001
(fingerprint) → API-003/004 (chunk shaper + usage mapping) → API-017 (splice) →
API-013 (model normalization).

- Red: tests reference `adapter.NewThinkSplitter()`, `adapter.Fingerprint(msgs)`,
  `adapter.ShapeDelta(...)`, `adapter.MapUsage(...)` — none exist yet.
- Green: implement pure functions until the group passes.
- Why first: the splitter and fingerprint are depended on by session (wave 2), mimo
  (wave 5), and api (wave 6). Everything downstream compiles against these signatures.

### Wave 2 — `internal/session` — **RETIRED (ADR 0017)** (was: needs adapter.Fingerprint + testutil.FakeClock)

Build `testutil.FakeClock` here (first consumer). Write: STICKY-002 → 003 → 006 → 012
→ 007 → 013 (the keyed store, its frontier verification and lifecycle). SESS-011/012/053
arrive in wave 2.5 as the verification and cost cases.

- Green unlocks: pool cooldown timing (wave 4 reuses FakeClock), api sticky wiring
  (wave 6), and pins the replay-guard semantics the whole stickiness contract rests on.

### Wave 2.5 — session identity & isolation — **RETIRED (ADR 0017)** (needed wave 2 green)

The milestone that replaces content-derived-only identity (spec
`docs/specs/session-identity.md`, SDD §4.3.1). Order inside the wave, because
each step changes the previous step's assumptions:

1. identity ladder in `internal/session/identity.go` — SESS-004 → 001 → 002 → 003
   → 005 (ladder + credential scoping + precedence);
2. frontier verification replacing the 5-message window — SESS-011 → 012 → 013
   (replay instead of a lookalike chain), then re-run STICKY-002/003 to prove the
   hit/miss contract survives the change of identity source;
3. keyed index replacing the full scan — SESS-053, then confirm a lookup never sees
   another key's entry (STICKY-004's replacement);
4. fork policy — SESS-020 → 021 → 022;
5. one-shot non-streaming — SESS-030 → 031, then re-run NOSTICK-001..004 (they
   must stay green: this is the contract that does *not* change);
6. migration, lifecycle, observability — SESS-040 → 050 → 051 → 052 → 060 → 061
   → 062.

- Green unlocks: nothing upstream (it is a self-contained milestone), but it
  gates the milestone's live evidence: one real chained session with a measured
  delta-versus-replay latency, plus one forced migration.
- Ordering rationale: 1–3 change *which* session a request resolves to, so the
  fork, non-stream and observability cases are only meaningful afterwards; each
  step's cases must be RED before its code exists, and the STICKY/NOSTICK
  re-runs are the guard that this wave does not silently change deployed
  behaviour.

### Wave 2.6 — account pool: attribution → risk control → observability → egress (spec `docs/specs/account-pool.md`)

Order is the owner's. 1) POOLATTR-001..010 (per-class ladders; needs `pool` + the api
classification layer); 2) RISK-001..006 (recording and correlation; needs ③'s view for
RISK-004, so its view assertion lands with wave 2.7); 3) POOLVIEW-001..006 (the admin
endpoint and the masking/no-credential scan); 4) EGRESS-001..006 (label validation and
per-account dialling). Each wave must leave `make fmt vet test-race cover-check hygiene`
green before the next starts.

### Wave 3 — `internal/config` + `internal/accounts` (no deps)

Write CONF-001..006 and ACC-001..007. Fast, low-risk; needed by pool (accounts
loading) and every integration test (config overrides for short TTLs/timeouts).

### Wave 4 — `internal/pool` (needs accounts + FakeClock; probes injected as fakes)

Write: POOL-001 → 002 → 003 → 004 → 005 → 006 → 007 → 009 → 010 → 008 → 011 → 012 →
013 → 014 → 015 → 016.

- Probe outcomes are injected via an interface (`ProbeFunc`), so no mock upstream is
  needed yet; the real probe (mimo client) arrives in wave 5 and is wired by tests in
  wave 6.
- Green unlocks: ERRMAP-003/004/005 (pool_parked/busy/no_accounts need a working
  pool), and the retry plumbing in api.

### Wave 5 — `internal/mimo` (needs adapter splitter + testutil.MockUpstream/MockPassport)

Build `testutil.MockUpstream` + `MockPassport` here (first consumers). Write in order:

1. SSE-001 → 002 → 003 → 005 → 012 (parser over fixtures).
2. LOGIN-001 → 002 → 003 → 004 → 005 → 006 → 007 → 008 → 009 (MockPassport).
3. RENEW-001 → 002 → 003 → 007 → 009 (renewal core).
4. client payload/header tests (msgId 32-hex, multiMedias `[]` never null, cookie
   string shape, `?xiaomichatbot_ph=` query param, fastchat routing for ultraspeed).
5. SSE-013 (empty retry) → SSE-014 (stall watchdog) → RENEW-005/006/008 (401 refresh
   paths against MockUpstream).

- Green unlocks: the full api stack (wave 6) and the LIVE suite's mock-side analogues.

### Wave 6 — `internal/api` (needs everything above; full stack via `testutil.NewTestServer`)

Write in order:

1. AUTHMID-001..008 (middleware alone).
2. API-005/006/018 (models, healthz — no upstream involvement).
3. API-001 → API-002/003 (golden stream) → API-004 → API-019 (keepalive).
4. NOSTICK-001 → 002 → 003 → 004.
5. STICKY-011 → 005 → 008 → 009 → 010 (end-to-end stickiness).
6. API-008/009/010/011/012/014/015/016/020/021/022/024/025.
7. ERRMAP-001..012 as the closing table — by this point every triggering path exists;
   the table is the final conformance sweep over §7.4.

### Wave 7 — Docker (needs a green wave 6)

DOCKER-001 → 002 → 003 → 004 → 005 → 006. Shell-driven via Makefile target
`make test-docker`; each test skips (not fails) when the docker binary is absent.

### Wave 8 — Live protocol (needs green waves 1–7 to be meaningful)

LIVE-001 → 002 → 003 → 004 → 005 → 006 (§8.2 steps), then LIVE-007..010 (Appendix B
questions — investigation tests; their outcome may amend the SDD).

### Unlock graph

```
W1 adapter ──► W2 session ──┐
      │                      ├─► W4 pool ──► W6 api ──► W7 docker ──► W8 live
      ├─► W3 config/accounts ┘      ▲            ▲
      └─► W5 mimo client ───────────┘────────────┘
```

---

## 5. Live verification protocol

Package `internal/live` — never part of the default suite (separate package + env
gate). Runbook:

```bash
# one-time: place the designated test account as the first entry in data/accounts.json
MIMO2API_LIVE=1 go test ./internal/live -v -count=1 -timeout 30m
```

Rules (SDD §8.2(3)): sequential, no `t.Parallel()`, one pass per run, ≥2s sleep
between upstream calls, credentials only from `data/accounts.json` (0600, git-ignored).

### 5.1 SDD §8.2 six steps

| ID | Step | Procedure | Pass criteria | SDD § |
|---|---|---|---|---|
| LIVE-001 | Cold-start login | empty session cache for the account; start server; first request triggers passport login (§2.2) → STS cookies | login succeeds without captcha/OTP; `data/accounts.json` updated with `userId`, `pass_token`, session cookie, `ph`; `/healthz` shows account `available` | §8.2.1 |
| LIVE-002 | Model catalog | `GET /v1/models` | 200; catalog contains `mimo-v2.6-pro` and the ultraspeed model; shape per API-005 | §8.2.2 |
| LIVE-003 | Non-streaming echo | `POST /v1/chat/completions` (stream:false), messages: [user: "reply with exactly: pong"] | 200; `choices[0].message.content` contains `pong`; `usage.total_tokens > 0`; `finish_reason == "stop"` | §8.2.3 |
| LIVE-004 | Streaming | same prompt, `stream:true` | ≥2 content deltas; first chunk role-only; terminal `finish_reason:"stop"`; usage chunk with `choices:[]`; `data: [DONE]` last frame | §8.2.4 |
| LIVE-005 | Sticky two-turn | turn 1: "my favorite number is 42, remember it"; turn 2 (streaming, history extends turn 1): "what is my favorite number?" | turn 2 logs a session-store hit; exactly **one** upstream call for turn 2 (delta path — assert via server log, single msgId); answer references 42; same account + conversationId as turn 1 | §8.2.5 |
| LIVE-006 | Transparent refresh | clear cached session (edit accounts.json session → null, or delete cache); issue one request | client gets 200; server log shows exactly one STS refresh + one retry; account remains `available` | §8.2.6 |

### 5.2 Appendix B open questions

Each is a probe test whose finding may amend the SDD; on unexpected results, record
the raw wire data to `internal/live/testdata/findings/` and fail with a readable diff.

| ID | Question | Procedure | Resolution criteria | SDD § |
|---|---|---|---|---|
| LIVE-007 | Does aistudio 429 carry a structured retry deadline? | deliberately exceed a single account's rate limit (sequential bursts); capture 429 status + body + headers | if body JSON contains a deadline field → document it; SDD §4.5 switches to upstream deadline + 1h grace (simple-chat model); else fixed ladder stands | App. B Q1 |
| LIVE-008 | Passport captcha probability for plain form posts from this IP | run LIVE-001 cold login 5× over the run; count captcha responses | 0/5 → plain variant ships; ≥2/5 → implement EUI-encrypted variant (§2.2) before shipping login | App. B Q2 |
| LIVE-009 | Fastchat multiple usage events — last-wins? | streaming completion on the ultraspeed model; capture full SSE | assert last `usage` event's totals ≥ all earlier ones and match token reality; if a different merge rule appears, document + fix SSE-012 analogue | App. B Q3 |
| LIVE-010 | Do sticky conversations survive upstream GC without `/conversation/save`? | create a sticky conversation; wait 24h (manual two-phase: `MIMO2API_LIVE_PHASE=2` resumes with a saved store snapshot); send a delta turn | delta turn still answers with context → auto-save suffices; `event:error` / lost-context → SDD §4.3 amended to call `POST /open-apis/chat/conversation/save` on store | App. B Q4 |

LIVE-010 is two-phase by construction (24h gap); phase 1 saves the store snapshot to
`/tmp/mimo2api-live-store.json`, phase 2 loads it. It is skipped entirely unless
`MIMO2API_LIVE=1` and the snapshot/phase flag is set.

---

## 6. Acceptance criteria

The project is test-complete when **all** of the following hold:

1. **Mock suite green:** `go test -race ./...` passes with zero skips attributable to
   missing implementation (env-gated LIVE-* and docker-absent DOCKER-* skips are
   expected and explicitly allowed).
2. **Coverage targets** (measured `go test -coverprofile=cover.out ./...` +
   `go tool cover -func`):

   | Package | Minimum |
   |---|---|
   | `internal/adapter` | ≥ 90% |
   | `internal/pool` | ≥ 85% |
   | `internal/session` | ≥ 90% |
   | `internal/mimo` | ≥ 80% |
   | `internal/api` | ≥ 70% |
   | `internal/accounts`, `internal/config` | ≥ 80% |

   Coverage is a floor, not a target: untested branches must be individually justified
   in review, not padded.
3. **Error-mapping conformance:** ERRMAP-001..012 all green — every §7.4 row verified
   end-to-end, including the Retry-After privacy invariant (ERRMAP-011).
4. **Session contract:** STICKY-002/003/005..013 + NOSTICK-001..004 + SESS-* green — delta-only
   sends on hit, replay guard, TTL/LRU behavior, commit boundary, and the
   non-streaming no-stickiness guarantee all pinned.
5. **Live suite:** `MIMO2API_LIVE=1 go test ./internal/live -v` passes all of
   LIVE-001..006 with the designated test account; LIVE-007..010 findings recorded
   (each either confirms SDD behavior or produces a documented SDD amendment).
6. **Audio limits:** TTSLIMIT-001..005 green — input over the upstream's 1024
   character cap is refused (400 `input_too_long`, no upstream task created), a
   request at the cap still costs exactly one synthesis, a raised
   `MIMO2API_SPEECH_MAX_CHARS` cannot take effect, and a failed synthesis never
   returns audio (ADR 0010).
7. **Docker:** DOCKER-001..006 pass on a docker-equipped host — image builds as
   distroless-nonroot, `mimo2api healthcheck` subcommand exits 0/1 correctly, the
   `/data` volume round-trips `accounts.json` with runtime state at mode 0600, and
   the compose service reaches `healthy`.

> **Status (2026-10-09).** 1: `make test-race` green over 8 packages.
> 2: floors met — adapter 94.5%, pool 95.5%, session 92.3%, mimo 83.4%,
> api 84.7%, accounts 84.8%, config 90.7% (`make cover-check`).
> 5: `internal/live` exists and runs — LIVE-002..006 pass against the real
> upstream; LIVE-001 and LIVE-008 are env-gated on operator-supplied OTP
> codes (the suite refuses to dispatch an SMS on its own); LIVE-007/009/010
> findings are recorded in `docs/live-findings.md`, together with which
> Appendix B questions remain open and why.
> 6: `make test-docker` → DOCKER-001..006 PASS=6 FAIL=0 (distroless
> nonroot, `mimo2api healthcheck` 0/1, `/data` round-trip at mode 0600,
> compose reaches `healthy`).
> 7: `make hygiene` green.

7. **No real credentials in anything mock-side:** grep of the repo (excluding
   git-ignored `data/`) finds no phone number, password, passToken, or serviceToken
   from §8 — enforced by a repo-hygiene check in the test wrapper.


## 8. ADR 0019 — leases, budgets and slot policy (TIMEOUT/SLOT/LEASE-TTL)

The lease is per-request capacity (never stickiness) and each request now has a real
client-facing budget, a per-turn deadline and an explicit answer to "when does the slot
go back?". Cases live beside their subject: `internal/api/lease_test.go` (external
`api_test` package, like the other end-to-end cases), `internal/pool/lease_ttl_test.go`,
`internal/config/config_test.go`.

| Case | Precondition | Stimulus | Observable expectation |
| --- | --- | --- | --- |
| TIMEOUT-001 | One account; upstream stalls before its first byte | `RequestTimeout=1s`, `LeaseTTL=30s`, stall 4s | `504 api_error chat_timeout`, answered in <3s, message names `1s` and `MIMO2API_REQUEST_TIMEOUT_MS`; account still `available`; `InFlight()==0` |
| TIMEOUT-002 | Same, but the turn TTL is the smaller budget | `RequestTimeout=30s`, `LeaseTTL=1s`, stall 4s | `504 chat_timeout` naming `1s` and `LEASE_TTL` — the message reports the budget that actually bit |
| SLOT-001 | `MaxInflight=1`, `QueueWait=300ms`, `SlotHold=first-byte` | Two concurrent chats; the first has its first byte then stalls 900ms mid-generation | The second is served `200` within 700ms (the slot came back at the first byte), and the first still ends `200` with its own answer; `InFlight()==0` afterwards |
| SLOT-002 | Identical setup, default `last-byte` | Same two requests | The second answers `429 pool_busy` while the only slot is busy — the conservative default is asserted, not assumed |
| LEASE-TTL-001 | `SlotTTL=40ms`, one account | Hold a slot (with `ArmExpiry` armed) past the TTL, then `ReapExpiredSlots` | Nothing reaped before the TTL; after it: 1 slot reaped, the armed cancel fired, `InFlight()==0`, the account serves again, and both `Release` calls stay idempotent |
| LEASE-TTL-002 | `SlotTTL=30ms` | Release normally, then wait past the TTL and reap | 0 slots reaped and the armed cancel never fires |
| LEASE-TTL-003 | No `SlotTTL` | Hold a slot, reap | 0 reaped: the reaper is off, so a deployment cannot silently lose capacity accounting |
| LEASE-TTL-004 | `SlotTTL=20ms` + `Logf` | Hold past the TTL, reap | Exactly one log line naming the account and the age |
| LEASE-TTL-005 | `SlotTTL=5s` | Acquire | The lease carries its own TTL and issue time, and does not report itself released |
| CONF-020 | `MIMO2API_SLOT_HOLD=first_byte` | `Load()` | An error naming `MIMO2API_SLOT_HOLD`; `" FIRST-BYTE "` (case/space) is accepted as `first-byte` |
| CONF-019 (extended) | env knobs set | accessor table | `LeaseTTL` and `SlotHold` answer from the parsed config, and `UpstreamTimeout()==LeaseTTL+30s` |

**Retired with this change:** the old `CONF-*` assertion that the request timeout was
300s (it is 120s now), and the implicit contract that a request had no client-facing
deadline at all — that was the dead-knob bug, now covered by TIMEOUT-001/002.

**Not covered by a test, deliberately:** that an intermediary (Cloudflare, a platform
router) cuts a long non-streaming request at its own ~100s. That is external
behaviour; the deployment check for it is a live long request through the public URL
(`docs/SDD.md` §7.6), and the mitigation is the default budget being below it.

## 9. ADR 0020 — account management CLI (MGMT)

The store may be a database with no file to edit, so management is a CLI resolved
against whichever backend the environment selects. Cases live in
`internal/accounts/manage_test.go` (store/backend semantics) and
`cmd/mimo2api/accounts_cli_test.go` (the commands themselves, stdout captured,
fake accounts only).

| Case | Precondition | Stimulus | Observable expectation |
| --- | --- | --- | --- |
| MGMT-001 | Empty file store | `Put` a new id, then `Put` the same id again with different fields | Two rows max; the second put replaces the first (no duplicate key); reopening the file shows both records; an id-less put is refused |
| MGMT-002 | Two accounts | `Remove` one, then `Remove` it again | First delete drops the row in memory and on disk; the second returns `ErrNotFound` (a CLI must be able to report it) |
| MGMT-003 | Backend whose writes fail | `Remove` an existing id | The error surfaces and the in-memory row survives — a failed delete never pretends success |
| MGMT-004 | File backend with ids `18`, `180`, `1800` | `DeleteAccount("180")` | Exactly one row goes (no substring matching); deleting a missing id is a no-op; the rewritten file stays mode 0600 |
| MGMT-005 | Store with a device anchor, session and secrets | `-accounts list` / `show <id>` | Output names the id and prints presence/ages; the password, pass token and cookie values are **not** in stdout; `list` totals and enable state match the store |
| MGMT-006 | One enabled account | `disable <id> passport locked`, then `enable <id>` | The file shows `enabled:false` + the recorded reason, then `enabled:true` and the reason cleared; an unknown id fails loudly |
| MGMT-007 | Account with no egress label | `set-egress <id> sg`, then `set-egress <id>` | Label set, then cleared; the label is the only thing written (endpoint/credentials stay in the environment) |
| MGMT-008 | Store with 2 rows, an import file with a changed row plus a new one | `rm <id>`, then `import <file>` | The row is gone; the import upserts (changed row replaced, new row added) and re-reads the store before reporting success |
| MGMT-009 | Any store | `export <file>` | The dump parses as an accounts file, holds every row, and lands mode 0600 (it is a credential file) |
| MGMT-010 | Empty store | `verify` | Non-zero exit with a message: an empty pool cannot serve a single request |
| MGMT-011 | — | `-accounts` with no command, with an unknown command, or `rm` with no id | Each fails with a message instead of doing something arbitrary |
| MGMT-012 | argv containing `-accounts list`, `-accounts=show <id>`, or no `-accounts` at all | `accountsFlagValue` | The command and its trailing arguments are extracted for both spellings; a server start without the flag is untouched |
