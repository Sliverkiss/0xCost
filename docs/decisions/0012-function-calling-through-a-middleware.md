# 0012 — Function calling: the middleware sits in front of chat, one hop, one key

Status: accepted (2026-10-09)
Related: SDD §5.6, `deploy/toolforge/` (PROVENANCE.md), spec REQ-3/INV-2, live-findings LIVE-TOOLS-001

## Context

MiMo Studio has no function calling: the chat surface takes messages, a model id
and a few switches, and nothing in `bot/config` or the request schema carries
`tools`. Our API decoded `tools` from the start only to report it as dropped in
`X-Mimo-Warnings` (SDD §5.2, TDD API-008/009).

ToolForge (`github.com/Sliverkiss/toolcall`, a fork of `YuJunZhiXue/toolforge`) does
exactly what is missing: it strips `tools`, injects an XYML prompt protocol, parses
the tool call out of the model's text, and answers with standard `tool_calls`. It is
built to sit in front of an OpenAI-compatible gateway — including this one.

## Decision

**The middleware is the chat entry point that understands tools; it calls this
gateway's ordinary chat surface. One hop, one key.**

```
client :8081 --(Bearer KEY)--> toolforge --(the same header)--> mimo2api :8080 --(pool)--> MiMo
                                 strips tools, injects XYML, parses back
```

1. **It sits in front, not behind.** The model call must stay on our path — the
   account pool, the session identity, the egress rules and the retry ladder all
   live there — but nothing requires *us* to make the first hop. In front, a tool
   request costs two hops; behind, it costs three (client → us → middleware → us)
   and needs an internal surface that must never be forwarded onward.
2. **We own no forwarding code.** What reaches us is an ordinary chat request: the
   middleware has already removed `tools`, so there is no `tools` in the body to
   trigger anything, no `/v1/raw` path, no recursion guard, and no
   `MIMO2API_TOOLFORGE_URL`. `internal/toollayer` exists only as history: the
   first implementation proxied from here and was replaced by this shape.
3. **One key, both hops.** `MIMO2API_TOOL_KEY` is the caller's credential, the
   middleware's `allowed_keys` entry, and an entry in our `MIMO2API_API_KEYS`. Our
   copy of the middleware forwards the caller's `Authorization` and `X-Mimo-*`
   headers upstream (`key_passthrough`, `passthrough_headers`) — the one change we
   carry in it. Without it the second hop would authenticate as the middleware,
   and since session state is scoped per credential with a content-derived identity
   (spec REQ-3/INV-2, including model, system prompt and tools), every caller would
   share one namespace.
4. **One address for the whole API** (`catch_all_proxy`). The middleware implements
   chat and `/v1/responses`; every other path — `/v1/models`, `/v1/audio/*`,
   `/v1/admin/*` — is relayed to us verbatim: method, query, body (multipart uploads,
   binary audio) and the caller's own headers, with the upstream's status and bytes
   passed back. A client therefore keeps a single `base_url` and a single key; it
   does not need to know that tools are served by a different process. `/healthz` on
   that port reports both its own state and ours. Without this, one port could serve
   tools and the other everything else, and every client would have to care.
5. **A middleware that is down does not degrade silently.** Callers pointed at
   :8081 get a connection error; callers pointed at :8080 get today's behaviour —
   `tools` reported as dropped in `X-Mimo-Warnings`. Neither ever answers a
   tool-bearing request as if the model had ignored its tools.
6. **The middleware is vendored.** `deploy/toolforge/` holds its source (base
   commit `758a2c4`, runtime + tests + licence) and our change to it, tracked like
   the rest of our code; `PROVENANCE.md` records where it came from and what we
   changed. The deployment needs no clone, no GitHub access and no credentials at
   build time (the base repo is private, so a build-time fetch would not work
   anyway), and the change is ordinary code rather than a patch.

## Consequences

- The middleware is ours to maintain: an upstream change has to be re-applied by
  hand (`PROVENANCE.md` names the eight files that differ).
- One published port (8081); the gateway's 8080 stays on the compose network. The
  trade-off: the middleware is on the path to *every* endpoint, so if it is down
  nothing answers — it is a small local process with a healthcheck and
  `restart: unless-stopped`, and 8080 can be published directly for an escape hatch
  (compose has the mapping commented out).
- The shared key means callers that send the same first message with the same tools
  derive the same session. A client that needs its own namespace pins
  `X-Mimo-Session-Id` (it travels through, via `passthrough_headers`).
- Tool-using turns and plain turns of the same conversation are different sessions
  unless the session is pinned: the injected protocol changes the system prompt,
  and the identity is derived from request content.
- Latency: one extra local hop per tool request, milliseconds. Live measurement of
  a real tool call through the middleware: 33-49 s per turn, all of it model time.
- The middleware sees the prompt and the tool schemas, as any prompt-protocol
  approach must. It never sees an account credential: those live in the pool and
  travel on the upstream hop only.
- A deployment adds a second process; `docker compose up -d --build` starts it, and
  with `MIMO2API_TOOL_KEY` unset nothing is reachable, so set it.
- The relay is generic: any endpoint we add later is reachable through the one port
  without touching the middleware.

## Alternatives rejected

- **Proxy from the gateway (client → us → middleware → us).** Built first, working,
  then dropped: it costs an extra hop, an internal raw surface, a recursion guard
  and ~150 lines of forwarding code, to buy nothing — the caller can reach the
  middleware just as easily, and the middleware can reach us on the ordinary path.
- **Route every chat request through the middleware.** Consistent identity and one
  code path, but plain chat would depend on a second process for no benefit.
- **Implement the protocol ourselves.** The middleware is an existing, tested
  project with its own tool-name mapping, recovery and streaming sieve; a second
  implementation would be a second thing to keep in step.
- **Native function calling.** The upstream has none, and the field is absent from
  the surface we reverse-engineered.
