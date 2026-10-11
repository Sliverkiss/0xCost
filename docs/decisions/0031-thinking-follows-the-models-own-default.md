# 0031 — The thinking default is the model's own, not ours

Status: accepted (2026-10-10)

## Context

`reasoning_content` came back empty for every caller that had not asked for
thinking — and a plain OpenAI body asks for nothing. The cause was our own default:
the upstream switch went out as `enableThinking:false` unless a request carried
`reasoning_effort` (or one of the older spellings).

The upstream says otherwise. Its catalog — the `bot/config` the Studio itself reads —
declares `thinkingDefaultOn: true` for **every chat model this account serves**
(`mimo-v2.6-flash`, `mimo-v2.6-pro`, `mimo-v2.6-pro-ultraspeed-studio`; the audio
rows carry no thinking switch), and that is the value the Studio's UI starts from. We
were parsing that field all along and using it only for the `/v1/models` capability
mark, while the request path overrode it with a default of our own.

Measured before the change (2026-10-10, through the public entry):

- a plain request returned no `reasoning_content`, while the platform still counted
  the work it had done — `usage.completion_tokens_details.reasoning_tokens` was 84
  (flash) / 95 (pro) with the switch off. The reasoning happened; the client saw none
  of it.
- with `reasoning_effort:"high"` the same prompt returned **781** (flash) / **212**
  (pro) characters of real reasoning — so the markers, the splitter and the response
  path were already right. The default was the whole difference.

An empty field looked like a misalignment in the splitting layer; it was a decision in
the request layer.

## Decision

The switch is resolved in two layers, both of them the protocol's own:

1. **The request**, when it says anything: `reasoning_effort` decides whenever
   present (`none` = explicit off, every other accepted value = on), then the older
   `enable_thinking` (now a pointer, so "false" and "not sent" are different
   requests), then `reasoning.type`. This is the only way to disagree with the
   provider, and it is the standard field (ADR 0027).
2. **The model's declared default** from the account catalog (`thinkingDefaultOn`),
   read from the cached snapshot the request path already holds — a chat request
   still never triggers a catalog read (ADR 0011). An id the catalog does not
   describe gets the same answer, on: that is what the platform declares for its chat
   models, and OpenAI's own reasoning models think by default, so an unknown id
   follows the provider's behaviour rather than a local opinion.

The built-in floor carries no declaration, and `ThinkKnown` keeps that distinct from
"declared off": a failed catalog read must not silently change how the models behave.

**No deployment-level switch.** An earlier cut of this change added
`MIMO2API_DEFAULT_THINKING`, and it was removed the same day: a gateway-level knob is
a second, non-standard way to say what `reasoning_effort` already says, and the rule
for this surface is that OpenAI's vocabulary leads and our own inventions are aliases
(ADR 0027). A deployment that wants less thinking sets it in the client, and the
default simply follows the platform's declaration.

## Consequences

- A plain OpenAI request now gets the model's working in `reasoning_content`, which is
  what the platform documents for these models and what a reasoning-capable model does
  by default. Turning it off stays one standard field away.
- Cost moves to the client's side, and it is measured rather than asserted: thinking
  on is roughly 2.5–4× the wall time on the flash/ultraspeed channels (7.6 s → 19.2 s
  on a short prompt; 17.9 s → 94.5 s on a derivation task) but ~neutral on `pro`
  (13.4 s → 12.4 s), and ~40 % more completion tokens (540 → 751). The escape hatch
  is the standard one: `reasoning_effort:"none"` on the request.
- The platform's own preamble — ~2,235 prompt tokens on every request, measured by
  intercept and identical across models, channels and both switch positions — is
  untouched by this and untouchable by us: we never send it, and an independent
  capture of the same API shows the same floor. It is per upstream request, so the
  only lever is making fewer upstream calls.
- `usage.reasoning_tokens` stays the upstream's number even when the switch is off
  and no `reasoning_content` is returned: it reports the work the model did, not what
  this gateway chose to expose.
