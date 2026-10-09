# 0003 — Compose the session from the whole landing chain's cookies

Date: 2026-10-09
Status: accepted

## Context

After identity verification the passport answers with a `location`, and the
session cookies are **not** on the first hop of that location:

```
/identity/result/check →302→ /pass/serviceLoginAuth2/end →302→
<upstream>/sts?sign=… →307→ /open-apis/user/mi/get →200
```

`xiaomichatbot_serviceToken`, `xiaomichatbot_ph` and `passToken` are planted
along the way — the session ones at the upstream hop. The previous
implementation followed a single hop and then re-asked
`/pass/serviceLogin` twice, which produced `code=70016 登录验证失败` on the
live service even though verification had just succeeded.

## Decision

- Walk the landing chain to its end with manual redirects, accumulating every
  `Set-Cookie` into **the caller's jar** (`followChain` → `stsExchangeWithJar`).
- Build the cookie string from that jar, in the browser's order and quoting:
  `cUserId; userId; serviceToken + xiaomichatbot_serviceToken (quoted);
  xiaomichatbot_slh (quoted); xiaomichatbot_ph (quoted)`.
- A `passToken`/`userId` found nowhere is not fatal: the session cookie is the
  deliverable and the caller keeps its stored identity.

## Consequences

- One code path for both the OTP completion and the renewal.
- The jar must be threaded through, not copied: collecting into a throwaway
  map was the actual bug behind "verified but no passToken" (`cfc286c`).
- `serviceToken=` (unquoted) is kept alongside the quoted spelling because the
  upstream accepts both and older captures rely on it.

## Evidence

- Live landing chain and cookie subset tests (`docs/login-chain-reverse.md` §1-§2).
- `internal/mimo/consent_test.go` OTP-CHAIN-001 (no `/pass/serviceLogin`
  fallback is used) and OTP-CHAIN-002 (session without a renewal token).
- `cmd/stsprobe` renews live from a stored `passToken` and validates the
  composed session.
