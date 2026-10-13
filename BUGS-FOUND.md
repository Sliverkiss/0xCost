# Bugs found in the 2026-10 repo-wide sweep — documented, not fixed

Format per the sweep brief: location with file:line, evidence, proposed fix.
Entries here are findings whose fix exceeded the ~20-line budget or whose
correct behaviour needed a product decision, so they were recorded instead of
changed. Fixed findings live in git history (`6f53f8e` and later).

## 1. handleOTPSubmit: concurrent submits race the challenge claim

**Location**: `internal/api/otp_admin.go:141-155` (`handleOTPSubmit`)

**Evidence**: `s.otp.get(body.ID)` reads the challenge but does not remove or
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

## 2. creds(): on-demand login has no single-flight

**Location**: `internal/api/chat.go:737-764` (`creds`)

**Evidence**: when an account has no cached session, every concurrent
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
