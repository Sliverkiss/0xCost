# ADR 0013 — Device identity is state, and an SMS is never spent implicitly

Status: accepted (2026-10-09). Supersedes nothing; amends SDD §2.2/§2.4 and the
default of `MIMO2API_OTP_AUTO`.

## Context

The account's passToken died (code 70016) and every recovery attempt stopped at
the upstream's new-device verification: `code=0`, `securityStatus=128`, empty
`location`, `notificationUrl` set. Reversing the account front end and a free
three-attempt live experiment (no code sent) showed why:

- the passport issues a **device anchor** — `deviceId=wb_<uuid>` (390 days),
  `pass_ua=web`, `uLocale=zh_CN`, plus a `JSESSIONID` binding the attempt — on the
  **login page navigation**, and it mints a fresh one for a cookieless request;
- our login chain never loaded that page: `serviceLoginAuth2` went out with three
  headers and no cookie context, so every attempt was new hardware and the
  "remember this device" flag (`trust`, which we already send as `true`) had
  nothing to attach to. Replaying the cookies alone does not remove the demand,
  so continuity is the mechanism;
- an attempt that ends in the demand **threw the freshly issued identity away**,
  so the next attempt presented a different device and the operator's pending
  code no longer belonged to any device we claimed. Live cost: the next code send
  was refused with `code=20024` (SMS rate limit) and the session that held the
  verification was gone with the process.

## Decision

1. **The device identity is persisted per account** (`accounts.Account.device`)
   and replayed on every login and renewal hop; whatever the passport issues in
   `Set-Cookie` is harvested back (server value wins on collision, a silent
   server never wipes a stored value).
2. **A login attempt persists the identity it harvested even when it fails** with
   an OTP demand or a captcha, so retries are the same device.
3. **The passport surface is sent as the browser sends it**: the login page is
   navigated to first (that is the anchor), the form POST carries Chrome's form
   header set, the identity/STS hops carry the SPA's XHR set — instead of the
   three headers used before. The aistudio surface already had its full set.
4. **`MIMO2API_OTP_AUTO` defaults to false.** Starting a challenge sends a real
   SMS and only a human can finish it, so the dispatch is an explicit operator
   decision. The previous default of true meant any login attempt — including the
   startup catalog warm-up — silently spent a code.

## Consequences

- Recovery after a dead credential becomes: operator enables the SMS dispatch
  for one attempt, one code is spent, the login completes, and the device
  identity that made it work is on disk for every later attempt.
- A login now performs one extra free GET (the login page) per attempt.
- Unattended deployments will not send SMS codes at all; that is intended, since
  an unattended process cannot enter one.
- The retry after an aborted attempt must run inside one process lifetime, or the
  challenge (held in memory) dies with it. Persisting the pending challenge is
  the next step if operator restarts turn out to be common.
