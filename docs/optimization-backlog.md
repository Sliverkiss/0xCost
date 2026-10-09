# Optimization review — what the other "2api" projects do, and what is worth taking

Date: 2026-10-09 · Method: GitHub search (`mimo2api`, `2api in:name`, `chat2api`,
`free-api`, openai-compatible reverse proxies), READMEs read in full for the
repositories below, then each candidate checked against our own code.
**This is a backlog, not a decision.** Anything accepted gets an ADR.

## 1. The field (who else is doing this, and with what)

| Repo | ★ | Stack | What they have that we do not |
|---|---|---|---|
| `Fly143/MiMo2API` | 146 | Python | **Same upstream as us** (MiMo AI Studio web). OpenAI + **Anthropic Messages** (/v1/messages, count_tokens, batches: 9 endpoints) + **Responses API**; **multimodal** (image URL/base64, the same three-step upload we implemented in `attach.go`); **7-strategy function-call extraction** with a streaming tool sieve; **long-conversation compression** (middle history → summary, ~20K recent kept); streaming; multi-account rotation; context compression to dodge the query cap |
| `IMROVOID/MiMo2API` | 1 | TS / Cloudflare Worker | Reads Xiaomi OAuth from MiMo **Code** Desktop; proactive token refresh; **multi-account round-robin with 429 failover** (parks the exhausted token, retries transparently); SSE `reasoning_content` deltas; 120 s upstream timeout; runs as a Worker *and* a local daemon |
| `Zhengyuuuui/mimo2api` | 3 | Python single file | Auto-harvests `passToken` from the Desktop cookie store; full-chain renewal (passToken → SSO 5 steps → serviceToken) with a 30 min cache and 401-triggered refresh; `tool_calls` + `reasoning_content` passthrough; survives Clash/Surge TUN |
| `Water008/MiMo2API` | 57 | Python | **Web management UI** (configure accounts + API keys, "test account" button) |
| `lanqian528/chat2api` | 3833 | Python | Multi-account rotation, retry-to-next-token, **token management UI** (upload/clear), scheduled forced refresh, explicit **"gateway mode, multi-machine deployment"**, `SeedToken` session isolation |
| `Xerxes-2/clewdr` | 1276 | Rust | One static binary, <10 MB RAM, <1 s start; **admin WebUI with a printed password**; cookie status with remaining quota; retry limits and cookie-skip rules; `count_tokens`; **multi-arch images (amd64+arm64)** with version tags |
| `lza6/smithery-2api`, `lza6/doubao-2api`, … | 40–100 | FastAPI | Multi-account rotation, identity-disguise module, Docker one-liner, pseudo-streaming SSE engine, anti-detection/stealth |

Two observations. First: **streaming is universal** in this field, and we turned it
off on purpose (ADR 0017) — so a feature-by-feature comparison will always show us
"behind" there, and that is a ruling, not a gap. Second: the projects that do the
same upstream as us solve the *same* three problems we did (cookie/session, device
identity, account rotation) — and they are not ahead of us on any of them.

## 2. Worth taking (ranked by value / cost)

### ~~P1 — Multimodal input~~ — **DONE (ADR 0028)**: the owner confirmed the models
read images; `image_url` parts (data URL or http(s)) now travel the attachment path
per turn, with the parse retried and named 400s. Live proof: a marker PNG's
`ZQ-7788` read back exactly.

#### (original entry, kept for the reasoning)
- **Then**: `internal/api/request.go:202` rejected `image_url` with 400
  `unsupported_content`; `internal/mimo/attach.go` already implemented the whole
  upstream chain (`genUploadInfo` → PUT → `resource/parse`) and was machine-verified
  via `cmd/attachprobe`, but **was not wired into `/v1/chat/completions`**. Now wired
  (ADR 0028), and the open question below answered **yes**: a live marker image's
  `ZQ-7788` came back exactly.
- **Them**: `Fly143/MiMo2API` accepts image URL *and* base64 across the same three
  steps; `chat2api` accepts URL and base64 too.
- **Cost**: small–medium — decode parts, fetch/decode the URL or data URL, upload,
  pass the parsed resource id into the turn. Tests: our mock can serve the three
  endpoints (it already does for TTS/ASR uploads).
- **Open question that decides the value**: our account's `modelConfigListNg` carries
  `{model,name,pageType,redirectTo,intro}` with **no capability flags**, and the
  served set is 7 ids after the `redirectTo` scrub. **Before building this, probe
  whether any served model actually accepts an image** — otherwise we would wire a
  path nothing can use.
- **Note**: an attachment is its own budget (measured: 100,036 chars parsed,
  110,020 truncated, ADR 0015), so the wiring has to keep the two budgets separate.

### P3 — Long-conversation compression: **downgraded after the owner's question**
The owner asked the decisive question — *"这个怎么个长压缩法，我们就一次对话"*. He is
right, and it kills the pay-off: this gateway is one-shot and stateless (ADR 0017),
so every request replays the client's history from scratch. Compression would have to
re-run on **every request** (one extra upstream call, a different summary each time,
no cache to amortise it), while the sibling project gets to compress once and reuse
it across many turns of a long-lived session. Client-side trimming is where this
belongs. What stays worth doing is the refusal's *quality*: tell the client exactly
how many characters to drop. (Original entry below.)
- **Now**: replay is capped at 20 turns and a query over the channel's 102,400
  characters is a 400 with guidance.
- **Them**: `Fly143/MiMo2API` compresses middle history into a summary and keeps the
  most recent ~20K tokens, so a long chat keeps working instead of failing.
- **Cost**: medium — one extra upstream call to summarise, a documented semantics
  change (what the model sees is no longer verbatim), a flag to control it
  (`auto`/`off`), and a line in `X-Mimo-Warnings` when it fires. Needs an ADR.

### P2 — A real multi-instance defect: the lease counter is per-process
- The account **store** is shared (Upstash), but in-flight/lease accounting lives in
  the process. Two instances would each believe `MAX_INFLIGHT_PER_ACCOUNT` is free
  and oversubscribe the same account — the exact thing the pool exists to prevent.
- **Them**: `chat2api` advertises "gateway mode, multi-machine deployment" explicitly.
- **Cost**: small–medium — move the counter to Redis (`INCR`/`DECR` + TTL, we already
  have a stdlib RESP2 client) **or** state in the docs that one account store serves
  exactly one instance. Either way it should be written down.

### P2 — Anthropic Messages surface (`/v1/messages`), non-streaming
- **Them**: `Fly143/MiMo2API` ships 9 Anthropic endpoints and `clewdr` ships
  `/v1/messages/count_tokens`.
- **Reality check**: the clients that made `/v1/messages` popular (Claude Code and
  friends) *need* streaming and tool use, both of which are closed rulings for us.
  So the honest value is "non-streaming Anthropic clients can point at us", not a
  new product. Cheap enough to be worth it only if that client class matters to you.

### P3 — Observability on the response (cheap, high debug value)
A few headers cost nothing and remove all guessing: `X-Mimo-Model-Resolved` (what the
alias actually hit), `X-Mimo-Account` (masked), `X-Mimo-Upstream-Ms`, plus a
Prometheus-format `/metrics` (stdlib only) and an opt-in JSON log format. We already
have `X-Mimo-Warnings`; this is the same idea extended to routing and timing.

### P3 — Key management and per-key limits
We have a static key list. `clewdr` and `chat2api` have key/token management with
per-key limits. Only worth doing if the gateway is shared with others; otherwise it is
machinery with no user.

### P3 — Packaging: multi-arch image and version tags
`clewdr` publishes amd64+arm64 with pinned tags; this host is arm64. A `buildx`
recipe plus a tagged release path would make the deploy portable, and costs an hour.

### P3 — `/readyz` split from `/healthz`
Our healthcheck answers before the pool exists (we deliberately bind early, ADR 0025).
A separate readiness endpoint that turns green only when the store and one account are
usable is the standard shape for orchestrators.

## 3. Deliberately **not** taking (with the ruling that closed it)

| Their feature | Why not |
|---|---|
| Streaming SSE everywhere | ADR 0017 — one JSON body; `stream:true` is a 400 by design |
| Sticky sessions / conversation store | ADR 0017 — the client replays its own history; the gateway is stateless |
| Function calling (7 extraction strategies, tool sieve) | ADR 0016 — the protocol prompt costs 1,355–5,222 characters of a 102,400 budget, to emulate something the channel does not do |
| TLS/JA3 fingerprint impersonation | Owner ruling 2026-10-09: not needed (UA + client hints + device fields are aligned) |
| Remaining-quota display | MiMo has no quota concept; the pool's EWMA/cooldown/risk events are our real signal |
| Anti-detection / stealth / headless-browser stacks | Those projects reverse a browser; our login chain is protocol-level and already lands |
| Web management UI | We have `-accounts` CLI + `/v1/admin/*` JSON; a UI is aesthetic work, not capability (and this project's taste call is the owner's) |

## 4. Where we are already ahead — do not regress these

- **Leases**: per-request capacity with a TTL, slot-hold policy, release before the
  response write, queue with a wait budget — the competitors' "round-robin + retry" is
  strictly weaker.
- **Cooldown causes and risk-control recording** (`/v1/admin/pool` names *why* an
  account is parked); the others park silently.
- **Self-healing credentials**: 12 h keepalive, async password re-login on revocation,
  and an OTP that only a human completes (ADR 0013/0014 — never an automatic SMS).
- **Measured limits** rather than guesses: 102,400 characters exactly, fastchat 50,000,
  TTS 1024, attachments ~100k.
- **Catalog hygiene**: superseded ids resolved via `redirectTo` instead of advertised.
- **Zero third-party Go dependencies** (our own RESP2 client), one static binary,
  no Node/Python/JS runtime.
- **Process**: ADR/TDD/coverage floors, `make hygiene` proving no real credential is
  tracked, and a mirror that publishes a scrubbed tree as one fresh commit.

## 5. Suggested order

1. **Probe first, then P1** — confirm whether a served model accepts images. If yes,
   wire `attach.go`; if no, write the finding down and close the question.
2. **P2 multi-instance accounting** — cheap honesty fix, or a documented constraint.
3. **P2 long-conversation compression** — the only item that makes a currently-failing
   request succeed.
4. **P3 observability headers + /metrics**, then packaging.
