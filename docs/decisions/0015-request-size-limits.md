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

| request | upstream answer |
| --- | --- |
| 95,000 chars | served normally |
| 100,000 chars | served normally |
| 100,100 chars | served normally |
| 105,000 chars | refused: *"Sorry, the text you sent is too long! I suggest you simplify the content appropriately or send it in parts."* (arrives as a mid-stream `error` event → we surfaced 502) |
| 300,000 ASCII chars | refused, same wording |
| 20 × 6,000 chars (120,000 total, none above 90,000) | accepted and processed |
| ultraspeed, 60,000 chars | refused: *"query is too long"* |

Two conclusions:

1. The old 90,000 figure was an unmeasured guess, and it **rejected work the
   platform serves** (95,000 is fine). It also measured the wrong thing: the
   platform's ceiling moves with content — a token budget near 100k rather than a
   clean character count — so no local number can mirror it exactly.
2. The ultraspeed 50,000 cap is real and sharp (60,000 is refused), and it is the
   Studio's own `ultraspeedQueryLimitLength: 5e4` (docs/web-surface-reverse.md §11).

The upstream's refusal is also useless to a client: English prose wrapped in our
502 `upstream_failure`.

## Decision

1. **fastchat (ultraspeed): keep the 50,000-character local cap.** It is the
   platform's own limit, enforced before any account is touched, so the caller
   gets a 400 with the number instead of a wasted call and an upstream refusal.
2. **Other channels: 150,000 characters, as a guardrail only** — not a claim
   about the platform. It stops absurd payloads (nothing else bounds a chat body),
   and cannot reject anything the upstream would have served.
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
- Requests between 100k and the platform's real ceiling are no longer blocked
  locally; they are forwarded and, if refused, answered with 400 instead of 502.
- The per-message check is deliberately not a per-conversation check: 20 messages
  totalling 120,000 chars are served by the platform (measured), so rejecting a
  long *history* locally would break working traffic.
- A chat body is still unbounded in the handler; the 150,000 guard is the only
  bound short of adding a body limit. Worth revisiting if abuse appears.
