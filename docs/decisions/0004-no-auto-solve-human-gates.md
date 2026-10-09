# 0004 — Never auto-solve a human gate; hand it to the operator

Date: 2026-10-09
Status: accepted

## Context

The login chain can demand two things a program must not fake:

1. a **captcha** (`captchaUrl` on the auth2 answer), and
2. a **new-device OTP code** delivered by SMS to the account owner.

The project's standing constraint is account safety over engineering
convenience: fabricated/faked security parameters (a guessed code, a solver
service, a spoofed device token) risk the account, and a gateway that burns
the owner's SMS budget is unusable in practice.

## Decision

- **No captcha solving.** A captcha answer disables the account with a reason
  naming the demand; an operator resolves it in a browser.
- **The OTP challenge may be started** (`MIMO2API_OTP_AUTO`, default true) —
  that only dispatches the message — but the **code is always supplied
  out-of-band** by the operator through `POST /v1/admin/otp/submit`. The
  challenge lives in memory with a 10-minute expiry; the code is never logged.
- **The live acceptance suite must not dispatch an SMS by itself.** A cold
  login test runs with auto-start off and skips with a message naming
  `MIMO2API_LIVE_OTP_CODE`; the captcha-probability case needs the operator to
  supply five codes up front (`MIMO2API_LIVE_OTP_CODES`) or it skips.

## Consequences

- Any test or script that would send an SMS is opt-in and explicit; a routine
  `make test` or a live run without switches never touches the phone.
- Operations keep a clear seam: the service reports `otp_pending` with the
  masked destination, a human types the code, the account returns to rotation.
- Automated end-to-end login from scratch always costs one human action — an
  accepted cost, recorded rather than worked around.

## Evidence

- `internal/api/otp_admin_test.go` (ADOTP-001..003), `internal/live/live_protocol_test.go`
  LIVE-001/LIVE-008 (env-gated skips), `internal/live/README.md`.
- `docs/live-findings.md` records LIVE-001 skipping for exactly this reason.
