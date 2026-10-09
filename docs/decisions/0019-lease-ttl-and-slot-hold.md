# 0019. A lease is per-request capacity with a TTL — not stickiness, and not a 30-minute hold

Date: 2026-10-09
Status: accepted
Related: ADR 0009 (session identity, retired by 0017), ADR 0017 (no streaming, no
session store), ADR 0015 (measured limits)

## Context

The owner asked why a request needs a 300-second timeout at all: the gateway turns
the upstream's SSE into one non-streaming body, so as soon as the upstream answers,
the account's work is done and its capacity can go back to the pool.

Investigating found the situation was worse than the question assumed:

1. **The timeout did not exist.** `MIMO2API_REQUEST_TIMEOUT_MS` (default 300s) had
   an accessor that **only tests called** — no production code read it. The real
   bounds were a hardcoded `http.Client{Timeout: 30 * time.Minute}` in
   `internal/mimo/client.go` plus the SSE idle watchdog
   (`MIMO2API_SSE_IDLE_TIMEOUT_S`, default 180s, which resets on any byte).
2. **The lease had no TTL.** A lease is one of
   `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT` (default 2) in-flight slots on one account,
   held from `Acquire` until `Release`. A connection that trickled a byte now and
   then could therefore hold a slot for up to 30 minutes, and two such requests
   exhaust an account — after which every new request queues
   (`MIMO2API_QUEUE_WAIT_S`, default 15s) and then fails.
3. **The slot was released after the client had the body**: on the final turn the
   response was written first and `finishTurn` ran afterwards, so a slow client
   held account capacity.

## Decision

**A lease is capacity for one request, never stickiness, and it carries a TTL.**

1. `MIMO2API_REQUEST_TIMEOUT_MS` is now wired: it is the whole client-facing budget
   for a chat request. Default lowered 300s → **120s**, which covers a measured
   single turn (6–40s; long ones ~100s) and sits inside the ~100s origin-read
   timeout of a CDN in front of a hosted deployment, so the client gets our own
   `504 chat_timeout` with numbers instead of an intermediary's 524.
2. `MIMO2API_LEASE_TTL_S` (default **120s**) bounds one upstream turn. It is
   applied as a per-turn context deadline, and the lease carries the same TTL for
   the pool's reaper.
3. The pool gained a **slot reaper** (`Pool.ReapExpiredSlots`, swept every 5s by
   the janitor): slots past their TTL are force-released and their in-flight
   upstream call is cancelled through `Lease.ArmExpiry`. The request path bounds
   each turn itself; the reaper exists for the case the path cannot cover — a
   wedged or panicking handler — because that failure is silent and shrinks the
   pool permanently.
4. `Lease.Release` is **idempotent**, and the request path adds a deferred
   backstop release, so early release (below), the reaper, the disconnect path and
   the normal path can all release without double-counting capacity.
5. The slot goes back **before the client is written to**: the completion is
   encoded into a buffer, the turn is finished (slot released), and only then are
   the headers and body written. A client that reads slowly no longer occupies
   account capacity, and a marshal failure is still a clean pre-response error.
6. `MIMO2API_SLOT_HOLD` chooses when a request hands its slot back:
   - `last-byte` (**default**): the slot spans the upstream generation.
   - `first-byte`: the slot is released the moment the upstream's first SSE event
     arrives — the owner's original proposal — so the generation itself does not
     occupy capacity and one account can serve more concurrent conversations.
7. The upstream HTTP client timeout is **derived** (`LeaseTTL + 30s`) instead of a
   hardcoded 30 minutes, and the client is built from that value in `main.go`.
8. A gateway deadline is **not** the account's fault: it is answered as
   `504 api_error chat_timeout` naming the knob, with **no retry and no cooldown**.
   Only the SSE idle-stall watchdog (a real upstream stream problem) still cools.

## What the owner's question clarified, and what it did not

- Correct: the lease is per request, not per conversation. Nothing about it is
  sticky, and different concurrent requests may share one account up to
  `MIMO2API_MAX_INFLIGHT_PER_ACCOUNT` — with a single account, that is the
  supported form of "one account, several conversations at once".
- Not changed: within **one** request, all replayed turns must stay on the same
  account, because `previousDialogueId` chaining is account-scoped (SDD §615). A
  lease pins the account for the request; the `first-byte` policy gives back the
  *capacity* early without moving that pin. Turning the alternative into a policy
  would mean resending history to a different account mid-request — more upstream
  calls, not fewer.

## Consequences

- A wedged request can no longer hold an account for 30 minutes; the worst case is
  one TTL, and the reaper logs what it reclaimed (`pool: reaped slot account=…`).
- `first-byte` is **opt-in** because the trade is account risk, not code risk:
  releasing the slot at the first byte raises per-account concurrency while a model
  generates, which is exactly what risk control observes. The default keeps the
  conservative reading until a bounded live test says otherwise.
- Requests that legitimately outlive the TTL now fail with a **504 naming the
  budget** instead of hanging: with the current non-streaming, full-replay design a
  20-turn request takes ~258s measured, so it exceeds the 120s default. That is
  the intended trade — the honest failure replaces an unbounded wait — and the
  documented fix is to send fewer messages per request (or raise the knob), with
  cross-request `dialogId` reuse as the real solution for long conversations.
- The SSE idle watchdog (180s) is now largely superseded for chat by the 120s turn
  TTL; it still governs audio flows and any stream the TTL cannot bound.

## Deployment constraint: one process owns the pool

The in-flight counters and the per-account slots live in the process, while the
account store may be shared (Upstash, ADR 0018). **One process is the supported
deployment**, and that is a constraint rather than an oversight: two gateways sharing
one store would each believe they hold an account's two slots, so a one-account pool
could serve four concurrent turns and the account — not the gateway — would take the
consequences from the platform. Scaling out is possible; the fix is to move the slot
bookkeeping into the shared store (a per-account counter with the same TTL semantics)
or to put a single-writer lock in front of the pool. It is deliberately not built:
the deployment is one container on one host (ADR 0016), and a distributed lock nobody
uses is worse to carry than a written constraint.

## Alternatives considered

- **Keep a fixed 300s upstream timeout and no TTL.** Rejected: it is what allows a
  wedged socket to consume the whole account for half an hour, and it left the
  client-facing knob dead.
- **Release the slot at the first byte by default.** Rejected for now (account
  safety first); shipped as a switch.
- **Make the reaper release the slot without cancelling the upstream call.**
  Rejected: it would let two goroutines read the same account's stream and
  mis-attribute failures. A reclaimed slot cancels the call it was holding.
- **Treat a gateway deadline as a transport failure (cool the account).**
  Rejected: our clock is not evidence about the account, and it would silently
  shrink the pool whenever clients send oversized replays.
