# 0009 — Sessions are streaming-only, explicitly identified when possible, and verified before chaining

Date: 2026-10-09
Status: accepted

## Context

The gateway decides per request which upstream conversation to continue. Until
now that decision was made *only* from message content: `adapter.Fingerprint`
hashes the last ≤5 non-system messages with each content truncated to 200 runes,
`Continuation` matches it against the message frontier of every stored entry,
and the store scans all entries picking the newest match
(`internal/adapter/fingerprint.go`, `internal/session/store.go`).

Four consequences followed, all of them silent:

- two callers whose first five non-system messages match (shared template,
  monitoring probe, copied integration test) are indistinguishable, and
  "newest wins" can hand one caller another's dialogue frontier;
- a concurrent request for the same head reports a continuation too, so two
  requests can interleave into one upstream dialogue and the session quietly
  becomes a fork;
- identity is lossy in both directions — 200-rune truncation can make different
  conversations hash equal, while a client that rewrites history (context
  trimming, summarisation — routine for coding agents) loses its own session;
- non-streaming requests were excluded from chaining altogether.

`Rfym21/Qwen2API` was studied as prior art. Its upstream is stateless — every
request mints a fresh upstream chat and continuity comes from replaying the
whole history — so its session key exists only to cache the re-uploaded history
prefix. Two of its ideas transfer, and one of its properties does not.

## Decision

1. **Chaining is a streaming-only capability.** `stream:false` is one-shot: it
   resolves no identity, reads nothing from the session store, writes nothing to
   it, and returns no session header. A session id supplied to a one-shot
   request is ignored with a debug note. Today's `NOSTICK-003` becomes the
   permanent contract instead of a default we intend to relax — a one-shot turn
   must not advance or fork a chain it does not own.
2. **Identity follows a ladder**, resolved once per streaming request:
   explicit `X-Mimo-Session-Id` header → the `session_id` body field → a derived
   key `sha256(credential_id ‖ model ‖ system ‖ tools ‖ first non-system
   message)` → a fresh minted id, which is returned in the response header so
   the caller can pin it. The OpenAI `user` field is **not** an identity source;
   it is usually a per-request nonce.
3. **Session state is scoped to the calling credential.** An entry created under
   one API key is not resolvable by another, so identical prompts from two
   tenants cannot collide and a forged id cannot reach a foreign chain.
4. **Reuse requires verification, never just a key match.** A request chains only
   when the canonical hash of the *whole* stored frontier is a prefix of the
   current history and the non-system message count strictly increased. A key
   collision may cost a rebuild; it can never attach another conversation's
   chain. Verification covers the full frontier rather than a 5-message window,
   so editing history near the start invalidates the session (replay) instead of
   silently matching a lookalike.
5. **Concurrent same-session requests wait briefly, then fork.** The second
   streaming request for a session waits up to `MIMO2API_SESSION_WAIT_MS`
   (default 2000) for the chain to free, then forks: new session id, fresh
   upstream conversation, reported in `X-Mimo-Session-Mode` and the debug log.
   The original chain is left intact for its owner.
6. **Session state is volatile.** It lives in process memory. A restart costs
   one full replay per live session; a client that re-pins its own session id
   re-establishes its session on the next turn. No snapshot file — a stale
   snapshot could resurrect a conversation the upstream has already dropped.
7. **Lookup is keyed, not scanned**, so cost is independent of the number of
   live sessions.

## Consequences

- Cross-tenant leakage and silent forks become structurally impossible rather
  than unlikely, because reuse is gated on verification and on a
  credential-scoped key.
- Clients gain a way to *own* their session (`X-Mimo-Session-Id` in and out),
  which is what makes the round trip in REQ-16 testable.
- One deliberate regression: a client that rewrites its own history no longer
  chains (the old lossy window sometimes did). Correctness over an accidental
  match — the fallback is a full replay, invisible to the client except in
  token cost.
- Chaining stays unreachable for non-streaming clients, which was the cheaper
  option for them; the owner chose the simpler one-shot contract instead, and
  this ADR is the record of that trade.
- The previous identity helpers (`Fingerprint`/`Continuation`) remain as the
  compatibility path for the deployed semantics the STICKY tests describe, but
  they no longer decide identity on their own.

## Rejected alternatives

- **Copy Qwen2API wholesale.** Its session exists to cache a replay artifact;
  MiMo's upstream has real server-side conversation state, so our session
  decides chaining versus replay — a decision with different guarantees to make.
- **Accept the OpenAI `user` field as an identity.** Convenient, but it is a
  caller-defined field commonly carrying a per-request nonce; two different
  turns of one conversation would then look like two sessions.
- **Let non-streaming requests create sessions.** Convenient for batch callers,
  rejected because a one-shot request has no way to receive the id back and
  advance the chain honestly.
- **Persist sessions across restarts.** Buys a warm restart at the cost of
  stale state that can point at a conversation the upstream has dropped.
- **Auto-detect forks by comparing upstream dialogue ids.** Policy with a
  reported outcome is auditable; inference from ids is guesswork.
