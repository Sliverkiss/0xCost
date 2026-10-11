# ADR 0015 — Request size: a real cap for ultraspeed, a translation for everything else

Status: accepted (2026-10-09). Amends SDD §2.8.

## Context

A live chat hit `400 context_too_long — "query exceeds the model channel's
character limit"` and the client could not tell why. The rejection was ours:
50,000 characters for the fastchat (ultraspeed) channel, **90,000 for every other
model, per message**.

Measured against the live platform on 2026-10-09 (single user message of CJK
filler, `mimo-v2.6-pro`, `MIMO2API_MAX_QUERY_CHARS` raised so the upstream was
the judge):

| request (single message, CJK filler unless noted) | upstream answer |
| --- | --- |
| 95,000 chars | served |
| 100,000 chars | served |
| 100,100 chars | served |
| 101,000 chars | served |
| **exactly 102,400 chars** | **served** |
| 102,415 chars (ASCII **and** CJK) | refused: *"Sorry, the text you sent is too long! I suggest you simplify the content appropriately or send it in parts."* (mid-stream `error` event → we surfaced 502) |
| 105,000 chars | refused, same wording |
| 300,000 ASCII chars | refused, same wording |
| 20 × 6,000 chars (120,000 total across messages) | served in 258 s, and the model's answer named all 20 parts and their ~112,000-char total — the whole payload was read, not merely accepted |
| ultraspeed 49,500 chars | served |
| ultraspeed 52,000 / 60,000 chars | refused: *"query is too long"* |

Three conclusions:

1. The old 90,000 figure was an unmeasured guess, and it **rejected work the
   platform serves** (95,000 and up to 102,400 are fine).
2. The real ceiling is **102,400 characters of the query text** — a clean number,
   and the same for CJK and ASCII (both refused at 102,415). An intermediate
   reading of the same data ("a token budget near 100k that moves with content")
   was **wrong**; only the distance to the boundary varies, not the boundary.
3. The ultraspeed 50,000 cap is real and sharp — 49,500 served, 52,000 refused —
   and matches the Studio's own `ultraspeedQueryLimitLength: 5e4`
   (docs/web-surface-reverse.md §11).

The upstream's refusal is also useless to a client: English prose wrapped in our
502 `upstream_failure`.

## Decision

1. **fastchat (ultraspeed): keep the 50,000-character local cap.** It is the
   platform's own limit, enforced before any account is touched, so the caller
   gets a 400 with the number instead of a wasted call and an upstream refusal.
2. **Other channels: 102,400 characters, the measured platform ceiling.** Since
   the boundary is a clean character count and is language-independent, mirroring
   it locally costs nothing and cannot reject served work (the platform refuses
   anything above it anyway), while saving a wasted account call and giving the
   client our own actionable error first.
3. **Translate the upstream's own refusal** (`too long` / `query is too long` /
   `长度不能超过` / `内容过长`), whether it arrives as an HTTP error body or as a
   mid-stream `error` event, into `400 invalid_request_error / context_too_long`
   with an instruction ("send fewer or shorter messages") and the two numbers we
   actually know. A refusal caused by the caller is the caller's status code, and
   it does not penalize the account (no pool cooldown).
4. **`MIMO2API_MAX_QUERY_CHARS`** overrides both caps (0 = per-channel defaults):
   a deployment can move the guardrail without a rebuild — the last resort when a
   client legitimately sends more than we allow.
5. **Every local rejection is logged** with the measured size, the cap, the model
   and the role. The old code rejected silently, which is why the first report of
   this had no server-side trace at all.

## Consequences

- The message a client sees now names the limit and the measured size, so "why is
  this too long" is answerable without reading our code.
- Requests up to 102,400 characters are no longer blocked locally (the old guess
  stopped at 90,000); anything above it is refused by the platform too, so we
  answer with our own 400 instead of forwarding a call that cannot succeed.
- The per-message check is deliberately not a per-conversation check: 20 messages
  totalling 120,000 chars are served by the platform (measured), so rejecting a
  long *history* locally would break working traffic.
- A chat body is still unbounded in the handler; the 150,000 guard is the only
  bound short of adding a body limit. Worth revisiting if abuse appears.

## What the ceiling actually counts (measured 2026-10-09, round 2)

The boundary is a character count, but not of the client's message: it is the
**query text the platform receives**. The planner splices every system message
into the first user query (API-017), so a system prompt — or the protocol prompt
a tool layer injects in front of us — spends the same budget.

| experiment | result |
| --- | --- |
| system 1,000 + user 101,900 (= 102,900 composed) | refused |
| system 2,000 + user 100,000 (= 102,007 composed) | served |
| gateway's raw chat path, 102,000 chars, no tools | served |
| gateway's raw chat path, 102,000 chars, **30 tools** | served (the gateway ignores `tools`; nothing is injected) |
| tool layer, 102,000 chars, no tools | served (no tools = straight passthrough) |
| tool layer, 100,000 chars, 1 tool | served (100,000 + 1,355 + 7 = 101,362) |
| tool layer, 100,000 chars, 3 tools | served (100,000 + 1,760 + 7 = 101,767) |
| tool layer, 101,000 chars, 3 tools | refused (101,000 + 1,760 + 7 = 102,767) |
| tool layer, 102,000 chars, 30 tools | refused |

The refusal body even names both numbers: `chars=102402, limit=102400`.

The injected XYML protocol prompt costs, measured by calling
`app.engine.xyml.build_tool_instructions` directly (no guessing):

| tools | injected chars |
| --- | --- |
| 1 | 1,355 |
| 3 | 1,760 |
| 5 | 2,012 |
| 10 | 2,642 |
| 30 | 5,222 |

So the tool path's effective client budget is `102,400 − injected`, and the pair
of tool-layer rows above is exactly the arithmetic: 100,000 + 1,760 = 101,767
(served), 101,000 + 1,760 = 102,767 (refused).

### Consequence in our code

The pre-flight check validates the **composed first query**, not each message in
isolation (`adapter.ComposedFirstQueryLen`), and the error names how much of it
is preamble. Before this, a client message that fitted on its own but not once
the protocol prompt was spliced in passed our check and was refused by the
platform — one wasted account call, and a much less useful message.

### Ways to go beyond the ceiling

1. **Send it in parts** — the upstream's own suggestion in its refusal text, and
   the shape our planner already uses: one upstream query per client user
   message, chained through `previousDialogueId`, so only the *per-query* ceiling
   applies. Verified end to end: 20 × 6,000 chars (120,000 total, every part under
   the ceiling) returned 200 and the model's reply accounted for all 20 parts and
   their ~112,000-character total. The cost is latency, not capacity: one upstream
   call per part, ~13 s each, so that request took 258 s.
2. **Spend less of the budget on preamble** — on the tool path the injected
   protocol is 1,355 characters for a single tool; trimming it buys the client
   real room.
3. **Attachments** — the live catalog advertises `fileUpload.enabled = true` with
   `maxFileSize: 100` (MB) and `maxFileCount: 50` for every model, and
   `POST /open-apis/resource/parse` answers with a `tokenUsage`, i.e. attachments
   are accounted for separately from the query text. This is the only path that
   plausibly lifts the ceiling by orders of magnitude; it is **not yet verified
   end to end** (see docs/attachments-reverse.md).
