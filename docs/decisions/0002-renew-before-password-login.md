# 0002 — Renew from the passToken before falling back to the password login

Date: 2026-10-09
Status: accepted

## Context

When an account has no cached session, the gateway needs one. The password
login re-runs the whole chain — cross-region consent, identity verification,
and (from a new device) a new-device OTP code that **only a human can
supply**. During the live acceptance work a burned OTP left the account with a
valid `passToken` but no session, and the gateway's answer was to demand
another SMS.

A renewal (passport `serviceLogin` with the `passToken` cookie → STS
exchange) re-mints the session with no human involvement at all, and TDD
LIVE-006 pins exactly that behaviour.

## Decision

`creds()` tries the renewal first whenever `user_id` + `pass_token` are
present. Only if the renewal fails does it fall back to
`Login(phone, password)` — and the failure is logged with its reason.

## Consequences

- Routine session loss is invisible to the operator; no SMS is spent.
- A truly dead passToken (`code=70016`) still needs a full re-login, so the
  fallback is kept rather than deleted (TDD RENEW-003).
- Sessions harvested by OTP or password login must never blank the stored
  identity: a chain that plants no `passToken` leaves the existing one in
  place, otherwise the *next* renewal would have nothing to renew from.

## Evidence

- `internal/api/renew_preference_test.go` — RENEW-PREF-001 (renewal used, zero
  auth2 calls) and RENEW-PREF-002 (dead passToken → password fallback).
- LIVE-006 passes live: a deleted session was restored by exactly one STS
  renewal in 7.8 s (`docs/live-findings.md`).
