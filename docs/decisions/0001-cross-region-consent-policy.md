# 0001 — Auto-agree the cross-region consent, with an explicit operator override

Date: 2026-10-09
Status: accepted

## Context

From an egress IP outside mainland China the passport login chain does not go
straight to identity verification: it lands on the personal-data
cross-border-transfer consent page and the login stays blocked until
`POST /pass2/userCrossAgreement/update {userId, status:1, needClose:false}`
has been sent once for that chain (`docs/login-chain-reverse.md` §1 shape A).
Without it every login fails; the account never reaches the OTP step.

That consent is a *legal* acknowledgement, not a technical parameter, so the
service must not silently speak for its operator.

## Decision

- Default: `MIMO2API_CROSS_REGION_AGREE=true` — the gateway agrees on the
  operator's behalf, because the operator runs the account from that egress IP
  and otherwise the service cannot work at all.
- `MIMO2API_CROSS_REGION_AGREE=false` refuses to agree. The login surfaces the
  demand as an operator action and the account leaves rotation.
- Approving it explicitly stays available through the API
  (`POST /v1/admin/consent/agree {"account_id": …}`), which performs the
  consent for that one login and then continues the flow (including any
  new-device OTP demand).

## Consequences

- The default is a deliberate, documented acknowledgement made by the
  operator's configuration — not a hidden bypass. Rotating the egress IP is a
  new consent event (`docs/login-chain-reverse.md` §1.1).
- A second consent POST on a consumed chain answers `code=21301 认证失败`; the
  client treats that as an error, never as success.
- Operators who want the strict posture lose zero capability: the admin
  endpoint closes what used to be a dead end.

## Evidence

- Live chain walk and the observed consent response (`docs/login-chain-reverse.md` §1).
- Unit/integration coverage: `internal/mimo/consent_test.go` UC-001..007,
  `internal/api/consent_admin_test.go`.
