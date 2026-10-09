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
| 20 × 6,000 chars (120,000 total across messages) | accepted and processed |
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
