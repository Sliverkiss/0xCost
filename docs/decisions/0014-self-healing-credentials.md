# ADR 0014 — Self-healing credentials: 12-hour keepalive + asynchronous re-login

Status: accepted (2026-10-09). Amends SDD §2.3/§7.1.

## Context

Everything automated mints from the passToken, so a revoked credential used to
stop the deployment until a human intervened. Two measurements change that:

1. a passToken lives **30 days** natural lifetime (live `Set-Cookie`), while a
   renewal **echoes** the token it is sent and never rotates it — only a full
   login replaces it (DEVICE-004). So the keepalive cannot extend the credential;
   it only keeps the session cookie warm;
2. a password login on a **known device** now completes hands-free —
   `LOGIN-OK in 3.058s`, no new-device demand (DEVICE-003) — because the device
   anchor is persisted and presented on every hop (ADR 0013).

Observed failure that motivates this: a credential with a 30-day lifetime was
revoked by risk control after 3.5 hours (`code 70016`). No amount of renewing
fixes that; only a login does.

## Decision

1. **Keepalive cadence: 12 hours** (`MIMO2API_SESSION_REFRESH_HOURS`, default
   changed from 6). It is a session-cookie refresh, not a credential refresh, so
   the interval trades staleness against upstream chatter; 12 h keeps the cookie
   comfortably inside its life.
2. **A dead credential schedules an asynchronous password re-login**
   (`internal/api/recovery.go`, triggered from the sweep and from any request
   that discovers it). The recovery rotates the passToken, persists session and
   identity, and puts the account back in rotation.
3. **The discovering request does not block on the login.** It answers
   `account_action` — "credential revoked; automated re-login in progress — retry
   in a few seconds" — and the account heals in the background. A caller that
   retries succeeds; a request that waited would hold a stream slot for the length
   of a full login chain.
4. **Single flight per account, with backoff.** Two concurrent password logins
   from one IP is the pattern risk control reacts to; a failed attempt is not
   retried for 5 minutes, so neither a sweep nor request traffic can hammer it.
5. **A human gate ends the automatic attempt.** New-device OTP, captcha or the
   cross-region demand: the harvested identity is persisted, the account is
   disabled with the reason (non-permanently), and nothing is dispatched — an SMS
   is only ever spent on purpose (ADR 0013). The operator finishes through
   `/v1/admin/otp/*`.
6. **Observability**: `/v1/admin/pool` gains a `recovery` block (`in_flight`,
   `attempts`, `last_error`) — a revocation healing itself must be visible.

Also fixed while testing this: a `serviceLoginAuth2` answer with HTTP 5xx/429 is
now a transient failure instead of a parseable login result (a proxy's JSON error
page must not be mistaken for a successful login).

## Consequences

- A revocation costs one failed request and a few seconds, instead of an operator
  and an SMS — as long as the device trust holds. If it is lost (new egress IP,
  cleared device cookies), recovery stops at the gate and one operator code is
  needed again; that path is unchanged and visible in the pool view.
- Clients should retry once on `account_action` during recovery; the error message
  says so explicitly.
- The recovery logs its start, outcome and failures. A silent failure would be a
  deployment that looks healthy while the account is dead.
