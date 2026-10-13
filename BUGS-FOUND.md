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

