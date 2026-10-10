# 0036 — The lease window moves with the upstream, because silence is the failure

Status: accepted (2026-10-10). Amends ADR 0019 (the deadline half); ADR 0019's lease
model, reaper and per-request capacity rules stand.

## Context

A web search through the public entry died at exactly 120.4s with `504` and the gateway
logged

```
chat: our own bound fired while account <account-id> was serving (silence window 120s, held 120s): ...
```

while the platform was working normally: the same question answered in 23.8s and 50.6s
locally and 29.8s through the public entry minutes later. The knobs at the time were both
fixed wall clocks — `MIMO2API_REQUEST_TIMEOUT_MS` (120s, client-facing) and
`MIMO2API_LEASE_TTL_S` (120s, applied as a per-turn `context.WithTimeout`) — and the only
part of the stack that followed the stream was the SSE body watchdog
(`MIMO2API_SSE_IDLE_TIMEOUT_S`, 180s, reset by any byte), which the two fixed gates always
beat to the punch. So a turn that was *alive* — a search fetching pages, which is why its
prompt grows to 15k–20k tokens — was indistinguishable from a turn that was *dead*.

The owner's requirement, stated directly: "上游流式有响应回来，我们就一直移动啊，不是固定的".

Two more facts shaped the fix:

- On the public path the client did not even see our error: the failing response was
  Cloudflare's own `504` page (`server: cloudflare`, body `error code: 504`), because a
  non-streaming request sends no bytes until it is done, so the intermediary gave up
  first. A wall clock shorter than the intermediary's patience is the only way our named
  error wins, and a wall clock longer than it is invisible.
- The two silences are not the same event. Silence *before* any byte means the platform
  has not started; silence *after* it started means the stream died. The first is our
  patience, the second is evidence about the account (POOLATTR-012 deliberately cools the
  account for it).

## Decision

1. **`MIMO2API_LEASE_TTL_S` (default 120s) is a silence window, and it moves.** Every byte
   from the upstream resets it, through the SSE reader (`idleWatchedReader.onActivity`)
   into `Lease.Touch()`. The chat pipeline passes the same number into the turn
   (`ChatRequest.IdleTimeout`), so one value bounds both the turn and the slot it holds.
2. **The pool reaper reaps for silence, not for age** (`now - lease.lastActivity > window`,
   falling back to the claim time). A slow request keeps its slot; a wedged one loses it on
   time, and `ArmExpiry` still takes its in-flight call down.
3. **Silence before any byte is our own bound.** A pre-first-byte guard (`time.AfterFunc`
   over `Chat`, cancelled as soon as response headers arrive) closes the window the body
   watchdog cannot see, and it surfaces as **`ErrSilentUpstream`** — distinct from
   `ErrIdleStall` on purpose: `isGatewayDeadline` answers it with our own `504 chat_timeout`
   naming the window and leaves the account alone, while a mid-stream stall keeps its
   transport classification and cools the account.
4. **`MIMO2API_REQUEST_TIMEOUT_MS` defaults to 0 (no wall clock).** It is the client's
   patience, not a judgement about the upstream; an operator sets it when an intermediary
   needs a ceiling to relay. When set, it is the only wall clock in the stack.
5. **The upstream HTTP client timeout follows the same rule** (`UpstreamTimeout()`): with no
   operator budget it is 0 (none), because the derived `leaseTTL+30s` would otherwise have
   become exactly the wall clock this ADR removes; with a budget it is that budget plus a
   30s margin so our named 504 wins the race against a bare i/o timeout.
6. **No new knob.** The two existing variables now mean what their names say, and neither
   needs a per-request-class branch: "is it alive?" is answered by the stream itself, for
   search and chat alike.

## Consequences

- A search that takes 200s while streaming steady bytes completes. A search that says
  nothing for 120s still fails, with our own error naming the window and no cooldown —
  which is the honest answer: the platform never started answering.
- Capacity can now be held longer by a genuinely slow turn (the window is reset, not
  extended by grace), so `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT` is the concurrency lever an
  operator should revisit if long searches become common. The trade is deliberate: a slot
  held by a working turn is doing work; a slot held by a fixed clock was only waiting.
- On the public path a long **non-streaming** request can still be cut by the intermediary
  before we answer, because nothing is on the wire until the end. Clients that want the
  answer, not the intermediary's error page, should use `stream: true` — documented in the
  README's endpoint table.
- The console log now reports two different numbers for a bound that fires: how long the
  upstream has been silent, and how long the request has held the slot. They used to be
  the same number, which is precisely why the bug was invisible in the logs.

## Alternatives rejected

- **Raise both thresholds to 300s.** Still a wall clock, only later; and the public path
  would still lose to the intermediary's ~100s read timeout.
- **A per-request-class budget (search gets more).** It encodes a guess about which turns
  are slow. The stream already knows: silence is the only thing that separates "slow" from
  "dead", and it costs nothing to ask it.
- **Keep `leaseTTL+30s` as the HTTP client timeout.** It silently re-imposes the wall clock
  one layer down, where the log would blame the transport instead of naming the window.
- **Make the stall cooldown-free too.** A stream that dies mid-answer *is* evidence about
  the account; ADR 0019's own-clock rule is about our patience, not about a broken stream.
