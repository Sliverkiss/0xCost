# Bugs found in the 2026-10 repo-wide sweep — documented, not fixed

Format per the sweep brief: location with file:line, evidence, proposed fix.
Entries here are findings whose fix exceeded the ~20-line budget or whose
correct behaviour needed a product decision, so they were recorded instead of
changed. Fixed findings live in git history (`6f53f8e` and later); the entries
below were later fixed with the boss's approval and are kept as the audit
trail, annotated with their fix SHAs.

## 1. handleOTPSubmit: concurrent submits race the challenge claim — FIXED

**Location**: `internal/api/otp_admin.go:141-155` (`handleOTPSubmit`)

**Fixed in `a7274e1`** (fix(api): claim the OTP challenge before verifying,
restore it on a wrong code). The submit claims (removes) the challenge under
the store's mutex before calling SubmitOTP; the loser of the claim 404s
instead of verifying a second time. A 70002-class rejection restores the
challenge so the retry window survives — unless the account vanished mid-
submit or a session already landed (login completed elsewhere). Tests:
`otp_submit_claim_test.go` ADOTP-005/006 (red: 2 upstream verifications for
a double submit; green: 1).

**Evidence** (pre-fix): `s.otp.get(body.ID)` reads the challenge but does not remove or
claim it. Removal happens only on success or account-vanished
(otp_admin.go:160,174). Two concurrent POST /v1/admin/otp/submit with the same
challenge id therefore both pass the existence check and both run
`SubmitOTP` upstream; on success both run `applySession` → `store.Update` →
`pool.Enable`.

**Impact**: the persist/enable side effects are convergent (same session
data, set-state), so state does not corrupt. The waste is two upstream
verifications and two store writes. The wrong-code path deliberately keeps
the challenge live (retry within the window is a real flow), so a submit
flood re-verifies against the upstream for the 10-minute challenge lifetime —
bounded by admin-key auth, but not by anything else.

**Proposed fix** (~30 lines): claim the challenge before verifying —
`remove` it up front, and restore it when `SubmitOTP` answers a wrong code
(70002-class) so the retry window the design intends survives. Care: the
restore must not resurrect a challenge whose account vanished, and the
removal-on-success path already exists.

## 2. creds(): on-demand login has no single-flight — FIXED

**Location**: `internal/api/chat.go:737-764` (`creds`)

**Fixed in `099727b`** (fix(api): single-flight credential resolution so a
login burst cannot stampede OTP challenges). One shared per-account
`loginGate` (recovery.go) covers both the foreground `creds` and the
background `recoverCredential`. Losers re-read the store: the winner's
session if it landed one, the winner's registered challenge as
ErrOTPPending if it stopped at the new-device gate, or the ADR 0014
account_action fail-fast while a background recovery runs. The
live-challenge check also runs on the fast path, so a late-arriving request
cannot start a second challenge either. Tests: `creds_gate_test.go`
CREDS-001..004 (red: 2 login chains / 2 challenges for a burst of 2; green:
1 / 1).

**Evidence** (pre-fix): when an account has no cached session, every concurrent
request holding a lease on it runs the full `Login` chain (renewal is tried
first and its failure path has the ADR 0014 single-flight via
`RecoverCredentialAsync`, but the fallback password login at chat.go:737 has
none). Each login that hits the new-device OTP demand auto-starts a challenge
and registers it in `otpStore` (chat.go:743-745) — the OTP
double-registration shape, one challenge per concurrent request.

**Impact in practice**: bounded. The first ErrOTPPending disables the account
(`isAccountDead` → `ReportDisabled`, chat.go:338-344), so the burst is
limited to requests that already hold leases when the first failure lands,
plus any that read the store before the session was persisted. Two co-existing
challenges in otpStore confuse the operator (which one is the code for?) but
both belong to the same device identity (persisted via
`persistDeviceFromError`), and submitting either completes the login.

**Proposed fix** (~25 lines): a per-account login mutex (or single-flight
group) around the Login call in `creds`, shared with `recoverCredential` in
recovery.go, so one login at a time per account; the losers either reuse the
session the winner persisted or see the challenge it registered.

## 3. Deferred lease.Release in the chat retry loop — examined, safe

**Location**: `internal/api/chat.go:244`

Not a bug. `defer lease.Release()` is registered inside the per-attempt loop,
so the leases of earlier attempts release at function exit — after their
explicit releases. Safe because `Lease.Release` is idempotent
(`l.released` flag, internal/pool/pool.go). Recorded here so the next sweep
does not re-litigate it.

## 4. Local cooldown amplified upstream transient signals into long
unavailability — REMOVED by owner ruling

**Ruling** (owner, 2026-10, verbatim): “我们不主动加冷却退避，影响到操作了。
清理掉这些代码，改成透传，这个冷却是不应该的。” — and again: “我们不要冷却啊，
不需要你给我加退避，不要自作主张，该砍的就给我砍了”

**What the mechanism did** (pre-removal): every upstream 429, 5xx, or transport
failure was recorded per-class on an exponential-backoff ladder
(`internal/pool/pool.go`: 60s→10m for 429, 30s→10m for 5xx, 15s→5m for
transport, doubling per consecutive failure). A cooling account was removed
from selection entirely; when all accounts were cooling the pool answered its
own synthetic 429 `pool_parked` with a locally-invented Retry-After. The chat
pipeline additionally retried an upstream 5xx once on a different account,
masking the failure from the client.

**The problem**: a transient upstream signal — one 429, one 502 — became
minutes of account unavailability *we* invented. The upstream recovered in
seconds; our ladder kept the account benched and, with few accounts, turned a
blip into a client-visible outage (`pool_parked`) with a Retry-After we made
up out of a config default, not anything the upstream said. The retry-on-
another-account did the same at the request level: it hid the failure and
doubled the upstream load during exactly the pressure that caused it.

**The removal** (this sweep): the entire ladder/cooldown/parked machinery is
deleted — no compat layer, no feature flag, no config knob
(`MIMO2API_COOLDOWN_*` env vars are gone too). Accounts now have exactly two
states, available / disabled; the only paths to disabled are risk-control
(451/461, fail-closed with evidence) and dead credentials. Upstream failures
pass through verbatim: status, body, and the upstream's own Retry-After when
it sent one. `ReportSuccess` stays as a no-op pipeline hook; `Report429`
records diagnostics only (last-error status/time for `/v1/admin/pool`).
ErrBusy (concurrency slots) is untouched — it is a capacity signal, not
backoff. `/healthz` and `/v1/admin/pool` report the two-state aggregates.
Tests: the ladder/parked cases were deleted or rewritten as pass-through
assertions (`passfail_test.go` PASSFAIL-001..004: upstream 429 → client 429
with the upstream's body and Retry-After, account still available, no
parking on repeated 429s).

## 5. Request paths renewed/logged in mid-flight — REMOVED by owner ruling

**Ruling** (owner, 2026-10-14, verbatim): “那个过期后自动重新token续期的
逻辑我们砍掉，改成只通过定时任务每小时自动续期，定时巡检比被调用接口
造成问题要稳定。”

**What the mechanism did** (pre-removal): `creds()`/`credsNow()` on the chat
and audio paths repaired sessions on demand — a missing session triggered a
password login inside the request, an auth-class upstream error triggered
`refreshSession()` (Renew + persist) and, on a dead credential,
`RecoverCredentialAsync` in the background. A burst of concurrent requests
on a session-less account could therefore start logins from inside request
handling — exactly the traffic pattern risk control reacts to — and a slow
renewal/login chain held the request (and its pool slot) for the whole
passport round-trip.

**The problem**: the request path was both a correctness hazard (concurrent
logins stampede OTP challenges — see #2 — and a wedged passport stalls
in-flight requests) and a stability hazard (an hourly-cadence concern was
being decided per-request). The upstream is the single source of truth: a
401 means the session is dead *now*, and the client should hear that, not
wait while the gateway re-logs-in.

**The removal** (this sweep): `creds()` is a pure store read — cookie/PH from
the account record, nothing else. A missing session fails with a
`noSessionError`; an upstream 401/auth-expiry passes through to the client
verbatim (same structure as the 429 pass-through ruling, #4), and the
account is NOT disabled by the request path. `refreshSession()`,
`RecoverCredentialAsync`, and `recoveryState` are deleted outright — no
compat layer, no flag. Session freshness moved to `RunRenewJob`
(`internal/api/renewjob.go`): one round immediately at startup, then one per
hour (`MIMO2API_RENEW_INTERVAL`, minutes, default 60; replaces
`MIMO2API_SESSION_REFRESH_HOURS`), walking enabled accounts serially in ID
order — Renew first, one password Login under the existing `loginGate`
single-flight only when the credential is dead (`renewalFailureClass` /
`IsDeadCredential` stay for exactly this), one `[renew-job]` log line per
account, no retry storm and no backoff: a failure waits for the next tick.
Tests: `norenew_test.go` NORENEW-001..004 (request path: 401 passes through
as 401 with zero passport calls, account stays available; no session →
fail with zero logins; burst → zero logins; creds reads the store only) and
`renewjob_test.go` RENEWJOB-001..005 (startup round renews every
enabled+passToken account once; hourly cadence; dead credential → exactly
one login with the session persisted; transient failure retried on the next
tick only; one log line per account in ID order). The OTP-admin,
consent-admin, and device-persistence tests were reworked to seed their
challenges through a synchronous `renewRound` — the only login path left.

## 6. Five self-imposed gateway restrictions — REMOVED by owner ruling

**Ruling** (owner, 2026-10-14, verbatim): “并发槽砍了，等待800m砍了。
upstream is busy的800ms改成0，直接重试，最多3次。字数预检砍了，超了上游
会自己报错透传。recovery砍了，主动处置砍了。”

Governing principle: 上游是唯一真相源，我们只做纯管道 — the upstream is
the single source of truth; the gateway is a pure pipe. Five mechanisms we
invented for its benefit were actually decisions taken away from it.

**1. Concurrency slots.** Per-account in-flight slots (MaxInflight, default 2)
with a 15s queue wait (ErrBusy/pool_busy), a slot TTL reaper, and warm-up
penalty — gone. `pool.Acquire` is a single-pass availability check with
latency scoring and band round-robin; every request that arrives gets an
account (NOSLOT-001/002/003). A lease is account identity passthrough only
(ADR 0017); `Release` is an idempotent no-op. LeaseTTL survives as the
request-liveness **silence** window feeding `chatReq.IdleTimeout`
(ErrSilentUpstream/ErrIdleStall, ADR 0036) — capacity was cut, liveness was
not. `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT`, `MIMO2API_QUEUE_WAIT_S`,
`MIMO2API_SLOT_HOLD`, and the `in_flight`/`max_in_flight` admin fields are
deleted.

**2. Busy backoff 800ms → 0, retries → 3.** An in-band “服务器繁忙，
请稍后再试” frame now retries immediately, up to 3 times (4 attempts total),
then answers 503 `upstream_busy` with the platform's own sentence attached
(ADR 0033). No `time.After` between attempts (BUSY-003: busy ×3 + success in
<2s wall).

**3. Character precheck.** Both chat (per-message cap loop +
ComposedFirstQueryLen, `adapter.MaxQueryChars`/`config.MaxQueryChars`) and
speech (`SpeechMaxChars` local precheck) are deleted. Over-length requests
are forwarded as sent and the upstream's own refusal passes through
(TOOLONG-005, ERRMAP-010, TTSLIMIT-001: 400 `context_too_long` /
`input_too_long` carrying the upstream's evidence; account stays available).
`MIMO2API_MAX_QUERY_CHARS` and `MIMO2API_SPEECH_MAX_CHARS` are gone;
`mimo.SpeechTextLimit` stays as the upstream's mirror for translation only.

**4. Async recovery.** Verified already removed by commit `1490fa0` (see #5):
`RecoverCredentialAsync` has zero non-test remnants; grep clean.

**5. Consent-anomaly sit-out.** `/v1/admin/consent/agree` no longer calls
`ReportDisabled` when the consent login lands on a new-device OTP demand —
the challenge is registered for the operator and the account stays
selectable (CONSENT-API-005). Consent state must not decide account
availability; the renew-job's own dead-credential disables (renewjob.go) are
credential management, not consent action, and stay.

**The removal** (this sweep): no compat layers, no switches — direct
deletion across pool, api, adapter, config, testserver, and cmd. Acceptance
gates: `go build ./... && go vet ./...` clean; `go test ./internal/... -race
-count=1` all green; residue grep (`MaxInflight|max_in_flight|ErrBusy|
pool_busy|busyRetryBackoff|queryCharCap|MaxQueryChars|RecoverCredentialAsync`)
finds only must-NOT-appear test assertions and ruling-mention comments;
`docker compose up -d --build` live checks: /v1/models lists 7 models, chat
200, /v1/admin/pool has no in_flight/cooldown/consent markers, and an
over-cap ultraspeed request returns the upstream's own refusal (400
context_too_long via `upstreamTooLong` translation, account still available).

## 7. The 2026-10-15 451s are an account ban, not a fingerprint failure — DOCUMENTED (2026-10-15)

**Symptom:** every generation surface (chat incl. fastchat, TTS, ASR) returned
451 with the literal 4-byte body `????` and `x-mife-upstream-status: 451`,
while EnsureConversation (conversation/save) and the whole passport surface
kept working. Suspected an outdated SDD §2.4 fingerprint.

**Investigation (headless Chromium 156 + curl, no code changes made):**

1. Frontend reversed from `runtime-main.94df3f32.js` → `main.f4f92c2a.chunk.js`.
   The SPA's chat request set is UNCHANGED from what we send: same path
   (`/open-apis/bot/chat`, `/fastchat/open-apis/bot/chat`), same body shape,
   same headers (`Accept-Language`, `x-timeZone`, `Content-Type: application/json`),
   same `xiaomichatbot_ph` query param, `credentials: same-origin`. No new
   signature headers (no x-sign/x-nonce/x-mife) exist anywhere in the bundle.
   Only difference: current Chrome is v156 (our stored UA says 153) — cosmetic.
2. Loaded the real site with the account's cookie in headless Chromium. The SPA
   itself shows the ban banner: "尊敬的用户，经平台监测当前账号存在异常使用行为，
   部分功能暂不可用" — i.e. `user/mi/get` returns `bannedStatus: "TEMPORARY"`,
   and the frontend's handleError maps 451→TEMPORARY / 461→PERMANENT bans and
   renders exactly this banner. The chat input never even fires the request.
3. A chat POST issued from inside the real browser (true Chrome-156 fingerprint,
   real cookies) got **451 `????` — identical to our Go client**.
4. Auth-vs-ban discrimination: garbage cookie → 401; valid cookie → 451. So
   MiFE authenticates the account fine, then the upstream refuses generation
   for it specifically. Read-adjacent writes still pass (conversation/save 200,
   genTitle 200), and anonymous requests are 401 — the block is account-scoped,
   generation-endpoint-scoped, and server-side.

**Conclusion:** not IP-level, not fingerprint, not request construction. The
single account in `data/accounts.json` (userId 2652042633) carries a
TEMPORARY ban on the generation endpoints. No `client.go` change can fix it;
the cure is a different (or unbanned) account. Evidence artifacts (not
committed): /tmp/aistudio_diag/ — requests.json, usermi.json, botconfig.json,
ui_after_send.png.

**Unrelated findings while in there (FYI, no action taken):**
- Current catalog (`bot/config`, open endpoint) now carries v2.6 model ids
  (`mimo-v2.6-pro`, `mimo-v2.6-flash`, `mimo-v2.6-pro-ultraspeed-studio`) —
  our `catalogEntries`/redirect layer already reads this list live, so model
  ids track the upstream automatically.
- `reactEnabled: false` for this account — the v3 react/chat path is not
  active; v2 `/open-apis/bot/chat` remains the correct route.
