# 0033 — A refusal keeps the platform's own words

Status: accepted (2026-10-10)

## Context

A live pass over the deployed entry sent an image the platform's moderation did not
like. The client saw:

```
400 {"error":{"message":"upstream content filter rejected the query",
              "type":"invalid_request_error","param":null,"code":"content_filter"}}
```

which is our sentence, not the platform's. The upstream had actually said more, twice
over, and both halves were dropped on the way out:

- the SSE frame is `event:sensitive_query`, and **the event name is the verdict** — the
  platform distinguishes `sensitive_query` from `sensitive_title`;
- the frame's `data` carries a payload with `content` (the platform's own wording, e.g.
  `topic not allowed`), which `handleEvent` parsed only for `message`/`finish` events.

Two neighbouring paths had the same shape: an in-band platform throttle
(`服务器繁忙，请稍后再试`) was reported as `upstream is busy; retry shortly`, and a
channel-level model refusal (`模型名称错误`) as a sentence of ours with the upstream
text double-prefixed through `err.Error()`.

## Decision

**A failure the upstream judged is reported with the upstream's own words, verbatim.**

1. `ErrContentFilter` keeps both halves — `Reason` (the event name) and `Message` (the
   frame's `content` payload) — and `UpstreamMessage` gives every other layer one
   accessor for the platform's wording.
2. `MapError` composes `message` as our finding plus the platform's sentence after a
   colon, skipping the colon when the payload was empty:
   `content filtered by upstream (sensitive_query): topic not allowed`.
3. Status and `code` stay ours. A client switches on those; prose is for the human
   reading the log, so it is never translated, truncated inside the sentence, or
   reworded.

This reverses the earlier instinct that an upstream body is "not something to return to
a client" (the comment on the `upErr` branch): for a *refusal the platform made* the
text is the diagnosis, and withholding it leaves a caller unable to tell a moderation
judgement from a malformed request. That rule still holds for the **5xx transport
bodies** (`upErr.Body`), which stay debug-logged: they describe our/their plumbing and
can carry internal audit traces, not the client's request.

## Consequences

- A client can quote the platform when it escalates, and can act on the event name
  (`sensitive_query` vs `sensitive_title` are different problems).
- The platform's wording is untrusted text on the wire — it is JSON-escaped like any
  other string (ADR 0030: client and upstream strings are data, never structure).
- Tests: ERRMAP-009, BUSY-002 and MODEL-001 assert the upstream text survives to the
  client; `internal/mimo` unit-covers `UpstreamMessage` across all three error shapes
  and the empty-payload case.
