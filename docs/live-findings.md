# Live findings (real upstream, real account)

Run: `MIMO2API_LIVE=1 go test ./internal/live -v` (see `internal/live/README.md`
for the env switches). Credentials come from the operator's
`data/accounts.json`; the suite works on a **copy** in a temp dir, so a run
never mutates the live session.

Last run: **2026-10-09 14:29 CST**, account `182******16`, egress IP outside
mainland China (which is why the login chain traverses the cross-region
consent page — see `docs/login-chain-reverse.md`).

## Status of LIVE-001..010 and DEVICE-001

| ID | Case | Result | Evidence |
|---|---|---|---|
| LIVE-001 | Cold-start login | **PASS** (2026-10-09) | The full from-zero login ran end to end: with the device anchor on file the password login answers the new-device demand, the operator's single SMS code completes the challenge, and the rotated passToken + session + device identity are persisted. Second login for the same device: `LOGIN-OK in 3.058s`, no demand (DEVICE-003). |
| LIVE-002 | Model catalog | **PASS** | `GET /v1/models` → 200, contains `mimo-v2.6-pro` and `mimo-v2.6-pro-ultraspeed-studio`; no upstream call. |
| LIVE-003 | Non-streaming echo | **PASS** | 4.4 s, `content="pong"`, `finish_reason="stop"`, `usage={prompt:2230 completion:11 total:2241}`. |
| LIVE-004 | Streaming | **PASS** | 5.0 s, 5 frames: role-only delta → 2 content deltas (`"1\n2\n3\n4\n"`,`"5"`) → finish frame → usage frame (`choices:[]`) → `[DONE]` — the documented SDD §5.4 order. |
| LIVE-005 | Sticky two-turn | **PASS** | 27.0 s: turn 2 answered "Your favorite number is 42 — you told me earlier in this conversation", session store hit (`lookups=1`, `len=1`) → the delta path was taken. |
| LIVE-006 | Transparent refresh | **PASS** | 7.8 s with the cached session deleted: one STS renewal re-minted and persisted a 886-byte cookie with `ph` set, account stayed available. No password login, no OTP. |
| LIVE-007 | 429 semantics | **PASS (inconclusive, recorded)** | See below. |
| LIVE-008 | Captcha probability | **SKIP** | Needs 5 one-time codes (`MIMO2API_LIVE_OTP_CODES`); each cold login sends an SMS, so the run refuses to start without them. |
| LIVE-009 | Fastchat usage merge | **PASS (recorded)** | See below. |
| LIVE-010 | Conversation GC | **phase 1 PASS** | Phase-1 snapshot written (24 h gap required); phase 2 runs with `MIMO2API_LIVE_PHASE=2`. |
| DEVICE-001 | Device identity vs. new-device verification | **PARTIAL** (needs one SMS) | Reverse + 3 free live attempts: the demand persists even when the passport's own `deviceId/pass_ua/uLocale` are replayed, while production code previously sent *no* device cookie at all and dropped what the chain planted. Persist-and-replay is implemented (DEV-001..007, DEVPL-001); whether one completed verification then makes later logins verification-free is the open half. See below. |
| LIVE-SESS-001 | Session chaining vs replay | **PASS** | See below — pinned turn chained in 8.2 s, replay control 47.4 s, context preserved across turns. |
| LIVE-SESS-002 | One-shot stays stateless | **PASS** | `stream:false` with a live session id: 200, no session headers, store untouched (`sessions 1→1`, `lookups 1→1`). |
| LIVE-TTS-003 | Long speech beyond the cap | **FAIL (by design, then removed)** | 1110 runes → 2 pieces → 502 after 278.2 s: piece 1/2 failed upstream. The split worked; a ~1020-rune piece is beyond what the service synthesises. Cap is now refused up front (ADR 0010). |

## LIVE-007 — does the upstream 429 carry a structured deadline?

```
burst=30 requests, no pacing, model=mimo-v2.6-pro
no non-200 within 30 unpaced requests — no 429 observed
```

**Finding:** a sequential burst of 30 requests against one account did **not**
reach the upstream rate limiter (each request took ~5 s, so the burst ran at
roughly 0.2 req/s). Appendix B Q1 therefore stays **open**: the fixed cooldown
ladder in SDD §4.5 remains untested against a real 429. Answering it needs
either concurrency (N in-flight requests) or a much longer run — deliberately
deferred, because tripping the limiter cools the account down for real.

`UpstreamError` now retains the response headers (`mimo.UpstreamError.Headers`)
so that a future run can read `Retry-After` / `x-ratelimit-*` straight off the
429; those headers are still never echoed to API clients (TDD ERRMAP-011).

## LIVE-009 — fastchat repeated usage events

```
model=mimo-v2.6-pro-ultraspeed-studio frames=4 done=true usage_events=1
usage[0] prompt=2241 completion=177 total=2418
last total >= all earlier totals: true
VERDICT: a single usage event — no merge question for this model/run
```

**Finding:** the fastchat channel emitted exactly one usage event for a short
answer, so the "multiple usage, last-wins?" question (Appendix B Q3) is not
answered by this run — it needs a long/tool-heavy completion. The SSE-012
last-wins rule stays unconfirmed, not contradicted.

## LIVE-010 — conversation GC across a 24 h gap

Phase 1 ran a turn, saved the sticky store snapshot and the turn transcript.
Phase 2 (`MIMO2API_LIVE_PHASE=2`) restores the snapshot, sends a delta turn and
checks whether the answer still carries the fact from phase 1:

- answer references the fact → upstream auto-save suffices, SDD §4.3 unchanged;
- `event:error` / lost context → SDD §4.3 must be amended to call
  `POST /open-apis/chat/conversation/save` on store.

## DEVICE-001 — the device identity, and why every login looked like new hardware

Goal: find out how the upstream decides "this is a known device", so a re-login
stops demanding an SMS code every time.

**Reverse (2026-10-09).** The verification page belongs to Xiaomi's account SPA
(`mcfe--mi-account` chunks `IVPh`/`IVTPh`/`IVE`/`mobile`); the MiMo bundle contains
no passport code at all. The SMS form is `{ticket, trust}`, with `trust` defaulted
from the server's `trustCheckBoxSelected` — and we already submit `trust=true`
(`otp.go` step 5). Trust-device routes exist (`/identity/verifyPush`,
`/identity/verifyCode`, `/identity/auth/deviceAuth`, `/identity/verifyTrustMobile`)
but are offered only to accounts that already own a trusted device; our demand
advertises `flag=4, options=[4]` (SMS only). Details: `docs/login-chain-reverse.md`
§5.1.

**Free experiment (no SMS sent, 3 × `serviceLoginAuth2`).**

- A — no cookies (the production behaviour at the time): `code=0 securityStatus=16`
  + `notificationUrl`; response plants `deviceId, pass_ua, uLocale`.
- B — replay A's `deviceId + pass_ua + uLocale`: same demand.
- C — replay A's `deviceId` only: same demand.

⇒ Replaying the device cookies is not sufficient on its own: the trust has to be
*established* by one completed verification bound to that device.

**What it exposed.** Production code had no `deviceId` concept (two throwaway probes
fabricated a random one per run), `http.Client` had no cookie jar so
`serviceLoginAuth2` went out with no `Cookie:` header at all, and `sessionFromJar`
composed the session from six fixed fields — dropping the device cookies the chain
had just planted. Every login therefore presented new hardware, and the trust could
never attach.

**Shipped.** `internal/mimo/device.go` (whitelist harvest + replay, session cookies
excluded), `accounts.Account.Device` (persisted, merged — a silent server never
wipes a stored identity), replay on Auth2 + every identity/STS hop, adoption of a
rotated `passToken`, `api/session_apply.go` for the fold-in, and device-aware
SMS-safe probes (`cmd/loginprobe` reads the accounts file, never argv, and is built
with `DisableOTPAuto`; `cmd/otpprobe`/`cmd/otp2probe`/`dump.go` deleted).
Unit + e2e proof: DEV-001..007, DEVPL-001.

**Open half (one SMS).** Whether one completed verification makes the *next* login
verification-free. Recipe: complete one OTP login via `POST /v1/admin/otp/submit`,
then `go run ./cmd/loginprobe data/accounts.json` — exit 0 = device remembered
(renewal can re-login hands-free), exit 2 = still demanded.

## Notes for the next run

- `MIMO2API_LIVE_OTP_CODE` completes LIVE-001 (the whole point is that the
  gateway, not a script, walks consent → identity → landing chain).
- `MIMO2API_LIVE_OTP_CODES` (5 codes) enables LIVE-008.
- `MIMO2API_LIVE_PHASE1_STATE` overrides the phase-1 snapshot path (default
  `$TMPDIR/mimo2api-live-phase1.json` — move it somewhere durable before the
  24 h wait).
- The suite paces upstream calls by 2 s; the burst case is the only exception
  and is opt-in.

## LIVE-SESS-001 — the chained session is real, and it is the cheaper path

```
turn 1 (fresh, 3 messages replayed) → mode=replay elapsed=26.1s session=60dd9a45f6d0fc9190b684be519a3756
turn 2 (pinned, 1 delta message)   → mode=chain  elapsed=8.2s
turn 3 (control: same 4-message history, own session) → mode=replay elapsed=47.4s

chained frontier advanced: msgs 3 → 4, dialog anchor "16075414" → "16075443", same upstream conversation=true
chained answer: 海水很蓝
control answer: 海水很蓝
delta (control - chained) = 39.3s
```

What this proves, and what it does not:

- The pin is honoured across processes without stored state on the client's
  side: turn 2 sent only the new message (`deltaQueries=1`) and the upstream
  answered with the fact from turn 1 ("海水很蓝"), so the server-side
  conversation really was continued rather than reconstructed from replay.
- The frontier and the dialog anchor advanced on the chained entry, and the
  upstream `conversationId` was reused — the store, the header and the answer
  agree with each other.
- The control turn replayed the same history into a session of its own and paid
  47.4 s against 8.2 s. Latency on this service is noisy (turn 1's own replay
  cost 26.1 s), so read the delta as "the replay path pays for the whole history
  and the chain does not", not as a fixed speed-up factor.
- **Not covered live**: account migration (SESS-040, covered by the integration
  test) and the concurrent-fork race (SESS-020/021, covered by the integration
  tests) — both need either a second account or two simultaneous streams, which
  the live suite deliberately does not do against the operator's single account.

## LIVE-TTS-003 — a split near the cap does not help, and the cap is now refused

```
input: 1110 runes ("小米 MiMo 语音合成测试。" × 74), format wav
result: HTTP 502 after 278.2 s
body:   the upstream audio task failed: piece 1/2:
        upstream audio task failed (task 16075873, audit PASS)

earlier, same day, single syntheses (no splitting):
  128 runes → 12.8 s, audio returned
  384 runes → 17.8 s, audio returned
  700 runes → no answer within a 220 s client timeout
```

Reading it:

- **The split fired**: the error names `piece 1/2`, so the gateway cut the text
  on sentence boundaries and submitted pieces in order — the mechanism worked.
- **The failure was the upstream's own**, not our deadline: the task came back in
  a failed state (its content audit passed) at 278 s, while that piece's budget
  was ~570 s (60 s + 500 ms × 1020).
- **Piece size is the cause**: the splitter packs up to the 1024 cap, so an
  1110-rune input becomes ~1020 + ~90, and ~1020 runes sits far outside the
  reliable region this service shows. Raising the deadline cannot fix that.
- **Outcome**: >1024 input is now refused with 400 `input_too_long` before any
  upstream work, the splitter is deleted, and `MIMO2API_SPEECH_MAX_CHARS` is
  clamped to 1024 (ADR 0010). The old path cost ~278 s of upstream time per
  over-length request and then failed anyway.

## LIVE-POOL-001 — the pool view against the real account (2026-10-09)

Run: `MIMO2API_PORT=18210 MIMO2API_API_KEYS=live-probe
MIMO2API_ACCOUNTS_FILE=/root/mimo2api/data/accounts.json ./mimo2api` (HEAD `139d796`,
the four pool waves). Read-only except for one small chat turn.

- `GET /healthz` → `{"status":"ok","accounts":{"total":1,"available":1,"cooldown":0,"disabled":0}}` —
  the frozen four-aggregate shape is intact.
- `GET /v1/admin/pool` without a key → 401; with the key → 200.
- Before any request: `{"id":"acct-40f3e6eb","egress":"direct","state":"available",
  "in_flight":0,"max_in_flight":2,"ewma_latency_ms":0,"failures":{},
  "credential_age_s":9356}`, `invariant_ok: true`. The real account carries no
  egress label, so it reports `direct` — which is the intended default.
- One real non-streaming turn (`mimo-v2.6-flash`, `max_tokens: 16`, `"hi"`) →
  200, `chatcmpl-11ab70195f20c4ef…`, content `Hi there! How can I help you today?`.
  The view afterwards: `ewma_latency_ms: 848`, `in_flight: 0`, `state: available`,
  `failures: {}` — the accounting reflects what the request actually did.
- Credential scan of the view body: the cookie (`serviceToken=…`), the `ph`, the
  passToken, the phone number and the raw account id are all absent. The id appears
  only as the stable mask `acct-40f3e6eb`, and `credential_age_s` matches the real
  session age.

### Still unverified, and why

- **LIVE-POOL-002 (passive 451/461 observation)**: not observed. The real service
  has never returned 451/461 to this gateway, and inducing one is forbidden
  (REQ-9). "No observation yet" is the expected result, not a gap in the run.
- **LIVE-EGRESS-001 (a real account behind an operator-supplied egress)**: not run.
  It needs an endpoint the owner supplies; labelling an account without one fails
  startup by design, so the labelled path is covered end to end by
  `internal/api/egress_e2e_test.go` against a real forwarding proxy, not live.

## LIVE-MODEL-REDIRECT — retired ids, found while probing the egress (2026-10-09)

Found by accident: a live `/v1/audio/speech` naming `mimo-v2-tts` answered **502**
in 0.6 s, while `mimo-v2.5-tts` synthesised a 76 844-byte WAV. The same 502 occurred
on an instance with no egress label at all, so it was not the egress work.

With the upstream's own error body logged (`MIMO2API_LOG_LEVEL=debug`):

- `mimo-v2-tts` → `400 {"code":400,"msg":"model: [mimo-v2-tts] not support"}`
- `mimo-v2.1-tts` → `400 {"code":400,"msg":"modelCode cannot be empty"}`
- `mimo-v2.5-tts` → 200, real RIFF WAV

`bot/config` explains it: rows carry `redirectTo` and **18 of 25 are superseded**,
including the chat models `mimo-v2-flash`, `mimo-v2-pro`, `mimo-v2.5`, `mimo-v2.5-pro`
and the whole v2.1 audio family. The Studio SPA builds its
`deprecatedModelRedirectMap` from that field and never sends a superseded id.

Chat refuses a superseded id differently — with the busy frame. Three interleaved
rounds inside one minute:

| model | round 1 | round 2 | round 3 |
|---|---|---|---|
| `mimo-v2.6-flash` (current) | 200 | 200 | 200 |
| `mimo-v2-flash` (superseded) | 503 busy | 503 busy | 503 busy |
| `mimo-v2.5` (superseded) | client timeout | 200 | 200 |

A second sweep on the same instance (before the fix) hit two superseded chat ids at
once — `mimo-v2-flash` → 503 `upstream_busy`, `mimo-v2-pro` → 503 `upstream_busy` —
while the current ids answered 200 in the same sweep. Both carry `redirectTo` in
`bot/config`, so the refusal is a property of the id, not a random platform
throttle: the busy frame is how chat says "this model is retired". (The list and
the resolution map both follow the catalog, so the fix covers ids the sweep never
named.)

After ADR 0011 (list cleaned, requests resolved, refusals learned from), the same
live instance on port 18240:

- `GET /v1/models` → **7** models, all `source=account`: `mimo-v2.6-pro`,
  `mimo-v2.6-flash`, `mimo-v2.6-pro-ultraspeed-studio`, `mimo-v2.5-tts`,
  `mimo-v2.5-tts-voiceclone`, `mimo-v2.5-tts-voicedesign`, `mimo-v2.5-asr`.
  Startup logs `models: 18 superseded ids will be resolved before they reach the
  upstream`.
- `/v1/audio/speech` naming `mimo-v2-tts` → **200**, 76 844 bytes, real WAV
  (was 502), log line `model: mimo-v2-tts is superseded by mimo-v2.5-tts upstream`.
- chat naming `mimo-v2-flash` → **200** (was 3/3 busy); `mimo-v2.5` → 200; the
  response echoes the id the caller asked for.
- `GET /v1/models/mimo-v2-flash` → 200 with `"redirect_to":"mimo-v2.6-flash"` and the
  successor's real capability bits.

## LIVE-TOOLS-001 — tool calling against the real model (2026-10-09)

The chain was run for real, with one account and the real model: our gateway on
18250, ToolForge from the fork (commit `758a2c4` + the identity patch) on 18701,
and the middleware's upstream pointed back at `/v1/raw/chat/completions`.

Round 1 — `get_weather(city)` offered, prompt "东京今天天气怎么样？用工具查一下":

```
finish_reason: tool_calls
tool_calls: [{"id":"call_ziacq1eyu89y","type":"function",
              "function":{"name":"get_weather","arguments":"{\"city\":\"东京\"}"}}]
```

39 s, `http=200`. The model, which has no native function calling, followed the
injected protocol and produced the argument correctly.

Round 2 — the result (`东京：晴，24°C，湿度 55%`) sent back as a `role:"tool"` turn:
`finish_reason: stop`, and a natural-language answer that used exactly that data
(晴 / 24°C / 55%), 49 s. The middleware saw its own call, and the loop terminated
at the raw path.

Control in the same session: a plain chat request with no tools answered directly
by the gateway ("只回两个字：收到" → `收到`) and never reached the middleware.

What the patch is for (and why it is not optional): with the middleware
authenticating upstream under its own key, every caller would arrive at the
gateway under one credential, and session identity is derived from
(credential ‖ model ‖ system ‖ tools ‖ first message) — the same first message
with the same tools would land two different callers in the same session. Live
observation of the headers at the callback hop: `authorization: Bearer
sk-mimo-tool-demo` (the caller's own key) and the caller's `X-Mimo-Session-Id`.

### LIVE-TOOLS-002 — the deployment's single entry point (2026-10-09)

`docker compose up -d --build` with `MIMO2API_TOOL_KEY` set, one published port
(8081), the gateway's 8080 left on the compose network. Real account, real model,
real containers:

| Call on :8081 | Result |
|---|---|
| `GET /healthz` | `{"status":"ok", ..., "upstream_health":{"status":"ok","code":200}}` |
| `GET /v1/models` | 200, the gateway's real catalog (7 models) — relayed |
| `POST /v1/chat/completions` + `tools` | 200, 18.3 s, `finish_reason: tool_calls`, `get_weather({"city":"广州"})` |
| `POST /v1/audio/speech` | 200, 5.8 s, `content-type: audio/wav`, 153,644 bytes, RIFF/WAVE header intact |
| `POST /v1/audio/transcriptions` (multipart, the WAV above) | 200, 2.8 s, `单口透传验收，今天天气不错。` |
| `POST /v1/chat/completions` without tools | 200, 40.5 s, plain answer |
| `GET /v1/admin/pool` | 200 — the admin surface relayed like any other path |
| `GET /v1/models/nope` | 404 — the gateway's own status, not a proxy artefact |
| wrong key / no key | 401 both |
| `curl :8080/healthz` | connection refused — 8080 is not published |

Two findings on the way, both fixed in the vendored copy: the config loader builds
`FeaturesConfig` field by field, so the new flag was silently off until it was listed
there (visible only because `/healthz` did not report `upstream_health`); and a
client is not the only thing that talks to `httpx.AsyncClient` — the first version of
the relay's tests patched the class globally and broke the test client itself.

One oddity seen while running this, not caused by the tool layer and not yet
explained: during round 2 the gateway logged an upstream `web_search` event with
41 KB of results while `web_search_status` was left at its default (`disabled`).
The direct path behaves the same way, so it is upstream-side; worth a look before
anyone relies on the switch being a hard off.


## DEVICE-002 — passport fingerprint alignment (2026-10-09)

Reverse + free live probes only (no code spent while investigating):

| Probe | Result |
|---|---|
| `GET {aistudio}/open-apis/user/mi/get` (cookieless) | 401, body carries the real `loginUrl` (`callback`, `sid=xiaomichatbot`, `_group`; no `qs`/`_sign`) |
| `GET {loginUrl}` (browser navigation headers) | **302** to `https://account.xiaomi.com/fe/service/login?...&_sign=…&serviceParam={…}&bizDeviceType=`; plants `deviceId=wb_<uuid>` (390 d), `pass_ua=web`, `uLocale=zh_CN`, `theme`, `JSESSIONID` |
| same URL, cookieless, repeated | mints a **new** `deviceId` each time — no cookie context means new hardware |
| `/fe/service/login` page | the account SPA (`main.6a03bf94.js` + `axios` + `crypto-util`), same bundles already reversed for the OTP UI |
| MiMo probe with the page's cookie jar | still 401 — the page navigation alone mints no session (no free-session shortcut exists) |

Consequence for the chain: it never loaded that page, so `serviceLoginAuth2` went
out cookieless and each attempt was new hardware; and the device cookies that
the chain *did* receive were thrown away whenever the login ended in the demand.

Lesson from the incident that followed (recorded because it cost a real SMS): a
"free" dry run of a session-less account file is **not** free — starting the
gateway runs the catalog warm-up, which falls back to a password login and, with
`MIMO2API_OTP_AUTO=true`, sent a real code. The instance was then killed, taking
the in-memory verification session with it, and the platform refused the next
send with `code=20024` (SMS rate limit). Hence ADR 0013: the dispatch defaults to
off, the harvested device identity is persisted even on an aborted login, and a
code is only useful if the challenge survives to the operator's submit —
upcoming, if the challenge should also survive a restart.


## DEVICE-003 — the OTP login completed, the credential healed, the device is remembered (2026-10-09)

The first real completion of the new-device flow on this deployment, run against a copy of
the account file and merged back afterwards.

| Step | Result |
|---|---|
| Login attempt (session-less copy, `MIMO2API_OTP_AUTO=true`) | demand at 16:59:13, `code sent to +86 182******16`; challenge `otp-dbe010705a092a2fb9774bd4746c5ad3` registered (10-minute window) |
| Device identity after that **aborted** attempt | persisted anyway: `deviceId=wb_63a194b2-…`, `pass_ua=web`, `uLocale=zh_CN` — the fix that makes the next attempt the same device |
| Code submitted 16:59:43 (operator, via `/v1/admin/otp/submit`) | `{"account":"18278724616","status":"ok"}`; log: `otp: account 18278724616 logged in via OTP (Phone)` |
| Credential | **passToken rotated** (`cea93425…` → `ad551ba2…`), session cookie replaced (886 → 923 chars), `cUserId` harvested |
| `cmd/stsprobe` (probe → renew → validate) | `RENEW-OK in 2.389s` / `2.447s` / `2.488s`, `VALIDATE-OK` — renewal works again |
| **Second password login, no SMS** (`cmd/loginprobe`, `DisableOTPAuto`) | `LOGIN-OK in 3.058s: userID=2652042633 session_cookies=6 ph=true` — **no new-device verification demanded**; device identity unchanged |
| Deployment on the real account file | `/healthz` 1 available; `/v1/models` 7 models, `source=account` (live catalog read); `/v1/admin/pool` `invariant_ok: true` |

Two semantics confirmed live, both previously only inferred: a **full login issues a new
passToken** (renewal never does), and **one completed verification makes later logins on the
same device verification-free** — the deployment therefore self-heals as long as the stored
device identity is kept.

Cost note: this used exactly one SMS. The attempt before it spent one accidentally (startup
catalog warm-up with the old `MIMO2API_OTP_AUTO=true` default) and then hit `code=20024`,
which is why ADR 0013 flipped that default and why an aborted login now persists its device
identity.


## DEVICE-004 — credential lifetimes, and which flow actually rotates the passToken (2026-10-09)

Asked directly ("can the login result show the validity?"), so measured instead of quoted.
Live `Set-Cookie` attributes from the login/renewal chain (values never printed):

| Cookie | Max-Age | Expires | Notes |
|---|---|---|---|
| `passToken` | 2592000 = **30 d** | 2026-11-08 | `Domain=.account.xiaomi.com; Secure; HttpOnly` |
| `cUserId`, `userId`, `ptn_count` | 2592000 = 30 d | 2026-11-08 | the account-scoped set |
| `deviceId` | 33696000 = **390 d** | 2027-11-03 | the device anchor |
| `pass_ua` | 7689600 = 89 d | 2027-01-06 | |
| `uLocale` | 1296000 = 15 d | 2026-10-24 | `Domain=.xiaomi.com` |
| `passInfo` | — | — | session cookie |
| `theme`, aistudio `userId` | 0 | 1970 | deletions |

Which flow rotates the credential, measured rather than inferred:

| Flow | Body `passToken` | Verdict |
|---|---|---|
| `serviceLogin&_json=true` (renewal) | byte-identical to the one sent (same sha256 prefix `ad551ba2bf45`, 347 chars) | **echo, no rotation** |
| password login + OTP (16:59) | `cea93425…` → `ad551ba2…` | **rotation** |

So: a passToken lives 30 days, renewal keeps it and never mints a new one, and only a real
login replaces it — which is why an account that dies at 3.5 hours (70016) was revoked by
risk control, not expired. Also note the renewal body carries `ssecurity`, `psecurity`,
`cUserId`, `nonce` and `securityStatus: 0` alongside the echoed token.

The gateway currently **discards both signals**: `collectCookies` keeps only `name=value`
(attributes dropped) and the account file has no expiry field. Recording them
(`credential_expires_at`) and showing the TTL in `/v1/admin/pool` is the obvious next
small change: `credential_age_s` says how old the credential is, never when it dies.
