# 0032 — Every error carries a code, and one name means one thing

Status: accepted (2026-10-10)

## Context

A live pass over the deployed surface found the error body inconsistent with the rule
this project already applies to responses: `apiError.Code` carried `omitempty`, so
every failure without a code of its own lost the key entirely. OpenAI's error object
always carries `code`, and the gateway's own documentation claimed the body was
`{"error":{"message","type","param","code"}}` — but a malformed body, an unknown
model and a few admin paths answered with three keys or fewer. A client switching on
`code` had to branch on its absence first, and `paramForCode` was keyed by a code that
was never set on those paths.

Measured on the deployed entry before the fix:

```
POST /v1/chat/completions {"messages":[]}  → 400 {"error":{"message":"malformed request body","type":"invalid_request_error","param":null}}
POST /v1/chat/completions {"model":"gpt-4o"} → 404 {"error":{"message":"model \"gpt-4o\" not found; ...","type":"invalid_request_error","param":null}}
GET  /v1/models/gpt-4o                       → 404 {"error":{"message":"...","type":"invalid_request_error"}}   # two keys
```

## Decision

1. **The key is always present.** `apiError.Code` is a pointer with no `omitempty`, so
   it serialises as `null` rather than disappearing — the same treatment
   `message.refusal` and `choices[0].logprobs` get.
2. **A code is information, so it is never empty.** `null` is reserved for a failure
   that genuinely has no name; every code path this surface can reach now passes a real
   one. The four that did not, and their names:

   | failure | code |
   |---|---|
   | unparseable / empty body (chat, audio, admin OTP, admin consent) | `malformed_request_body` |
   | no sendable user message | `no_user_message` |
   | history over the replay cap | `too_many_turns` |
   | a model id the deployment does not serve (chat and `/v1/models/<id>`) | `model_not_found` (OpenAI's own code, so a client already switching on it keeps working) |
   | the account disappeared from the store mid-OTP | `account_not_found` |

3. **One name means one thing.** The chat surface's malformed-body code was empty while
   the audio surface already answered `malformed_request_body` for the same failure;
   now all four surfaces share the name. A dead branch that answered
   `unsupported content part` with no code was removed rather than given a name: nothing
   raised that sentinel (unknown part types are decoded permissively), so naming it
   would have documented a path that cannot happen.

## Consequences

- A client can switch on `code` for every failure. `param` keeps its own rule: it names
  the offending field, and is `null` for a failure that is not about one field — that
  one is genuinely not information for those cases.
- `codeOf` stays as the single place that decides what the wire value is, so a future
  path that really has no code emits `null` rather than an empty string.
- TDD OSTD-013 pins both halves (four keys present, code non-null and equal to the
  expected name) across nine failures, including the OTP/admin surfaces that had the
  same hole.
