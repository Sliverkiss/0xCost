# 0027. The external surface leads with OpenAI-standard fields; the model id is never an option carrier

Date: 2026-10-09
Status: accepted
Related: ADR 0016 (no tools), ADR 0017 (no streaming, no session store), ADR 0011
(model catalogue read from the account's own Studio config), SDD §5.2

Numbering note: 0022–0026 live on branch `save/app-layout-and-deploy-fixes` (the
abandoned hosted-platform layout work, rolled back at the owner's request). This
record takes the next free number in this tree so a later merge cannot collide.

## Context

The owner's ruling: *"我们对齐opensi协议的来，你看着操作，我们不改模型名，外部应该用
openai标准字段对齐"* — an OpenAI-compatible client must be able to ask for
everything this gateway offers through fields that client already knows, and the
model ids must stay exactly what the upstream calls them.

Two of the gateway's knobs were ours, not OpenAI's:

| Capability | What we accepted | What OpenAI clients send |
|---|---|---|
| Thinking (深度思考) | `enable_thinking: true`, `reasoning:{"type":"enabled"}` | `reasoning_effort: "minimal"\|"low"\|"medium"\|"high"` (`"none"` = off) |
| Web search (联网搜索) | `web_search_status: disabled\|auto\|enabled`, `:online` model suffix | `web_search_options: {"search_context_size": "low"\|"medium"\|"high"}` |

Consequence of the gap: a standard client asking for thinking in the way its own
documentation describes got **silence** — the field simply landed in the dropped
bucket. The reverse also held: the only way to reach the upstream's `auto` search
mode was a field no OpenAI client knows. And the `:online` suffix put an option
into the model id, which is exactly the "rename the model to carry a setting" habit
the owner rejected.

## Decision

**The standard field is the documented path; our spellings stay as aliases; the
model id travels byte-for-byte.**

1. `reasoning_effort` is now the documented thinking knob. `none` → upstream
   `modelConfig.enableThinking:false`; `minimal`/`low`/`medium`/`high` → `true`.
   The upstream switch is a boolean, so the *level* has nothing to map onto: we
   accept the level as the opt-in it implies rather than inventing a gradation the
   channel cannot honour.
2. When `reasoning_effort` is present it decides — **including `none`, which
   overrides the older spellings** (`enable_thinking: true` +
   `reasoning_effort: "none"` means off). When it is absent, `enable_thinking` /
   `reasoning:{"type":"enabled"}` still work, so nothing that worked before breaks.
3. `web_search_options` (presence = search this turn) is the standard search knob.
   `web_search_status` remains for the finer `disabled`/`auto`/`enabled` control
   and **wins when both are given**, because it is the only way to express `auto`;
   the `:online` suffix stays as the shorthand for `enabled`.
4. `search_context_size` is validated (`low`/`medium`/`high`) but cannot be
   honoured — the upstream switch has no size axis. It is therefore reported in
   `X-Mimo-Warnings` as dropped, never silently pretended to matter. Missing
   granularity must never look like applied granularity.
5. A misspelled knob is a **400 before any account work**, naming the accepted
   values (`unsupported_reasoning_effort`, `unsupported_search_context_size`).
   A typo previously would have cost an upstream turn and changed nothing.
6. Both fields are in the accepted-field whitelist, so they are no longer counted
   as unknown fields, and neither one touches the request's `model`.
7. No model id changes: `/v1/models` keeps advertising the upstream's own ids and
   the request's `model` reaches the channel unchanged (asserted at the wire in
   OSTD-007).

## Consequences

- An OpenAI SDK can now turn on thinking (`reasoning_effort`) and search
  (`web_search_options`) with no gateway-specific knowledge; the extensions are
  additive and documented as extensions.
- `search_context_size` is accepted-but-dropped-with-a-warning: a client that
  needs to know will read `X-Mimo-Warnings`; a client that ignores it loses
  nothing real.
- The dropped `reasoning_effort` *level* is a known, permanent gap of the upstream
  channel (`enableThinking` is a boolean) — documented in SDD §5.2 so nobody
  later mistakes it for an oversight.
- Two spellings of each knob now exist. That is deliberate: the standard path is
  what new clients use, the old path is what already-deployed clients sent.

## Evidence

- Tests: `internal/api/openai_standard_test.go` OSTD-001..007 (upstream
  `enableThinking`/`webSearchStatus` asserted at the mock's recorded request, the
  model id asserted unchanged, the warning header asserted).
- Gate: `make fmt vet hygiene test-race cover-check` — exit 0 on the commit that
  carries this record.
- Live, against the real channel through the running gateway:
  - `reasoning_effort:"high"` → 200, `reasoning_content` (119 chars),
    `usage.completion_tokens_details.reasoning_tokens: 55`, answer `23, 12`;
  - `reasoning_effort:"none"` → 200, no `reasoning_content`;
  - `reasoning_effort:"very-high"` → 400 `unsupported_reasoning_effort`;
  - `web_search_options:{"search_context_size":"medium"}` → 200 after 21.8 s with
    16 `citations` / 16 `message.annotations` (`type: url_citation`) and
    `X-Mimo-Warnings: ignored unsupported fields: [web_search_options.search_context_size]`;
  - `web_search_options:{"search_context_size":"huge"}` → 400
    `unsupported_search_context_size`;
  - `web_search_status:"disabled"` + `web_search_options` → 200, 0 citations
    (the explicit control wins);
  - `/v1/models` → the same 7 ids, none carrying `:` or `thinking`.
