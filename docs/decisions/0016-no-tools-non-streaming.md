# 0016. No function calling, and chat is non-streaming

Date: 2026-10-09
Status: accepted (partly superseded by ADR 0017)
Supersedes: ADR 0012 (the tool layer) — the deployment no longer runs it.

**Update 2026-10-09 (ADR 0017):** §1 (no tool layer) stands. §2 (the
`MIMO2API_ALLOW_STREAM` switch) is superseded: the streaming path is now deleted
outright rather than switched off, together with the session store it existed for.

## Context

Two facts from the same measurement round made the tool layer's cost visible and
its benefit unclear.

The tool layer was a second service in front of the gateway: a client sent tools
to it, it stripped them, injected a prompt protocol (XYML), called us, and parsed
the model's text back into a `tool_calls` response. MiMo has no native function
calling, so that indirection was the only way to offer tools at all.

But the injected protocol prompt is text the platform counts: 1,355 characters for
one tool, 1,760 for three, 5,222 for thirty — and the platform refuses any query
above 102,400 characters **as composed**, system prompt and injection included
(ADR 0015). Measured: 102,000 characters with thirty tools is served on the raw
chat path and refused through the tool layer. So every tool request silently
bought a smaller context, on a channel whose ceiling the owner already finds
small: "上下文太短，做不了什么事".

Streaming, meanwhile, was the only reason the session machinery existed: chaining
is streaming-only (ADR 0009), non-streaming is one shot. The owner does not use
streaming.

## Decision

1. **The tool layer is not deployed.** The deployment is one container with one
   published port: `127.0.0.1:21223 → mimo2api:8080`. The vendored source tree
   (`deploy/toolforge/`) stays in the repository — it is work we own and git
   history is the backup — but nothing builds or runs it. A request that still
   carries `tools` is answered normally with the tools reported as dropped in
   `X-Mimo-Warnings`: explicit, never a silent downgrade.
2. **Chat is non-streaming.** `MIMO2API_ALLOW_STREAM` defaults to **false**; a
   request with `stream: true` is refused with `400 stream_unsupported` naming the
   fix, before any pool or account work. The SSE emission path stays in the code
   behind the switch (the streaming suites still exercise it), so returning to
   streaming is one environment variable, not a rebuild of the architecture.

## Consequences

- **Context back**: a tool request no longer pays 1,355–5,222 characters of
  injected protocol, and the whole 102,400-character ceiling belongs to the
  caller's own text.
- **Simpler deployment**: one container instead of two, one health check, no
  middleware config, no second credential, and no `depends_on` ordering to get
  wrong on a cold start.
- **No function calling.** A client that wants tools runs them itself and sends
  the results back as messages — the supported shape is plain chat.
- The session/chaining machinery is now unreachable from the public API
  (non-streaming is one-shot by design, ADR 0009). It is kept, tested, and
  documented rather than deleted in the same change; retiring it is a separate,
  reversible cleanup.
- The proxy in front (Caddy) keeps `flush_interval -1` and
  `read_timeout/write_timeout 0`: harmless for a single JSON response, and it
  keeps the config valid if streaming is switched back on.
