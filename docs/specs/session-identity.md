# Spec — Session identity and isolation

Status: **accepted** — the owner's decisions are recorded in §9 and ADR
[0009](../decisions/0009-sessions-are-streaming-only-and-verified.md);
implementation follows the SESS cases in `docs/TDD.md` §3.5.1
Applies to: `/v1/chat/completions` conversation chaining, the sticky store, and
everything that keys per-conversation state
Author: Hermes (for review by the project owner)

---

## 1. What "session" means in this gateway

A **session** is the gateway's answer to one question:

> Which upstream conversation does this request continue?

MiMo's web protocol has real server-side conversation state: a `conversationId`
entity plus a `dialogId` chain (`previousDialogueId`), both created by the
Studio and reused across turns. Continuing a session is therefore *cheaper and
better* than replaying history: the upstream keeps the context, the gateway
sends only the delta, and the previous turn's dialogue id anchors the chain.

A session is **not**:
- an OpenAI "conversation" object (the API has none),
- a stored message transcript (the gateway never stores content),
- a client credential (it must stay orthogonal to auth).

Three artifacts hang off a session and have different lifetimes:

| Artifact | Lives where | Lifetime | Cost if lost |
|---|---|---|---|
| Session identity (opaque key) | gateway memory | derived per request, cheap | none |
| Upstream conversation (`conversationId`) | upstream | unknown: GC across a 24 h gap is still an open live question (`live-findings.md` LIVE-010) | re-create + one full replay |
| Chain frontier (`previousDialogueId`) | gateway memory | session idle TTL | one full replay |

## 2. Current behaviour, and why it needs a design

Today identity is **content-derived only**:

- `adapter.Fingerprint` = SHA-256 over the last ≤5 non-system messages, each
  content truncated to 200 runes, first 16 bytes hex (ignores `system`) —
  `internal/adapter/fingerprint.go:19-52`.
- A request is a *continuation* when dropping 1..5 tail messages makes the
  prefix fingerprint equal the stored one — `fingerprint.go:66-89`.
- The store scans **every** entry per lookup, requires a strictly larger
  non-system message count (regeneration guard), picks the newest `UpdatedAt`
  on multiple matches, has a 30-minute idle TTL and a 500-entry LRU —
  `internal/session/store.go:74-112`.
- Non-streaming requests never consult the store at all (SDD §4.2, TDD
  `NOSTICK-003`).

Failure modes this produces, in the order they matter:

1. **Cross-tenant collision.** Identity ignores who is asking. Two clients whose
   first ≤5 non-system messages match — a shared system-prompt-less template, a
   monitoring probe, a copied integration test — are indistinguishable, and
   "newest wins" can hand one client's dialogue frontier to another.
2. **Fork blindness.** Two requests in flight with the same head (agent
   fan-out, retry, double-submit) both report a continuation; both mint a
   dialogue; whichever finishes last overwrites the frontier. The session
   silently becomes a fork, and the next turn chains onto an arbitrary branch.
3. **Lossy identity.** The 200-rune truncation plus the 5-message window means
   two different conversations can hash equal whenever their recent turns agree
   in the first 200 characters; conversely, a client that rewrites history
   (context-window trimming, summarisation — routine for coding agents) loses
   its session and pays a full replay.
4. **Half the traffic has no continuity at all.** Non-streaming clients always
   replay, so the same client alternating stream/non-stream flip-flops between
   a chained session and a fresh upstream conversation.
5. **Unindexed.** Every request hashes up to 5 candidate prefixes and compares
   against up to 500 entries — O(entries × window) per turn for something that
   is conceptually a hash-map hit.

## 3. Prior art: Rfym21/Qwen2API (reference, not a template)

Studied at commit `HEAD` of `github.com/Rfym21/Qwen2API` (2026-10-09). Relevant
facts:

- Its upstream (`chat.qwen.ai`) has **no reusable server-side conversation per
  session**: every request mints a fresh `chat_id` via `POST /api/v2/chats/new`
  (`src/utils/request.js:879-925`), and continuity is achieved by resending the
  whole history. `parentId` is used only when an agent runtime continues a chat.
- Session identity exists for a narrower purpose — caching an *uploaded history
  artifact* so the whole conversation is not re-parsed every turn:
  `buildContextPrefixKey({userId, model, system, tools, firstMessage})`
  (`src/utils/context-prefix-cache.js:26-34`), where `userId` comes from
  `metadata.user_id` — i.e. an **explicit client-supplied session id** when the
  client provides one (Claude Code puts its session id there), otherwise a
  content-derived fallback.
- Safety against collisions is a **verification, not a lookup assumption**:
  reuse requires the canonical hash of the whole stored prefix to match
  (`prefixMatches`, `:88-101`), so a colliding key can at worst overwrite an
  entry and force a re-bake — it cannot leak another conversation's state.
- Session state is **in-process memory** with an absolute TTL and an LRU cap,
  deliberately without timers (`:60-84`); Redis in that project is used for
  account/settings persistence only, never for session state.
- Account affinity is explicit: the cached entry carries the account, and a
  failover path exists per session.

**What we take:** the two ideas that are protocol-independent — an identity
ladder that prefers an *explicit* client session id and falls back to a derived
key; and *verify-before-reuse* so a key collision can never leak state.

**What we deliberately do differently:** Qwen2API's session is a cache key for a
replay artifact because its upstream is stateless. MiMo's upstream is
stateful, so our session decides **chaining versus replay** — a decision that
changes cost, latency and answer quality, and therefore needs stronger
guarantees (fork policy, non-stream parity, account migration) than a cache key
needs.

## 4. Requirements

Identity

- **REQ-1** Identity has a defined ladder: (a) an explicit session id from the
  client, (b) a derived key, (c) a fresh session. Each request resolves to
  exactly one, and the resolution is observable (REQ-12).
- **REQ-2** The explicit id arrives either as the `X-Mimo-Session-Id` header or
  as a body extension `session_id` — two carriers for one extension. Precedence
  is fixed (header first) and conflicting sources resolve deterministically
  without erroring. The OpenAI `user` field is **not** an identity source: its
  semantics are caller-defined and it is commonly a per-request nonce.
- **REQ-3** The derived key is namespaced by the calling credential (API key
  identity) so two tenants with byte-identical prompts cannot collide.
- **REQ-4** The derived key is a hash over bounded, canonical inputs — model,
  system preamble, tool schema, the first non-system message — never a stored
  copy of any content. Two clients that differ only in later turns still
  collide on the key by design, which is why REQ-5 exists.

Isolation

- **REQ-5** Reuse of a stored session requires **verification of the whole
  stored prefix** (per-message canonical hash over the frontier actually
  stored), not just a key match. A key collision may cost a rebuild; it must
  never attach another conversation's chain (INV-1).
- **REQ-6** The regeneration guard stays: a request whose history does not
  strictly extend the stored frontier is not a continuation.
- **REQ-7** A session never spans credentials: an entry created under one API
  key is never resolvable by another (INV-2).

Chaining and forks

- **REQ-8** Concurrency policy per session: the first in-flight request chains;
  a concurrent request for the same session either waits (bounded) or
  explicitly forks (fresh upstream conversation, new session id) — never
  silently interleaves into one upstream dialogue.
- **REQ-9** A fork is visible: the response (header/extension) and the debug
  log say the request forked, with the reason.
- **REQ-10** Non-streaming requests are **one-shot**: they resolve no session
  identity, read nothing from the session store, write nothing to it, and return
  no session header. A session id supplied on a non-streaming request is
  ignored (logged at debug, no error), because a one-shot turn must never
  advance or fork someone else's chain. Chaining is a streaming-only capability
  — this preserves today's `NOSTICK-003` rule permanently rather than relaxing
  it.

Account affinity

- **REQ-11** A session pins its account while that account is usable; when it
  is not, the session migrates to another account by minting a **new upstream
  conversation** and performing the replay path. Migration is logged and never
  presented as a continuation of the old chain.

Lifecycle, capacity, persistence

- **REQ-12** Lookup is a keyed index hit (O(1)-ish), with verification cost
  proportional to the stored frontier, not to the number of live sessions.
- **REQ-13** Lifecycle is explicit per artifact (§1): idle TTL for the chain
  frontier, absolute TTL for anything expensive, LRU cap, janitor sweep; a
  lookup on an expired entry is a miss, not an error.
- **REQ-14** Restart behaviour is declared and tested: either sessions survive
  (snapshot/restore) or they are documented as volatile with a defined
  cold-start experience. Silent loss is not acceptable.

Observability and privacy

- **REQ-15** Every session decision emits one debug line: resolved identity
  source (explicit/derived/fresh), decision (chain/replay/fork/migrate),
  session id, frontier sizes. No message content, no credentials.
- **REQ-16** The effective session id is returned to the client (response
  header) so a client can pin it explicitly on the next turn — this is the
  contract that makes REQ-1(a) usable.

Compatibility

- **REQ-17** With no new fields and no new headers, behaviour stays
  OpenAI-compatible and no worse than today; extensions are additive.
- **REQ-18** Config switches default to the behaviour we would defend in
  review, and every switch has a documented "why would I change this".

## 5. Invariants

- **INV-1** No request ever chains onto a frontier that the gateway cannot
  verify as its own. Verification failure ⇒ replay path, never a chain.
- **INV-2** Session state is scoped to (credential, session id). Cross-scope
  reads are impossible by construction.
- **INV-3** A session never stores message content; only hashes, ids, counts,
  and timestamps.
- **INV-4** One upstream conversation has at most one live chain writer at a
  time (REQ-8), so a fork cannot be silently merged.
- **INV-5** Losing session state (TTL, eviction, restart) degrades to a full
  replay — never to a wrong answer, an error, or a cross-session leak.
- **INV-6** Every decision is reconstructible from the debug log line alone.

## 6. Proposed design (to be elaborated in SDD §4.3)

**Identity ladder** (evaluated once per request):

1. Explicit: `X-Mimo-Session-Id` header, else the `session_id` body extension.
2. Derived: `sha256(credential_id ‖ model ‖ system ‖ tools ‖ first non-system
   message)`, opaque 32-hex.
3. Fresh: mint a new session id (returned in the response header).

Identity resolution happens on the **streaming path only** (REQ-10).

**Entry shape** (per credential scope):

```
SessionKey   string   // opaque; the ladder's result
AccountID    string   // pinned account (affinity)
ConversationID string // upstream conversation
PrevDialogID string   // chain frontier
FrontierHash string   // canonical hash of the stored message frontier
MsgCount     int      // regeneration guard
InFlight     int      // fork policy
CreatedAt / UpdatedAt int64
```

**Public ids carry their own scope.** An id we hand out is recognisable without
stored state: derived ids are `ms-d-<hash of the key>` and minted ids are
`ms-f-<token>`. A pinned derived id resolves to its session while the request's
derived inputs still agree (same model, system, tools, opening) — which is the
normal case, because a client resends its history. If a client pins a derived id
with a *rewritten* history, the pin is honoured as an opaque id of its own: a
stable, isolated session that is **never** a chain into the original (the
alternative is a silent lookalike match, which is the failure this design
exists to remove). A pinned `ms-f-` token resolves to the same fresh session
that produced it.

**Verification**: `FrontierHash` is the canonical hash of every non-system
message up to the frontier (role + content, length-prefixed). A request chains
only if the stored frontier equals a *prefix* of the current history and the
message count strictly increased. Unlike the current design, verification is
over the whole frontier rather than a truncated 5-message window, so history
edits near the start invalidate the session instead of silently matching.

**Fork policy**: while a session has an in-flight request, a second request for
the same session takes one of `wait` (bounded, default for short streams),
`fork` (default for agent fan-out), or `reject` — config-selectable, with the
default justified in review. A fork mints a new session id and a fresh upstream
conversation; the original chain stays intact for its owner.

**Non-streaming is one-shot (REQ-10)**: the identity ladder and the store are
reachable only from the streaming path. A non-streaming request always does a
full replay into a fresh upstream conversation, leaves the store untouched, and
gets no session header — a supplied session id is ignored and noted at debug.
`NOSTICK-003` stays the permanent contract, not a default we intend to relax:
a one-shot turn cannot be allowed to advance or fork a chain it does not own.

**Cost note**: chaining is the point of the design — the upstream really does
keep server-side context, verified over a 6-turn chain, and the fastchat channel
answered the same short question in 1.54s versus 5.77s on the baseline channel
(`docs/web-surface-reverse.md` §11.1–11.2). Today only streaming clients can
reach that path, which is what REQ-10 fixes.

## 7. Failure handling

| Condition | Behaviour | Client sees |
|---|---|---|
| No session (fresh) | Mint session + upstream conversation, replay | 200 |
| Verified continuation | Chain (`previousDialogueId` = frontier) | 200 |
| Verification failed | Replay into a new conversation, keep the session id | 200 (no error) |
| Session expired/evicted | Treated as fresh; the client's explicit id simply re-establishes it | 200 |
| Account unusable | Migrate: new conversation on another account, replay | 200 (+ debug log) |
| Concurrent request, policy `fork` | New session id + new conversation | 200 (+ header says fork) |
| Concurrent request, policy `wait` | Bounded wait, then fork if the bound expires | 200 |
| Upstream drops the conversation (`conversation not exist`) | Re-create once (existing behaviour), keep the chain frontier | 200 |

## 8. Non-goals

- Sharing session state across gateway instances (single-writer, in-process;
  revisit only if we run more than one replica).
- Persisting transcripts, titles, or any conversation content.
- Exposing an OpenAI "conversations" API surface.
- Auto-detecting forks by comparing upstream dialogue ids (we choose policy,
  not guesswork).
- Rewriting a client's history (trimming/summarising) on its behalf.

## 9. Decisions (owner, 2026-10-09)

1. **Explicit id surface**: *decided: both carriers accepted on input
   (`X-Mimo-Session-Id` header, `session_id` body field), header on output.*
2. **Concurrent same-session policy**: *decided: `wait` with a 2-second bound,
   then fork.* Interactive streams may wait briefly for their own chain; they
   never queue indefinitely, and a fork is always reported. Non-streaming is out
   of scope here — it has no session at all (REQ-10).
3. **Derived-key inputs**: include the tool schema? *Decided: yes — a changed
   tool set genuinely is a different agent session.*
4. **Restart durability**: *decided: sessions are volatile.* Session state is
   in-process; a restart costs one replay per live session, and a client that
   re-pins its own id re-establishes its session on the next turn. No snapshot
   file, so no stale snapshot can resurrect a conversation the upstream has
   already dropped.
5. **`user` field**: *decided: not an identity source* (it is usually a
   per-request nonce).

## 10. Acceptance plan

TDD cases (to be written into `docs/TDD.md` before implementation; ids fixed so
the RED phase is reviewable):

- Identity: `SESS-001` explicit header wins; `SESS-002` body field accepted;
  `SESS-003` `user` is not an identity source; `SESS-004` derived key includes
  the credential; `SESS-005` deterministic precedence on conflicting carriers.
- Isolation: `SESS-010` cross-credential lookup is impossible; `SESS-011`
  frontier mismatch ⇒ replay, never chain; `SESS-012` history rewrite ⇒ replay;
  `SESS-013` regeneration (identical history) ⇒ replay.
- Forks: `SESS-020` concurrent same-session ⇒ policy honoured; `SESS-021` fork
  minted id returned; `SESS-022` original chain intact after a fork.
- One-shot non-streaming: `SESS-030` a non-streaming request resolves no
  session, touches the store zero times and returns no session header;
  `SESS-031` a session id supplied to a non-streaming request is ignored
  (debug note, no error) and cannot advance a chain.
- Migration: `SESS-040` unusable account ⇒ new conversation + replay, logged.
- Lifecycle: `SESS-050` expiry ⇒ miss; `SESS-051` LRU eviction; `SESS-052`
  restart ⇒ defined behaviour; `SESS-053` lookup is keyed (no full scan; assert
  via instrumented counts).
- Observability: `SESS-060` one debug line per decision with the required
  fields; `SESS-061` response header carries the effective id; `SESS-062` no
  content or credentials in any session log line.

Live evidence required before claiming the milestone: one real chained session
through the gateway showing `previousDialogueId` reuse and a measured latency
delta versus the replay path, plus one migration case forced by disabling an
account.

## 11. References

- `internal/adapter/fingerprint.go:19-89` — current fingerprint/continuation.
- `internal/session/store.go:14-112` — current store, guard, TTL, LRU.
- `docs/SDD.md` §4.2 (selection paths), §4.3 (sticky store), §7.1 (retry class).
- `docs/TDD.md` `STICKY-001..013`, `NOSTICK-003` — today's test contract, with
  the section refs each case traces back to.
- `docs/live-findings.md` LIVE-010 — open question on upstream conversation GC
  across a 24 h gap (the phase-2 run is still pending).
- `docs/web-surface-reverse.md` §11.1–11.2 — `previousDialogueId` chaining is
  real (6 turns verified) and the measured fastchat-versus-baseline latency.
- Prior art: `github.com/Rfym21/Qwen2API`, `src/utils/context-prefix-cache.js`
  (session key + verify-before-reuse), `src/utils/request.js:879-925` (fresh
  upstream chat per request; stateless upstream).
