# 0017. Chat has no streaming path and no session store

Date: 2026-10-09
Status: accepted (superseded in part 2026-10-10 by
[0034](0034-streaming-chat-is-served-again.md): the client-facing SSE path is
served again. Everything below about the conversation store, the session header and
the fork policy still stands.)
Supersedes: ADR 0016 §2 (the `MIMO2API_ALLOW_STREAM` switch), ADR 0009 (sessions for
chaining). Closes the "kept, tested, documented" caveat ADR 0016 left open.

## Context

ADR 0016 turned streaming *off* with a switch: `stream: true` answered
`400 stream_unsupported`, while the SSE emission path, the keepalive, the
conversation store and the sticky/fork machinery stayed in the tree behind it.
The owner's reaction settled what to do with that half-state: "不是默认关，而是可以
移除了" — a disabled feature that no client uses is not an option to keep, it is
code that has to stay correct (and covered by tests, and documented) for nobody.

Two facts made the removal cheap rather than risky:

- **Nothing in the deployment used it.** The only client-facing path in production
  was `stream: false`, which ADR 0009 had already defined as one-shot: no session
  identity, no store read or write, no `X-Mimo-Session-Id`. The store's entire
  client-facing surface was reachable from the path that was switched off.
- **The upstream is still SSE.** Removing our *output* streaming does not remove
  the upstream parser: MiMo answers every chat POST with `event:message` frames,
  and the client still collects them. `internal/mimo/sse.go` (parser, thinking
  splitter, usage merge, dialogId) is load-bearing and stays.

## Decision

1. **The streaming output path is deleted, not switched off.** `MIMO2API_ALLOW_STREAM`,
   the `allowStream` field, the SSE writer, the keepalive ticker and the
   pre-first-byte commit boundary are gone. A request with `stream: true` is
   refused with `400 stream_unsupported`, naming the fix, before any pool or
   account work. The refusal is now structurally unconditional — there is no
   configuration that turns it off.
2. **The conversation store is deleted.** `internal/session/`, the sticky
   helper path it served, the `X-Mimo-Session-Id` contract, the fork policy and
   their environment variables
   (`MIMO2API_STICKY_TTL_S`, `MIMO2API_STICKY_MAX`, `MIMO2API_KEEPALIVE_INTERVAL_MS`,
   `MIMO2API_SESSION_WAIT_MS`, `MIMO2API_SESSION_FORK_POLICY`) are gone. Every
   request is a **full replay** into a fresh upstream conversation
   (`adapter.PlanFullReplay`), which was already the non-streaming behaviour.
   *Note*: `MIMO2API_SESSION_REFRESH_HOURS` stays — that is the credential
   keepalive (ADR 0014), not the conversation store.
3. **One delivery point.** The JSON body is written exactly once, at the end of
   the turn; every failure is pre-response and therefore retryable, and the
   client never sees a half-written answer. `finishTurn` records success and
   releases the lease.
4. **Only chat changed.** Web search still runs, and its citations ride in the
   JSON body (`adapter.ShapeCompletion(..., cits...)`). TTS and ASR never touched
   the switch; the shutdown of the SSE path leaves them untouched.

## Consequences

- **A smaller, honest surface**: seven environment variables fewer, one internal
  package fewer, no write path into conversation state, and no possibility of a
  half-delivered answer.
- **Thought-off reasoning no longer leaks.** The deleted streaming path skipped
  reasoning pieces when the client had not asked for thinking; the non-streaming
  path included `reasoning_content` unconditionally, which a converted case
  (API-010) caught. `handleChat` now strips it explicitly (`!p.thinking`).
- **Retired cases, not silently deleted.** The SSE/session test material is listed
  as retired with its replacement in `docs/TDD.md` §2.1.3: the client-facing cases
  (API-002/019/021, DEBUG-001, LIVE-004/005/010, the STICKY/SESS waves) are gone,
  and their subject matter is covered by non-streaming cases where the behaviour
  still exists (API-001 for the answer, the new API-021 mid-body failure case for
  error delivery, API-010 for reasoning, LIVE-011 for replay depth).
- **The upstream price is unchanged** and now unconditional: one collected answer
  per request, so a client that wanted incremental tokens has to change client or
  accept the full answer (the owner's decision: nobody in this deployment does).
- **The reverse proxy keeps its streaming-safe settings** (`flush_interval -1`,
  zeroed read/write timeouts) — harmless for one JSON body and not worth a
  config-change risk on a working site.
- **Reviving streaming is a real change, not a flag**: the SSE writer, the
  keepalive and the session store would have to come back from git history, plus
  the cases that pinned them. Recorded here so the cost is visible before anyone
  proposes it.
