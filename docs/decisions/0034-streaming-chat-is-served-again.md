# 0034 — Streaming chat is served again (the client-side SSE half of 0017 is reversed)

Status: accepted (2026-10-10). Supersedes the client-facing half of
[0017](0017-no-streaming-no-session-store.md); 0017's session-store half stands.

## Context

ADR 0017 deleted the streaming path entirely — `MIMO2API_ALLOW_STREAM`, the SSE
emission path, the keepalive ticker and the pre-first-byte commit boundary — and
refused `stream: true` with `400 stream_unsupported`. The reason it could be
deleted at all is the fact that makes restoring it cheap: **the upstream has only
one shape**. `POST /open-apis/bot/chat` answers `text/event-stream` on both
channels (`/open-apis/bot/chat` and the fastchat prefix `/fastchat/open-apis/bot/chat`),
and the non-streaming response this gateway served was that stream collected in
full and then shaped into one JSON body. The owner hit the cost directly: with
non-streaming, a client sees nothing until the last token, so a long answer feels
slower than it is (`非流式慢，我们需要把流式这个功能给加回来`).

Two consequences of that shape decided the restoration:

- The upstream request is byte-identical in both transports — no upstream flag, no
  second endpoint, no capability negotiation. The parser (`internal/mimo/sse.go`),
  the OpenAI chunk shapers (`internal/adapter/openai.go`), `adapter.EncodeChunk`,
  the fixture set and the golden stream file all survived the deletion, because
  the JSON path needed them.
- Both channels stream, so this is a delivery choice, not a per-model capability.
  `client.go` routes the two channels by *path* only; the SSE handling after it is
  shared, and the recorded fixtures confirm the same frame vocabulary
  (`dialogId`/`message`/`usage`/`finish`) on each.

The web SPA was re-read for this decision rather than guessed at (`main.911869f9.chunk.js`,
the `content`-thunk's read loop, docs/web-surface-reverse.md §11). It establishes
the vocabulary we now consume and, for streaming clients, mirror:

| upstream fact | what it means here |
|---|---|
| the SPA accepts only `finish, usage, message, dialogId, web_search, error, sensitive_query, sensitive_title, doc, tip_ratio, tip_truncate` and `continue`s past anything else | unknown events are skippable, not fatal — our parser already ignores them |
| `message` frames are **deltas** (`n += s` in the SPA's accumulator) | forwarding them is correct; no diffing against a cumulative text is needed |
| thinking rides **inside** `message` text between `<think>\0` and `</think>\0` (NUL-terminated markers, with a holdback the length of a marker) | the splitter's job is unchanged; the marker bytes must never reach a client |
| `error` mid-stream carries the platform's own sentence in `data.content`; the SPA also treats `451`/`461` as (temporary/permanent) bans | our in-band error must keep our error contract and the platform's words, and the ban classification stays in `run` |
| `tip_ratio` / `tip_truncate` mark context pressure and truncation | not modelled by OpenAI's stream; left unforwarded (documented, not silently dropped) |

## Decision

1. **`stream: true` selects SSE delivery, and nothing else does.** There is no
   deployment switch: `MIMO2API_ALLOW_STREAM` stays deleted, and a body that does
   not ask for streaming still gets exactly the JSON body it got before. The
   client's own field is the whole decision.
2. **The commit boundary is the first upstream delta.** Nothing is written to the
   response — not the headers, not a comment — until a content or reasoning piece
   arrives. A failure before that point is still pre-response, so the existing
   retry machinery keeps working unchanged: refresh-retry on the same account,
   one retry on another account, cooldown attribution. After the first delta the
   response is committed, every retry branch is skipped, and failures travel
   in-band. The accepted cost: a client sees nothing until the upstream's own
   time-to-first-byte (measured 0.8–2.9 s on the pro channel, ~0.5 s on
   ultraspeed), because a keepalive before the commit would commit the response
   and take account failover off the table.
3. **Frame order is fixed**: role-only delta → content/reasoning deltas →
   `delta:{}` + `finish_reason:"stop"` → citations chunk (`choices:[]`, only when
   search returned results and `include_citations` is not false) → usage chunk
   (`choices:[]`) → `data: [DONE]`. The usage chunk cannot clobber the
   finish_reason a client already read, and a choices-less chunk is ignorable by
   design — the same reasoning the surviving shapers documented (§5.4). The wire
   bytes are pinned by the recorded golden
   (`internal/testutil/testdata/openai_stream_expected.txt`).
4. **Usage is always sent, in the final chunk.** OpenAI gates usage behind
   `stream_options.include_usage`; this gateway always reports it, because the
   JSON transport already does and a client that ignores the chunk loses nothing.
   A deliberate, documented non-alignment.
5. **Thinking follows the same rule as JSON**: reasoning deltas when thinking is
   on, suppressed entirely when the client turned it off (`reasoning_effort:none`
   / `enable_thinking:false`). The marker bytes never appear on the wire.
6. **An error after the commit is data, not prose.** The failure is emitted as
   `data: {"error":{message,type,param,code}}` followed by `[DONE]`, carrying all
   four keys (ADR 0032) and the platform's own wording where there was any (ADR
   0033). The deleted path appended the error text to `delta.content` and then
   finished the stream as if all were well — that presents a failure as an answer,
   which is exactly what ADR 0032/0033 exist to prevent. TDD API-021 is redefined
   accordingly (its ID is reused, its expectation is not).
7. **Streaming revived no session state.** ADR 0017's other half — no
   `internal/session/`, no `X-Mimo-Session-Id`, no fork policy, one full replay
   per request — stands untouched (ADR 0009 stays superseded). Only the last turn
   of a replay is streamed; earlier turns exist to rebuild the upstream
   conversation and their pieces are collected and dropped, which is the same
   single-delivery-point rule both transports follow.
8. **The keepalive cadence is a constant (15 s), not an env var.** It is timing,
   not policy: the client's field decides whether there is a stream at all.
   `Options.KeepaliveInterval` exists so a test can shorten it (the old
   `MIMO2API_KEEPALIVE_INTERVAL_MS` stays deleted).

## Consequences

- OpenAI chat clients get incremental output again with no configuration, and a
  client that needs one shot still sends `stream:false` (or omits it) and gets the
  identical JSON body as before — the non-streaming path is unchanged code, not a
  parallel implementation.
- What did **not** come back, and must not be read as collateral: the env switch,
  the session store and its header, the fork policy, and the golden-comparison
  acceptance step for a separate transport (§5.7's refusal paragraph is rewritten,
  not deleted in spirit: the transport is now described instead of denied).
- Web search, TTS and ASR are untouched — they never consulted the switch, and
  citations still arrive in the JSON body for non-streaming callers.
- Tests: STREAM-011..023 replace STREAM-001 (which asserted the refusal), and
  API-019/020/021 come back under their original IDs. The load-bearing ones are
  STREAM-019 (keepalive cannot commit), STREAM-016 (a pre-first-byte failure still
  fails over), STREAM-015 (in-band error keeps the contract) and STREAM-023
  (byte-for-byte golden).
- The 15 s keepalive starts only after the commit, so a client that waits 2 s for
  the first token sees no traffic at all in that window. Clients with an aggressive
  own timeout must allow the upstream's TTFT; this is stated here because it is a
  real behavioural difference from a gateway that opens the stream immediately.

## Alternatives rejected

- **Keep non-streaming and tell clients to wait.** That is the state the owner
  rejected; the latency is in the client's perception, not in the total token time.
- **Commit the response immediately** (headers + role chunk + keepalives from the
  start). It makes the first byte instant, but it consumes the commit boundary:
  a failure at token 1 could no longer fail over, and the retry ladder this
  deployment relies on would become unreachable exactly when it matters most.
- **Relay the upstream frames verbatim** (`event:` lines and all). The upstream is
  not an OpenAI endpoint: its event names, its NUL-terminated thinking markers and
  its in-band error shape are all things an OpenAI client cannot parse.
- **Keep the old in-band error handling** (append the text to `delta.content`).
  Rejected as fabrication: a failure would be indistinguishable from an answer,
  and the `code` ADR 0032 promises would have nowhere to live.
