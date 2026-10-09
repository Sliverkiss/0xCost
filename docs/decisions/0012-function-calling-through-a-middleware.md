# 0012 — Function calling lives behind a middleware, reached through one path

Status: accepted (2026-10-09)
Related: SDD §5.6, TDD TOOL-001..008, `deploy/toolforge/` (PROVENANCE.md), spec REQ-3/INV-2

## Context

MiMo Studio has no function calling: the chat surface takes messages, a model id
and a few switches, and nothing in `bot/config` or the request schema carries
`tools`. Our API decoded `tools` from the start only to report it as dropped in
`X-Mimo-Warnings` (SDD §5.2, TDD API-008/009).

A separate project, ToolForge (`github.com/Sliverkiss/toolcall`), does exactly
what is missing: it strips `tools`, injects an XYML prompt protocol, parses the
tool call out of the model's text, and answers with standard `tool_calls`. It is
designed to sit in front of any OpenAI-compatible gateway — including this one.

## Decision

**Tool-bearing chat requests are forwarded to the middleware; nothing else is.**

1. **Only tool traffic crosses.** `needsToolLayer` fires on `tools`, `functions`,
   `tool_choice`, or a `role: "tool"` message (the turn answering a call). A plain
   chat request keeps today's path, today's latency and today's failure modes; if
   the middleware is down, plain chat does not care. `/v1/models`, the audio
   surface, the admin endpoints and `/healthz` never touch it.
2. **The middleware is called at `/v1/raw/chat/completions`.** The same chat
   handler, marked raw, so it never forwards again. Without that marker the round
   trip would be a loop. It is authenticated like every other path: it spends
   accounts.
3. **The caller's identity travels with the request.** Our copy of the middleware
   forwards the caller's `Authorization` and `X-Mimo-*` headers upstream
   (`key_passthrough`, `passthrough_headers`) — the one change we carry in it. This is not a convenience: session
   state is scoped per credential with a content-derived identity (spec REQ-3,
   INV-2, and the derivation includes model, system prompt and tools). A
   middleware that authenticated upstream with its own key would put every caller
   in one namespace, where two callers sending the same first message with the
   same tools derive the *same* session.
4. **The body is forwarded byte-for-byte.** Tool schemas and tool results keep
   the shape the caller sent; we do not re-encode what we do not interpret.
5. **A middleware failure is an error, not a downgrade.** A tool-bearing request
   that cannot reach the middleware gets `502 tool_layer_unavailable`. Answering
   it from the direct path would look like a model that ignored its tools — the
   caller cannot tell the difference, and would not know to retry.
6. **The middleware is vendored.** `deploy/toolforge/` holds its source (base
   commit `758a2c4`, runtime + tests + licence) and our change to it, tracked like
   the rest of our code; `PROVENANCE.md` records where it came from and what we
   changed. The deployment therefore needs no clone, no GitHub access and no
   credentials at build time, and the change is a normal code edit rather than a
   patch applied on top of somebody else's tree. The alternative — fetching the
   repo during the image build — also fails here: the base repo is private.

## Consequences

- The middleware is now ours to maintain: an upstream change has to be re-applied
  by hand (`PROVENANCE.md` says exactly which eight files differ).
- Tool calling is opt-in per deployment: `MIMO2API_TOOLFORGE_URL` empty (the
  default) means the feature does not exist and behaviour is exactly as before.
- A deployment that enables it needs the same key on both hops
  (`MIMO2API_API_KEYS` on our side, `client_authentication.allowed_keys` in the
  middleware config). The middleware's `key_passthrough` then sends that same
  header on to us, so both hops are authenticated as the caller.
- Tool-using conversations share session state with plain ones only when they
  carry an explicit `session_id` / `X-Mimo-Session-Id`: the identity is derived
  from the request content, and the injected protocol changes the system prompt.
  A client that mixes plain and tool turns should pin the session.
- Latency: one extra local hop per tool request. Measured live: a tool call
  against the real model took 39 s (round 1) and 49 s (round 2 with the result
  fed back) — the model time dominates, and the two middleware hops are
  millisecond-scale.
- The middleware sees the prompt and the tool schemas, as any prompt-protocol
  approach must. It never sees an account credential: those live in our pool and
  travel on the upstream hop only.

## Alternatives rejected

- **Client points at the middleware directly (two public ports).** Works, and the
  compose keeps it possible (`expose`s the port), but it splits the deployment in
  two for callers and needs the same identity plumbing anyway.
- **Route every chat request through the middleware.** Consistent identity and one
  code path, but makes plain chat depend on a second process for no benefit; the
  middleware's own no-tools path is a pure proxy.
- **Native function calling.** The upstream has none, and the field is absent
  from the surface we reverse-engineered.
- **Implement the protocol ourselves.** The middleware is an existing, tested
  project with its own tool-name mapping, recovery and streaming sieve; a second
  implementation of the same protocol would be a second thing to keep in step.
