# mimo2api — Software Design Document

**Project:** mimo2api — OpenAI-compatible API gateway for Xiaomi MiMo AI Studio
**Upstream:** https://aistudio.xiaomimimo.com/ (MiMo AI Studio web chat)
**Status:** Design (no implementation yet)
**Date:** 2026-10-07

---

## 1. Overview & Goals

mimo2api is a self-hosted gateway that reverse-proxies MiMo AI Studio's web chat into the
OpenAI Chat Completions protocol. Any OpenAI-SDK-compatible client (cherry studio, one-api,
langchain, curl) points at mimo2api and chats with MiMo models as if they were OpenAI models.

**In scope:**

- `POST /v1/chat/completions` — streaming (`stream:true`) and non-streaming
- `GET /v1/models` — model catalog
- Bearer-key authentication (one or more API keys)
- Multi-account pool with rotation, health states, cooldown, and auto-recovery
- **Streaming stickiness**: same OpenAI conversation maps to the same account + upstream
  MiMo conversation, TTL-based
- Automatic login (phone/password → Xiaomi passport → STS cookies) and automatic
  session renewal (passToken → fresh serviceToken)
- Usage accounting from upstream token events

**Out of scope:**

- **Tool calling / function calling** — not supported. Requests carrying `tools` /
  `tool_choice` / `functions` get the fields stripped (and a warning header), not rejected.
  (References implement prompt-injected tool emulation — `mimo-web-proxy/internal/toolcall/toolparse.go`,
  `mimo-web-api/src/toolcall.js` — we deliberately do not replicate that complexity.)
- Multimodal inputs (images/files). The upstream upload flow is documented in
  `mimo-web-proxy/internal/mimo/media.go:22-31` if ever needed; v1 sends `multiMedias: []`.
- Anthropic-protocol endpoints, embeddings, audio, web-search toggle exposure (upstream
  supports `webSearchStatus` — see §2.4 — but v1 pins it to `"disabled"`).
- Multi-instance deployment / shared pool state across processes.

### What the four reference repos actually are

Grounding note — the references are **not** four implementations of one protocol:

| Reference | Upstream surface | Relevance to mimo2api |
|---|---|---|
| `mimo-web-api` (Node) | `aistudio.xiaomimimo.com` `/open-apis/*` + `/fastchat/*` | **Primary protocol source.** Full login, STS refresh, chat payload, SSE, OpenAI mapping. Ships `PROTOCOL.md`, a written reverse-engineering report. |
| `mimo-web-proxy` (Go) | Same aistudio surface | **Confirming protocol source + sticky-session design.** Independent implementation; conversation fingerprint store; retry/backoff; char limits. |
| `xm2api` (Node) | `mimo-server-cn.xiaomimimo.com/api/route/*` (OpenAI-shaped mirror, desktop-client sid `mimopc`) | Different surface, byte-passthrough. Useful for: Xiaomi passport login variants, account-file format, pool cooldown/renewal, 401-renewal event model. Its `docs/architecture.md` documents that route auth needs `serviceToken + userId`, not `passToken`. |
| `simple-chat` (Go) | `chat.deepseek.com` (DeepSeek, **not MiMo**) | Not a MiMo protocol source at all. Useful purely as **account-pool engineering**: health states, lease/in-flight model, pre-first-byte retry ladder, error→HTTP mapping, park-deadline monotonicity. |

All MiMo wire-protocol claims below cite `mimo-web-api` or `mimo-web-proxy` unless noted.

**Implementation-language bias:** `mimo-web-proxy` and `simple-chat` are both **Go**.
Since mimo2api is now Go (§3, §6), their code is not just protocol evidence — it is
directly borrowable engineering: `mimo-web-proxy`'s `internal/convstore` (fingerprint
store, §4.3), its SSE/idle-timeout handling (`internal/handler/chat.go`), and
`simple-chat`'s pool lease model (`pool.go` acquire/wait/exclude, in-flight semaphores)
port nearly line-for-line. Their `httptest.Server`-based test suites are also the
template for ours (§8.4).

---

## 2. MiMo upstream protocol (distilled from references)

### 2.1 Identity model

Three credential layers, per `mimo-web-api/PROTOCOL.md:105-130` and
`mimo-web-api/src/session.js:7-21`:

- **Xiaomi passport account** — phone + password. Long-lived.
- **RT (refresh token) = `passToken`** — cookie on `.xiaomi.com` domain, valid for months
  (`xm2api/docs/architecture.md:339-342` confirms ~1 month on `.account.xiaomi.com`).
  This is what we persist per account.
- **AT (access token) = `xiaomichatbot_serviceToken`** — httpOnly cookie on
  `aistudio.xiaomimimo.com`, expires. Regenerated from `passToken` via STS (§2.3).
- **App token = `xiaomichatbot_ph`** — a base64 value; sent **both** as a cookie and as a
  query parameter `?xiaomichatbot_ph=...` on every upstream POST
  (`mimo-web-api/PROTOCOL.md:8-11`, `mimo-web-api/src/upstream.js:40-46`,
  `mimo-web-proxy/internal/mimo/web_client.go:148`).

Full working cookie string for an upstream call (`mimo-web-proxy/internal/mimo/web_client.go:55-62`):

```
userId=<id>; serviceToken=<tok>; xiaomichatbot_serviceToken="<tok>"; xiaomichatbot_ph="<ph>"
```

`userId + xiaomichatbot_ph` alone (without httpOnly cookies) yields **401** on
`/open-apis/*` — the httpOnly `serviceToken` cookie is mandatory
(`mimo-web-api/PROTOCOL.md:11`).

### 2.2 Login flow (phone/password → session cookies)

Implemented in `mimo-web-api/src/xiaomi-login.js`; independently in
`xm2api/lib/login.mjs:238-297` (unencrypted variant) and `xm2api/lib/login.mjs:341-437`
(SMS/OTP new-device variant).

1. **Login context probe.** `GET {aistudio}/open-apis/user/mi/get` with
   `redirect: manual` → 401 body carries `loginUrl`
   (`https://account.xiaomi.com/pass/serviceLogin?callback=...&sid=xiaomichatbot`).
   Parse `callback, sid, qs, _sign, serviceParam, _group` from it
   (`xiaomi-login.js:123-152`).
2. **Password auth.** `POST https://account.xiaomi.com/pass/serviceLoginAuth2` with
   form fields `sid, callback, qs, _sign, serviceParam, user, hash, _json=true, ...`
   (`xiaomi-login.js:206-228`).
   - `hash = MD5(password).hex().toUpperCase()` — no salt (`xiaomi-login.js:219`;
     same in `xm2api/lib/login.mjs:37`).
   - The web client additionally AES-128-CBC-encrypts writable fields (random 16-char
     key, fixed IV `0102030405060708`) and sends an `EUI` header =
     `base64(RSA_PKCS1_v1_5(base64(key))) + "." + base64(field-name-list)`
     (`xiaomi-login.js:25-70`). The unencrypted form-encoded variant in xm2api also
     works (`xm2api/lib/login.mjs:259-268`); we implement the plain form variant first
     and keep the EUI variant as fallback if risk control starts rejecting plain posts.
   - Responses are `&&&START&&&{...json...}` XSSI-prefixed
     (`xiaomi-login.js:72-75`, `xm2api/lib/login.mjs:36`).
   - `code === 0` + `location` → success. `captchaUrl` / `notificationUrl` → captcha or
     new-device verification needed (`xm2api/lib/login.mjs:281-290`). **No auto-solver**;
     these surface as account-state errors (§7).
3. **STS exchange.** Follow `location` (one-time ticket, host must be aistudio or
   account.xiaomi.com) → response `Set-Cookie` plants `xiaomichatbot_serviceToken`,
   `xiaomichatbot_ph`, `userId` (`xiaomi-login.js:249-268`; `PROTOCOL.md:117-127`).
4. **Persist per account:** `userId`, `passToken` (from passport cookie jar,
   `xiaomi-login.js:242-246`), and the derived cookie string + `ph`.

> **Amendment (2026-10-08, live smoke test).** The new-device OTP demand also
> arrives with `code=0`: `securityStatus=128`, empty `location`,
> `notificationUrl` set. `Login()` treats that combination as an OTP demand
> (auto-start unless `MIMO2API_OTP_AUTO=false`), NOT a "no location" error.
> The OTP flow (reference `xm2api/lib/login.mjs:341-470`):
> 1. **Walk the `notificationUrl` chain by hand** (no auto-follow: the
>    intermediate `Location` values carry `sign`/`token`/`context` that the
>    page URL drops). Two shapes exist:
>    - *direct*: the `notificationUrl` already is
>      `/fe/service/identity/authStart?sid=..&context=..`;
>    - *cross-region consent* (non-mainland egress IP, `securityStatus=128`):
>      `/pass2/redirect` → `/pass2/userinfo/start?bizType=UserCross…` →
>      `/fe/service/userCross?needClose=false&userId=..` → **`POST
>      /pass2/userCrossAgreement/update`** `{userId, status:1, needClose:false}`
>      (`status=1` = 同意; a repeat POST answers `code=21301` 认证失败) →
>      `/pass2/userinfo/check?callback=<authStart URL>` → `/identity/authStart`,
>      where `sid`/`context` come from the `callback` parameter. Auto-agreeing
>      is `MIMO2API_CROSS_REGION_AGREE` (default true; `false` disables the
>      account with an operator-action reason instead).
> 2. `GET /identity/list?sid=..&supportedMask=0&_locale=zh_CN&context=..` —
>    `flag` 4 = phone, 8 = email (a `data`-wrapped flag is merged). The masked
>    destination is **not** here: it comes from the step-3 answer
>    (`{"maskedPhone":"+86 182******16"}`).
> 3. `GET /identity/auth/verify{Phone|Email}?_flag={flag}&_json=true` —
>    trigger the verification session.
> 4. `POST /identity/auth/send{Phone|Email}Ticket` with form
>    `{retry:"0", icode:"", _json:"true"}` and **`X-Requested-With:
>    XMLHttpRequest` (mandatory — without it the email send always fails
>    66108)** — the code goes to the account owner's phone/email.
> 5. `POST /identity/auth/verify{Method}` with form
>    `{_flag, ticket:<code>, trust:"true", _json:true}` + X-Requested-With
>    → `location`.
> 6. **Walk the landing chain to its end** (observed 2026-10-09):
>    `/identity/result/check` → 302 → `/pass/serviceLoginAuth2/end` → 302 →
>    `<upstream>/sts?sign=…` → 307 → `/open-apis/user/mi/get` → 200. The
>    `xiaomichatbot_*` session cookies and `passToken` are planted along the
>    chain (the session ones at the **upstream** hop), so single-hop following
>    yields `code=70016` at the next step. The session is composed from the
>    accumulated jar.
> 7. `userId`/`passToken` come from the jar; `/pass/serviceLogin` is kept only
>    as a fallback for passport variants whose landing location is a bare
>    intermediate.
>
> The challenge (id, method, masked notify, sid, cookie jar) lives in memory
> with a 10-minute expiry. Completion is out-of-band via the admin
> endpoints (§5.1): `GET /v1/admin/otp/pending` lists live challenges,
> `POST /v1/admin/otp/submit` `{id, code}` completes the login, persists
> the session, and re-enables the account. The OTP code never touches
> disk.

### 2.3 Session renewal (passToken → serviceToken), keepalive

`mimo-web-api/src/session.js:121-201` — fully automatic, no human in the loop:

1. Probe `GET /open-apis/user/mi/get` with `Cookie: userId=<id>;` → 401 body gives
   `loginUrl` (fallback loginUrl with a hardcoded `sign` param exists at
   `session.js:149-153`).
2. `GET {loginUrl}&_json=true` with `Cookie: userId=<id>; passToken=<rt>` →
   `&&&START&&&{code:0, location:"https://aistudio.xiaomimimo.com/sts?sign=..."}`.
   `code 70016` = passToken dead → account needs full re-login or eviction.
3. `GET location` → Set-Cookie plants fresh `serviceToken` / `ph` / `userId`.
4. Cache to disk (`data/session-cache.json` in the reference; we persist per-account in
   the accounts store, §4.7).

Session validation: `GET /open-apis/user/mi/get` → `{"code":0,"data":{"userId":...}}`
(`PROTOCOL.md:12`; `mimo-web-proxy/internal/mimo/web_client.go:305-328` also checks
business `code==0` inside the 200 body). `GET /open-apis/user/info` is dead (410) —
`web_client.go:302-304`.

Keepalive: reference runs a 6h interval that validates cached sessions and refreshes
only when invalid (`session.js:203-230`, `SESSION_REFRESH_HOURS` default 6,
`config.js:122`; `xm2api/server.mjs:892-910` runs the same 6h fallback renewal model
with 401-driven renewal as primary). mimo2api adopts: **event-driven refresh on 401 as
primary** (attempt once per request, §7), **timer sweep every 6h as fallback**, refresh
only on validation failure to minimize passport traffic.

### 2.4 Chat completion request

- Normal models: `POST https://aistudio.xiaomimimo.com/open-apis/bot/chat?xiaomichatbot_ph=<ph>`
- UltraSpeed models only: `POST https://aistudio.xiaomimimo.com/fastchat/open-apis/bot/chat?...`
  — sending ultraspeed through the normal channel returns a model-name error
  (`mimo-web-proxy/internal/mimo/web_client.go:24-25,144-147`). Routing rule: model id
  matches `/ultraspeed/i` (`mimo-web-api/src/config.js:137`).
- **No separate "create conversation" endpoint** — the first message with a fresh
  `conversationId` auto-creates the conversation (`PROTOCOL.md:19`).

Payload (`mimo-web-api/src/upstream.js:140-164`; confirmed
`mimo-web-proxy/internal/mimo/web_client.go:95-140`):

```jsonc
{
  "msgId": "<32-hex, NEW for every message/retry>",       // upstream dedupes by msgId
  "conversationId": "<32-hex, stable per conversation>",
  "query": "<user message text>",
  "isEditedQuery": false,
  "previousDialogueId": "<last turn's dialogId | null>",   // multi-turn chaining key
  "sceneType": null,
  "params": {},
  "modelConfig": {
    "model": "mimo-v2.6-pro",
    "enableThinking": false,
    "webSearchStatus": "disabled"                          // disabled | auto | enabled
  },
  "multiMedias": []          // MUST be an array, never null (web_client.go:121-125)
}
```

- `msgId`/`conversationId` = `uuid4` with dashes stripped (`mimo-web-api/src/utils.js:3-6`).
  **Every retry must regenerate `msgId`** — upstream rejects duplicates with
  "请求已处理，请勿重复提交" (`mimo-web-proxy/internal/mimo/web_client.go:154-168`).
- `previousDialogueId`: null on first turn; afterwards the `dialogId` SSE event value
  from the previous turn (`PROTOCOL.md:29,69`). MiMo keeps conversation context
  server-side keyed by `conversationId`.
- Headers: `Content-Type: application/json`, `Accept: application/json,
  text/plain, */*`, `X-Requested-With: XMLHttpRequest` (**both are axios
  defaults in the Studio SPA — `docs/login-chain-reverse.md` §3**),
  `Accept-Language: zh-CN`,
  `x-timeZone: Asia/Shanghai`, `Origin`/`Referer` = aistudio base, Chrome UA,
  full `Cookie` (`mimo-web-api/src/upstream.js:28-38`;
  `mimo-web-proxy/internal/mimo/web_client.go:37-49` adds `sec-ch-ua`/`sec-fetch-*` —
  UA version should match `sec-ch-ua` for risk-control consistency, `web_client.go:30-33`).

### 2.5 SSE response format

`text/event-stream`, colon **without** space (`PROTOCOL.md:43-63`). Parser reference:
`mimo-web-api/src/upstream.js:72-123` (tolerates `event:x` and `event: x`, `\n` and `\r\n`).

```
id:<32-hex>
event:dialogId
data:{"type":null,"content":"<this turn's id>","usage":null}

event:message
data:{"type":"text","content":"<delta — may embed thinking markers>","usage":null}

event:usage
data:{"promptTokens":2242,"completionTokens":78,"totalTokens":2320,"nativeUsage":{...}}

event:finish
data:{"type":null,"content":"[DONE]","usage":null}
```

| Event | Meaning | Handling |
|---|---|---|
| `dialogId` | This turn's dialogue id | Store → next turn's `previousDialogueId` |
| `message` | Text delta (`type:"text"`) | Split thinking markers, emit deltas |
| `usage` | Token counts; **emitted multiple times, last one wins** (`mimo-web-proxy/internal/handler/chat.go:156-159`) | Keep last |
| `error` | Upstream error, `content` = message | Fail turn |
| `sensitive_query` / `sensitive_title` | Content-moderation block | Fail turn with content-filter error |
| `web_search`, `doc`, `tip_ratio`, `tip_truncate` | Search/parse progress | Ignore (web search disabled in v1) |
| `finish` | `content:"[DONE]"` | End of turn |

**Reasoning markers**: thinking arrives inline in `message` deltas, delimited by
`<think>\0` … `</think>\0` (marker + NUL byte; front-end constants
`O="<think>\0"`, `U="</think>\0"` — `PROTOCOL.md:79-84`). Splitter reference:
`mimo-web-api/src/thinking.js` (3-state machine, strips NULs, tolerant of markers split
across chunk boundaries, unterminated block at stream end counts as reasoning).

### 2.6 Models

Source of truth: `GET /open-apis/bot/config` → `data.modelConfigListNg` (`PROTOCOL.md:88`).
Static list as of the references (`mimo-web-api/src/config.js:139-149`,
`mimo-web-proxy/internal/router/multimodal.go:9-15`):

`mimo-v2.6-pro-ultraspeed-studio` (fastchat), `mimo-v2.6-pro`, `mimo-v2.6-flash`,
`mimo-v2.5-pro`, `mimo-v2.5`, `mimo-v2.1-pro`, `mimo-v2.1-pro-preview`,
`mimo-v2-pro`, `mimo-v2-flash`.

mimo2api: static default list in config, with optional live refresh from
`/open-apis/bot/config` (cached 5 min, like `xm2api/server.mjs:280-317`) — plus alias
normalization (strip `-studio` suffix etc., per `multimodal.go:50-85`).

### 2.7 Rate limits & anti-bot considerations

- Upstream HTTP semantics: `302/401` session expiry (body carries `loginUrl`), `429`
  rate limit, `451/461` account restricted — 461 permanent (`PROTOCOL.md:132-139`).
- Per-account parallelism is limited upstream (simple-chat's `parallel_chat_limit`
  analogue; MiMo expresses it as 429). We cap per-account in-flight requests instead of
  discovering the limit by being banned.
- Query size caps measured by `mimo-web-proxy/internal/router/multimodal.go:130-137`:
  **45,000 chars** for ultraspeed (fastchat channel; 48,750 OK / 50,000 rejected),
  **90,000 chars** for others (~100K rejected). Enforce client-side with a clear error.
- `msgId` dedup (§2.4), UA/`sec-ch-ua` consistency, `x-timeZone` header, and the
  `xiaomichatbot_ph` query param are all risk-control-relevant. Keepalive comment
  frames (`: keepalive\n\n`) keep intermediaries from closing idle streams
  (`mimo-web-api/src/openai.js:313-319`).
- Xiaomi passport may demand slider captcha (`captchaUrl`) or new-device OTP
  (`notificationUrl`) at login (`xiaomi-login.js:19-20`, `xm2api/lib/login.mjs:281-290`).
  No auto-solve: the account goes to `disabled` with a human-readable reason; admin
  re-login resolves it.

---

## 3. Architecture

Single Go binary, one process, concurrency via goroutines. HTTP on the standard
library: `net/http` with a hand-rolled SSE writer (`http.Flusher` + `bufio.Writer`);
routing via `http.ServeMux` (Go 1.22+ method+pattern routing) — `chi` is acceptable if
middleware chaining ever earns its dependency, but v1 ships stdlib-only.

```
┌────────────────────────────────────────────────────────────┐
│                  net/http server (main.go)                 │
│                                                            │
│  POST /v1/chat/completions      GET /v1/models             │
│  GET /v1/models/{id}            GET /healthz               │
│         │                              │                   │
│  ┌──────▼───────────────┐    ┌─────────▼─────────┐         │
│  │  internal/api        │    │  internal/api      │         │
│  │  (OpenAI layer)      │    │  auth middleware   │         │
│  │  request validation  │    │  Bearer key check  │         │
│  │  response mapping    │    │  (constant-time)   │         │
│  │  SSE writer (flush)  │    └───────────────────┘         │
│  └──────┬───────┬───────┘                                │
│     non-stream│stream                                    │
│         │     │                                          │
│  ┌──────▼─────▼───────────────┐   ┌────────────────────┐ │
│  │ internal/session           │   │ internal/session   │ │
│  │ (conversation router)      │◄─►│ (sticky store:     │ │
│  │ fingerprint → sticky slot  │   │  TTL + LRU map)    │ │
│  │ or round-robin pick        │   └────────────────────┘ │
│  └──────┬─────────────────────┘                          │
│  ┌──────▼─────────────────────┐   ┌────────────────────┐ │
│  │ internal/pool              │──►│ internal/accounts  │ │
│  │ (account pool)             │   │ (store + loader)   │ │
│  │ states, cooldown, probe    │   └─────────┬──────────┘ │
│  └──────┬─────────────────────┘             │            │
│  ┌──────▼─────────────────────┐   ┌─────────▼──────────┐ │
│  │ internal/mimo              │──►│ internal/mimo       │ │
│  │ (client: chat, SSE,        │   │ (login: passport    │ │
│  │  validate, models)         │   │  + STS renewal)     │ │
│  └────────────────────────────┘   └────────────────────┘ │
│  ┌─────────────────────────────┐                          │
│  │ internal/adapter            │  thinking splitter,     │
│  │ (OpenAI ⇄ MiMo)             │  usage mapping, chunks  │
│  └─────────────────────────────┘                          │
└────────────────────────────────────────────────────────────┘
```

Packages (Go module `mimo2api`, layout per standard Go project idiom):

| Package | Responsibility |
|---|---|
| `internal/api` | HTTP handlers. Validate OpenAI request shape (manual decoding — OpenAI clients send extra fields, so decode into a permissive struct, never a strict schema), auth middleware, emit OpenAI response / SSE. No upstream logic. |
| `internal/pool` | Account pool: states, selection (round-robin + sticky lookup), cooldown, recovery probe, per-account in-flight caps. One `sync.Mutex` guards the whole pool (pool ops are microseconds; a single mutex beats per-account locking at this scale — simple-chat's `pool.go` does the same). Acquire/wait via `sync.Cond` or channel-based semaphore with `context` timeout. |
| `internal/session` | Sticky-session store: conversation fingerprint → `{accountID, conversationID, previousDialogID}` with TTL + LRU. Own mutex, background janitor goroutine. |
| `internal/adapter` | Pure translation: OpenAI messages ⇄ MiMo query turns; thinking-marker splitter (3-state machine over byte chunks); usage mapping; chunk shaping. Pure functions — unit-testable without network. |
| `internal/accounts` | Account credentials store: load/save `data/accounts.json` (§4.7) with atomic write + fsync, 0600, hot-reload. |
| `internal/mimo` | Upstream client: `bot/chat` (stream + collect), SSE byte parser (tolerant of `event:x`/`event: x`, `\n`/`\r\n`), `/open-apis/user/mi/get` validation, `/open-apis/bot/config` model fetch; plus Xiaomi passport login (§2.2) and STS renewal (§2.3). One `http.Client` per account (distinct cookie jars). |
| `internal/config` | Env-var config, model list, API keys. Parsed once at startup, immutable thereafter. |
| `cmd/mimo2api` | `main()`: parse config, load accounts, start pool janitors + session sweeper goroutines, `http.ListenAndServe`. Graceful shutdown on SIGTERM/SIGINT (`srv.Shutdown(ctx)`) so in-flight streams drain before the container stops. |

Concurrency model:

- **One goroutine per request** (net/http's own model — no manual pooling).
- **Cancellation** is `context.Context` end-to-end: client disconnect cancels the
  request context (`r.Context()`), which cancels the upstream request, which releases
  the pool slot. No bespoke AbortController equivalent needed — this is precisely the
  cleanup path `mimo-web-api/src/openai.js:305-326` implements by hand in Node.
- **Shared state** (pool, session store) behind mutexes; no channels-as-queues in v1.
  Background loops (recovery probes every 60s, session sweep, 6h STS keepalive — §2.3)
  are plain `time.Ticker` goroutines started in `main`, stopped via the shutdown context.
- **SSE writing**: set headers, then loop `fmt.Fprintf(w, "data: %s\n\n", chunk)` +
  `flusher.Flush()` after each event. Keepalive comments (`: keepalive\n\n`) on a
  `time.Ticker` when upstream is silent — same wire behavior as
  `mimo-web-api/src/openai.js:313-319`.

Key data flow — streaming request:

1. `internal/api` validates request + key.
2. Session router computes the fingerprint of the message history.
   - Streaming: `session.Lookup(fingerprint)` → hit? sticky account+conversation.
     Miss? `pool.Acquire(ctx)` round-robin, new `conversationId`.
   - Non-streaming: always `pool.Acquire(ctx)` round-robin, always a **fresh**
     `conversationId` (no stickiness).
3. `adapter` builds the upstream turn plan (§5.3).
4. `mimo.Client.Chat(ctx)` streams SSE events (scanner over the response body, byte
   reader wrapped for the thinking-splitter); `adapter` converts events to OpenAI
   chunks incrementally; the handler writes and flushes each one.
5. On `dialogId`/`usage`/`finish`: update `session` (sticky path), `pool`
   health, usage stats.

---

## 4. Account pool design

This is where the four references diverge most, so each decision cites its origin.

### 4.1 Account states

```
                 ┌────────────┐  429 / auth fail (recoverable)
   ┌────────────►│  available │◄───────────────────────────┐
   │             └─────┬──────┘                            │
   │ cooldown expires  │ acquire                          │
   │ + probe OK        │                                   │
   │             ┌─────▼──────┐   401 that refresh fixes   │
   │             │  in-flight │────────────────────────┐   │ (transparent)
   │             └─────┬──────┘                        │   │
   │    soft failure   │        hard failure           ▼   │
   │             ┌─────▼──────┐  ┌─────────────┐  refresh OK
   └─────────────│  cooldown  │  │  disabled   │───────────┘
                 └────────────┘  └─────────────┘
                  (429, 5xx)      (passToken dead, captcha
                                   required, 451/461 ban)
```

- **available** — selectable.
- **cooldown** — temporarily excluded; auto-recovers (§4.5).
- **disabled** — excluded until an admin/manual action or a successful re-login.
  Includes: dead `passToken` (STS returns `70016`, `PROTOCOL.md:121`), captcha/OTP
  demanded at login, upstream `451/461` restriction (461 is permanent per
  `PROTOCOL.md:139`).

No `muted`/`risk` split in v1 — MiMo's observed failure vocabulary (429/401/451/461,
`mimo-web-api/src/upstream.js:197-203`) doesn't include DeepSeek-style `biz_code`
mute deadlines. If MiMo later returns structured mute deadlines, `cooldown` gains an
`until` timestamp (the simple-chat park model, `pool.go:201-207` +
`client.go:1212-1217`, is the template).

### 4.2 Selection: two paths

**Non-streaming — round-robin, no stickiness.**
Plain cursor rotation over `available` accounts, skipping cooldown/disabled
(the `CookiePool.pick` model, `mimo-web-api/src/utils.js:52-71`; same in
`xm2api/lib/accounts.mjs:79-89`). If all accounts are cooling down, mimo2api **rejects
with 429** rather than hammering a rate-limited account (references differ here —
mimo-web-api and xm2api "still pick one" (`utils.js:74-75` "别断粮"), simple-chat
fails closed (`pool.go:651-655`); we follow simple-chat because retrying into a 429
extends the limit window).

**Streaming — sticky via conversation fingerprint.**
The OpenAI protocol is stateless: there is no conversation id in the request. "Same
conversation" is inferred from message history, exactly as `mimo-web-proxy` does
(`internal/convstore/convstore.go`):

- **Fingerprint** = SHA-256 over the last ≤5 non-`system` messages'
  `(role, content)` pairs, content truncated to 200 chars each, first 16 bytes hex
  (`convstore.go:97-118`).
- A stored session matches when the current message list *extends* a stored one
  (continuation test: some prefix of the current list equals the stored fingerprint,
  `convstore.go:124-149`) **and** the non-system message count strictly increased
  (replay guard — identical history re-sent is a regeneration, not a continuation,
  `convstore.go:172-174`).
- On match, reuse `{account_id, conversation_id, previous_dialog_id}` and send only
  the **delta** messages (messages after the stored frontier). On miss, acquire
  round-robin, mint a fresh 32-hex `conversationId`, replay the full turn chain (§5.3).
- Multiple matches → newest `UpdatedAt` wins (`convstore.go:159-170`).

**Why stickiness matters for streaming**: MiMo context lives server-side per
`conversationId`, and `previousDialogueId` chaining only works within one account's
conversation. Rotating accounts mid-conversation would either lose context or force a
full-history replay every turn — expensive (multiplied token usage) and
fingerprint-fragile. Non-streaming requests in v1 don't chain turns across calls (§5.3),
so they need no stickiness — which also makes them the natural load-shedding path.

### 4.3 Sticky-session store

In-process, with optional JSON persistence for restart survival
(`mimo-web-proxy` persists to `data/conversations.json`, `convstore.go:39-60`).

```
key:   conversation_key = account_id + ":" + conversation_id
value: {
  account_id,            # sticky account
  conversation_id,       # 32-hex MiMo conversation
  previous_dialog_id,    # last turn's dialogId (chaining anchor)
  fingerprint,           # SHA-256 of message frontier (convstore.go:97-118)
  msg_count,             # non-system message count at frontier (replay guard)
  created_at, updated_at # unix seconds
}
secondary index: fingerprint → conversation_key (newest first)
```

- **TTL (default 30 min, idle-based)**: entry expires when `now - updated_at > TTL`.
  On expiry the next request for that conversation is a miss → new account pick +
  fresh `conversationId` + full chain replay. Client-visible effect: none (the full
  history is replayed upstream, so the answer still has full context), except higher
  token usage for that one turn. Rationale: MiMo conversations are server-side rows;
  30 idle minutes is well within observed serviceToken lifetime and keeps the store
  from pinning accounts indefinitely. (References had **no** TTL — `convstore.go` uses
  LRU-50 only; we add TTL because our pool rotates accounts and stale pins would
  starve rotation.)
- **LRU cap (default 500 conversations)**, evicting oldest `updated_at`
  (scaled-up version of `convstore.go:63-79`).
- **Mid-stream account failure** (sticky account dies while streaming): the commit
  boundary rule (§7.3) applies — after the first byte to the client we do not switch
  accounts or replay; the stream terminates with an error. Before the first byte, the
  sticky entry is discarded and the request restarts on another account with a fresh
  conversation (full replay). The stale sticky entry is deleted in both cases.
- **TTL expiry mid-conversation**: next turn is simply a store miss — handled by the
  miss path above. No client error, no stickiness leak.

### 4.4 Concurrency & in-flight caps

Per-account semaphore, default `max_inflight = 2` (simple-chat
`DefaultMaxInflight = 2`, `pool.go:18` — same upstream parallelism concern). Acquire
waits up to `queue_wait` (default 15s; simple-chat uses 30s, `pool.go:162-163`, we
default shorter because our clients are interactive). Timeout → `429 pool_busy` with
`Retry-After` (error mapping from simple-chat `server.go:923-959`).

Selection score (near-best-of-N, per simple-chat `pool.go:612-614, 662-680`):
`score = (inflight/capacity)*100 + ewma_latency_seconds + warm_penalty`, choose
randomly among accounts within `best*1.15+1`. EWMA alpha 0.2, observed per request
from first-byte latency. (simple-chat implemented the EWMA term but never wired it —
`pool.go:683-700` has no production caller; we wire it, it's two lines.)

### 4.5 Cooldown & auto-recovery

- **Cooldown on 429**: `cooldown_until = now + 60s`, doubling per consecutive failure,
  capped at 10 min (`mimo-web-api/src/utils.js:100-106`). Success resets the counter.
- **Cooldown on 5xx/transport**: same ladder, starting at 30s.
- **Auth failure (401/302)**: first attempt is transparent renewal — refresh via STS
  and retry once (§2.3; `mimo-web-api/src/openai.js:89-124` does exactly one
  refresh-retry, only when nothing has been streamed yet). Renewal failure (70016 /
  repeated 401) → account `disabled`.
- **Recovery probe**: a cooldown that expires does not immediately return to full
  service. A lightweight background probe — `GET /open-apis/user/mi/get` session
  check, like `mimo-web-proxy`'s health check (`web_client.go:305-328`) — flips the
  account back to `available` only when the probe succeeds. Probes run on a 60s sweep
  over expiring/expired accounts, staggered to avoid synchronized bursts. If the probe
  itself 401s, trigger one STS renewal before disabling.

### 4.6 Health transitions summary

| Upstream signal | Pool action |
|---|---|
| 200 + SSE completes | success — reset failure counter |
| 429 | cooldown ladder (60s → 10min) |
| 401 / 302 (pre-first-byte) | STS refresh + retry once; second failure → disabled |
| STS `70016` / passToken rejected | disabled (needs re-login; admin surface or manual file edit) |
| 451 | disabled with reason (account restricted) |
| 461 | disabled, permanent flag (never auto-recovers) |
| 5xx / transport / stream stall > 180s (`chat.go:62,112-120`) | cooldown from 30s |
| `event:error` / `sensitive_query` mid-stream | fail the request; no pool action (content-level, not account-level) |
| Empty response, no error | one retry with fresh `msgId` + fresh `conversationId` (`mimo-web-proxy/internal/handler/chat.go:283-324` empty-response rotation) |

### 4.7 Accounts file format

`data/accounts.json` (mode 0600, like simple-chat `json.go:54-56`), hot-reloadable:

```jsonc
{
  "accounts": [
    {
      "id": "<phone>",           // stable pool identity
      "phone": "<phone>",
      "password": "<redacted>",  // passport login; see §8
      "enabled": true,
      "label": "test",
      // runtime state (managed by the server, safe to persist):
      "user_id": "...",           // Xiaomi userId after first login
      "pass_token": "...",        // RT from passport (preferred if present)
      "session": {                // derived, refreshable; absent = login on demand
        "cookie": "userId=...; serviceToken=...; ...",
        "ph": "...",
        "fetched_at": 0
      },
      "state": "available",       // available | cooldown | disabled
      "cooldown_until": null,
      "disabled_reason": null
    }
  ]
}
```

Design notes:

- Password lives in the file, never in code (§8). Both `pass_token` (preferred, from a
  prior login or manual paste) and `password` are supported — mirroring mimo-web-api's
  dual `cookie`/`account` entry types (`config.js:47-94`).
- Per-account single files (xm2api style, `data/accounts/<user_id>.json`,
  `xm2api/lib/accounts.mjs:23-37`) were considered; one file wins for a small pool —
  atomic write, one fsync, trivial backup.
- Runtime state is persisted back so restarts don't re-login the whole pool
  (session cache concept from `mimo-web-api/src/session.js:69-98`).
- Unknown/duplicate ids: last enabled entry wins, earlier duplicates logged
  (simple-chat joins duplicate identities to one semaphore, `pool.go:349-382` —
  we reject duplicates at load instead, simpler and louder).

---

## 5. API surface

### 5.1 Endpoints

| Route | Behavior |
|---|---|
| `POST /v1/chat/completions` | Chat. `stream:true` → SSE; else JSON. |
| `GET /v1/models` | Model list (§2.6), OpenAI `list` shape. |
| `GET /v1/models/{id}` | Single model or 404 with available ids (`xm2api/server.mjs:751-776`). |
| `GET /healthz` | Liveness + pool summary (states only, no credentials — xm2api `summaryOf` precedent, `lib/accounts.mjs:92-107`). |

Auth: `Authorization: Bearer <key>` checked against `API_KEYS` (comma-separated).
Missing/invalid → 401 OpenAI-shaped error (`{"error":{"message","type":"invalid_request_error","code":"invalid_api_key"}}`,
shape per `mimo-web-proxy/main.go:225`, `simple-chat/server.go:235-257`). If no keys
configured, auth is open (bind-local default). Constant-time comparison.

### 5.2 Request mapping (OpenAI → MiMo)

Supported request fields:

| OpenAI field | Mapping |
|---|---|
| `model` | Normalize aliases (strip `-studio`, fuzzy match, `multimodal.go:50-85`) → upstream model id; `auto`/missing → `default_model`. Fastchat routing if `/ultraspeed/i`. |
| `messages` | Turn planning, §5.3. `content` as string or `[{type:"text",...}]` parts (text parts joined, `mimo-web-api/src/openai.js:36-45`). Image parts rejected with 400 in v1. |
| `stream` | Streaming vs collected delivery. |
| `temperature`, `top_p` | Forwarded inside `modelConfig` (mimo-web-proxy sends `temperature:0.8, top_p:0.95` defaults, `web_client.go:127-140`); pass through when provided, else omit. |
| `max_tokens` / `max_completion_tokens` | Not supported upstream; ignored (warning header). |
| `tools`, `tool_choice`, `functions`, `response_format`, `n`, `seed`, ... | Stripped, not rejected. `X-Mimo-Warnings` response header lists dropped fields. |

`reasoning_effort`/thinking: expose a non-standard opt-in — request field
`enable_thinking: true` (or `reasoning: {"type":"enabled"}` a la simple-chat
`openai.go:355-417`) → `modelConfig.enableThinking: true` → reasoning surfaced as
`reasoning_content` deltas (marker splitting per §2.5). Default: off.

### 5.3 Turn planning (the multi-turn problem)

MiMo has no "send full message array" API — one `query` per call, context held
server-side per `conversationId`, chained by `previousDialogueId`. The references
solved this three ways; we use two of them:

**A. Sticky hit (streaming, store match):** send only the delta — the user message(s)
after the stored frontier. One upstream call per new user message, each chained.
Cheapest path; this is mimo-web-proxy's `DeltaMessages` strategy
(`internal/handler/serialize.go:205-224`).

**B. Store miss (or non-streaming): full chain replay.** For each `user` message in
order: one `bot/chat` call, `previousDialogueId` = previous turn's `dialogId`, first
null. All intermediate calls run with `stream:false`-style collection (we still parse
SSE; only the final call's answer is returned). This is mimo-web-api's loop
(`src/openai.js:181-242` — "for each query, call botChat... previousDialogueId =
lastDialogId"). Cap replayed turns at 20 (config) to bound upstream token burn;
beyond that, 400 with guidance.

**System messages:** NOT dropped (mimo-web-api drops them, `openai.js:59-61` —
silently changes behavior). Spliced into the first user query as a preamble:
`[system instructions]\n\n---\n\n<first user message>`, the same splice pattern
mimo-web-proxy uses for tool prompts (`chat.go:250-252`, `toolprompt.go:84-95`) and
simple-chat uses for system merges (`openai.go:266-345`). Multiple system messages
concatenate.

**Assistant messages in history (miss path):** the chain replay is driven by user
messages only; assistant text is not re-sent as a query. Consequence: on a miss, the
model sees only its prompts, not its own prior answers. This matches mimo-web-api's
behavior (`openai.js:52-64`) and is the accepted trade-off; the sticky path (A) is
where multi-turn fidelity lives, which is why streaming requests carry the
conversation store and non-streaming requests don't.

### 5.4 Response mapping (MiMo → OpenAI)

Non-streaming:

```jsonc
{
  "id": "chatcmpl-<uuid>",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "<requested model, echoed>",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "<answer text>",
      "reasoning_content": "<thinking, only if non-empty>"   // omitted otherwise
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 2242,          // sum across replayed turns
    "completion_tokens": 78,
    "total_tokens": 2320
  }
}
```

`finish_reason`: `"stop"` normally; `"length"` never (no upstream max-token control);
content-filter turns end `"stop"` with the filter message in `content` (or a
`"content_filter"` code in error details — see §7.4).

Streaming — OpenAI chunk sequence:

```
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","created":...,"model":"...",
       "choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {...,"choices":[{"index":0,"delta":{"reasoning_content":"..."},"finish_reason":null}]}
data: {...,"choices":[{"index":0,"delta":{"content":"..."},"finish_reason":null}]}

data: {...,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: {...,"choices":[],"usage":{"prompt_tokens":...,"completion_tokens":...,"total_tokens":...}}

data: [DONE]
```

- One `id` for the whole stream (`chatcmpl-<uuid>`), role delta only in the first
  chunk (mimo-web-api `openai.js:328-340`; simple-chat `server.go:704`).
- **Usage chunk carries `choices: []`** so it can't clobber `finish_reason`
  (bug documented in `mimo-web-proxy/internal/adapter/openai.go:152-160`).
- Thinking deltas as `reasoning_content` — incremental, true passthrough, unlike
  mimo-web-proxy which buffers the whole answer and emits one fat chunk
  (`chat.go:399-411`; we do not replicate that).
- `: keepalive` comment every 15s of upstream silence
  (`mimo-web-api/src/openai.js:313-319`, default `KEEPALIVE_INTERVAL_MS=15000`).
- Chunk shape `{id, object, created, model, choices:[{index, delta, finish_reason}]}`
  per `mimo-web-api/src/openai.js:75-83`.

Usage accounting: `prompt/completion/total` from the last `usage` SSE event
(§2.5). `nativeUsage.prompt_tokens_details.cached_tokens` and
`completion_tokens_details.reasoning_tokens` mapped into
`prompt_tokens_details`/`completion_tokens_details` when present
(`mimo-web-proxy/internal/handler/chat.go:36-57`). Missing usage (upstream omitted
it) → zeros, never invented (simple-chat logs "unknown" rather than fabricating,
`server.go:649-654`).

---

## 6. Tech stack

**Go 1.22+ / stdlib `net/http` / single static binary.** No web framework — Go 1.22's
`http.ServeMux` handles method+pattern routing (`mux.HandleFunc("POST /v1/chat/completions", ...)`),
which is all this API surface needs (§5.1). `chi` is pre-approved as the one acceptable
router dependency if middleware chaining becomes real; v1 ships stdlib-only. Zero
non-stdlib dependencies expected — the JSON, TLS, and SSE primitives are all in
`encoding/json`, `net/http`, and `bufio`.

Justification:

- **Two of the four references are Go** (`mimo-web-proxy`, `simple-chat`) — their pool
  lease model, convstore, SSE handling, and httptest suites port directly (§1 bias note).
- **Streaming + cancellation is native**: `http.Client` streaming reads + `context`
  propagation give incremental SSE bytes with clean client-disconnect cleanup — no
  async runtime to wrangle (§3 concurrency model).
- **Single static binary, scratch container**: no interpreter, no layers to patch,
  ~10 MB image. Deployment is `docker run` or a bare `scp` + systemd unit.
- **SSE parsing** is ~80 lines over a `bufio.Scanner`/`bufio.Reader`
  (reference logic: `mimo-web-api/src/upstream.js:72-123`; Go precedent:
  `mimo-web-proxy/internal/handler/chat.go`).
- Alternatives rejected: Python/FastAPI (fine, but the Go references are directly
  borrowable and the Docker footprint is smaller); Node (no edge over Go here given
  the references).

**Delivery: Docker.** Multi-stage build, distroless-style runtime:

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mimo2api ./cmd/mimo2api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mimo2api /mimo2api
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/mimo2api"]
```

(Alpine `FROM scratch` + `USER 65534` works too; distroless-nonroot is chosen for
`HEALTHCHECK`-adjacent tooling and a conventional non-root uid. `CGO_ENABLED=0`
guarantees the static binary runs on either.)

`docker-compose.yml`:

```yaml
services:
  mimo2api:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data          # accounts.json + runtime state (§4.7)
    environment:
      - MIMO2API_ACCOUNTS_FILE=/data/accounts.json
      - MIMO2API_API_KEYS=${MIMO2API_API_KEYS:-}
      - MIMO2API_LOG_LEVEL=info
    healthcheck:
      test: ["CMD", "/mimo2api", "healthcheck"]   # in-binary check hitting GET /healthz
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    restart: unless-stopped
```

Healthcheck note: distroless has no `curl`/`wget`, so the binary exposes a tiny
`mimo2api healthcheck` subcommand that does `GET http://127.0.0.1:8080/healthz`
and exits 0/1 — a common static-binary pattern.

Config surface — env vars, prefixed `MIMO2API_` to stay collision-free in compose
files (defaults):

```
MIMO2API_HOST=0.0.0.0                # empty API_KEYS forces 127.0.0.1 bind (§5.1)
MIMO2API_PORT=8080
MIMO2API_API_KEYS=                   # comma-separated; empty = open (bind-local)
MIMO2API_UPSTREAM_BASE=https://aistudio.xiaomimimo.com
MIMO2API_ACCOUNTS_FILE=/data/accounts.json
MIMO2API_DEFAULT_MODEL=mimo-v2.6-pro
MIMO2API_REQUEST_TIMEOUT_MS=300000   # upstream idle/stream timeout (mimo-web-api config.js:127)
MIMO2API_SSE_IDLE_TIMEOUT_S=180      # no-event watchdog (mimo-web-proxy chat.go:62)
MIMO2API_SESSION_REFRESH_HOURS=6     # timer sweep (session.js:203-230)
MIMO2API_STICKY_TTL_S=1800           # §4.3
MIMO2API_STICKY_MAX=500
MIMO2API_MAX_INFLIGHT_PER_ACCOUNT=2
MIMO2API_QUEUE_WAIT_S=15
MIMO2API_KEEPALIVE_INTERVAL_MS=15000
MIMO2API_X_TIMEZONE=Asia/Shanghai
MIMO2API_USER_AGENT=<Chrome UA, matched to sec-ch-ua>
MIMO2API_LOG_LEVEL=info
```

Default `ACCOUNTS_FILE` moves under `/data` (the container volume mount) rather than
the source-tree `data/` — same file, container-native path.

---

## 7. Failure handling

### 7.1 Classification

| Class | Signals | Retry? | Pool action |
|---|---|---|---|
| Auth-expired | 401/302 pre-first-byte, STS refreshable | 1 transparent retry after refresh | refresh; fail → disabled |
| Rate-limited | 429 | no (this request) | cooldown ladder 60s→10min |
| Upstream sick | 5xx, transport, non-SSE 200 body (`upstream.js:208-223`), stall > 180s | 1 retry, another account, pre-first-byte only | cooldown from 30s |
| Account dead | 451/461, STS 70016, login captcha/OTP demand | no | disabled (+permanent flag for 461) |
| Content-level | `event:error`, `sensitive_query` | no | none — fail request |
| Empty response | SSE completes, zero text, no error | 1 retry, fresh `msgId` + fresh `conversationId`, full replay (`chat.go:283-324`) | none |

### 7.2 Retry policy

- **Pre-first-byte only** for account switching. Once any byte (headers or body) has
  reached the client, no account switch, no replay — the stream terminates with an
  explicit error frame. This is simple-chat's commit boundary
  (`server.go:745-771`, pinned by `stream_empty_retry_test.go:87-122`) and it's the
  correct contract: a client that saw deltas must never see them restart.
- Max 2 upstream attempts per request (original + 1 retry,
  simple-chat `maxAttempts = 2`, `server.go:439`), plus the transparent auth refresh.
- Retry on another account excludes the failed account's identity
  (`AcquireWithWaitExcluding` pattern, `pool.go:505-507`).
- Every retry regenerates `msgId` (§2.4 dedup).

### 7.3 Stream interruption

- Upstream stalls (no SSE event for `SSE_IDLE_TIMEOUT_S=180`) → abort upstream, emit
  error frame, close. (`mimo-web-proxy` `chat.go:62,112-120`.)
- Client disconnects → abort upstream request immediately (`r.Context()` cancellation
  propagates to the upstream request context — §3; `mimo-web-api` uses AbortController
  the same way, `openai.js:305-326`), release the pool slot, and if the turn had
  produced a `dialogId`, keep the sticky entry (context advanced) but do not count it
  as success or failure.
- Mid-stream upstream `event:error` with partial content already delivered: append a
  final content delta `"\n\n[upstream error] <msg>"` and close with
  `finish_reason:"stop"` (mimo-web-api does this, `openai.js:406-414`) — clients get
  an honest terminated stream rather than a hang.

### 7.4 Error responses (OpenAI shape)

```jsonc
{"error": {"message": "...", "type": "...", "code": "..."}}
```

| Condition | HTTP | type | code |
|---|---|---|---|
| Bad API key | 401 | invalid_request_error | invalid_api_key |
| Malformed request / no user message | 400 | invalid_request_error | — |
| All accounts cooldown/parked | 429 | rate_limit_error | pool_parked (+ Retry-After = nearest recovery) |
| All accounts busy (queue wait exceeded) | 429 | rate_limit_error | pool_busy (+ Retry-After) |
| All accounts disabled / empty pool | 503 | api_error | no_accounts |
| Upstream 429 surfaced | 429 | rate_limit_error | upstream_rate_limited |
| Upstream 401 after refresh retry | 401 | invalid_request_error | upstream_auth_expired |
| Upstream 5xx / transport | 502 | api_error | upstream_failure |
| Content filter | 400 | invalid_request_error | content_filter |
| Query exceeds channel char cap (§2.7) | 400 | invalid_request_error | context_too_long |

Codes `pool_parked` / `pool_busy` / `no_accounts` borrowed from simple-chat's mapping
(`server.go:923-959`); `Retry-After` always reflects **local** recovery deadlines and
never leaks account identity (simple-chat guarantees the same,
`error_mapping_test.go:17-46`).

Streaming errors before first byte → normal HTTP error JSON. After first byte → a
final SSE chunk `{"choices":[{"delta":{},"finish_reason":"stop"}]}` preceded by a
chunk carrying `delta.content` with the error text, then `[DONE]`.

### 7.5 Account eviction

- `disabled` accounts are excluded from all selection and probes except explicit
  admin/manual re-enable (re-login with stored password, or `pass_token` replacement).
- 461/permanent stays disabled across restarts (persisted state, §4.7; simple-chat
  precedent: persisted bans survive, manual revive = editing the store,
  `pool.go:328`).
- Cooldowns persist across restart too, so a restart can't be used to bypass a limit
  window (simple-chat `pool.go:316-340` restores persisted park state).

---

## 8. Testing account

A single real Xiaomi account is designated for live verification. The
credentials are NOT written here — they live only in `data/accounts.json`
(git-ignored, 0600) as the first account entry, never in code, config
templates, docs, or git history. A repo grep must find no phone/password
literals. (An earlier draft of this SDD carried them inline; scrubbed
2026-10-08 — check git history stays clean before any commit.)
2. **Live verification protocol** (run before merging any protocol-touching change):
   1. Cold start → account performs passport login (§2.2) → STS cookies cached.
   2. `GET /v1/models` returns the catalog.
   3. Non-streaming completion: single-turn "reply with exactly: pong" — assert
      200, `choices[0].message.content` contains the echo, `usage.total_tokens > 0`.
   4. Streaming completion: same prompt with `stream:true` — assert ≥2 content
      deltas, terminal `finish_reason:"stop"`, usage chunk, `[DONE]`.
   5. Sticky streaming: two-turn conversation (second request extends the first) —
      assert second request hits the session store (log line), single upstream call
      (delta path), and the answer references first-turn context.
   6. Session refresh: clear the cached session, single request → transparent STS
      refresh + retry, 200 to client.
3. Live tests are **manual/CI-optional** (gated by env `MIMO2API_LIVE=1`; tests call
   `t.Skip` when unset), rate-limit aware (one pass per run, sleep between calls), and
   never parallel.
4. Unit/integration tests: Go native `go test` with `httptest.Server` standing in for
   the MiMo upstream (recorded SSE fixtures served as `text/event-stream` — the
   test style `mimo-web-proxy` and simple-chat both use in their Go suites). The real
   account is for protocol drift detection only.

---

## Appendix A — Reference cross-comparison

| Concern | mimo-web-api | mimo-web-proxy | xm2api | simple-chat | mimo2api decision |
|---|---|---|---|---|---|
| Upstream | aistudio `/open-apis` | aistudio `/open-apis` | mimo-server-cn `/api/route` | DeepSeek | aistudio (mission) |
| Auth storage | cookie or `{userId,passToken}` entries | `{service_token,user_id,ph}` + browser extension | `pass_token`/`service_token` files | phone/password → bearer | phone/password + optional pass_token, one JSON file |
| Session renewal | STS from passToken, 6h sweep, 401-driven | none (manual re-capture) | 401-driven + 6h timer | lazy relogin on auth fail | 401-driven primary + 6h sweep |
| Multi-turn | chain replay of user turns via `previousDialogueId` | fingerprint convstore + delta send | N/A (passthrough) | fresh session per request | sticky fingerprint + delta; chain replay on miss |
| Streaming | true incremental chunks | buffered, one fat chunk | byte passthrough | true incremental | true incremental |
| Thinking | `<think>\0` splitter → `reasoning_content` | stripped | passthrough | `reasoning_content` | splitter → `reasoning_content` |
| Account states | healthy / cooldown (exp backoff) | active / unhealthy flag | enabled / cooldown 5min | ready/muted/risk/cooling/banned + leases | available / cooldown / disabled |
| All-cooldown behavior | still picks one | n/a | still picks one | fails closed 429 | fails closed 429 |
| Retry | 1 auth-refresh retry | 429/5xx ×5 (2/4/8/16s) + empty-retry | 1 renewal retry (non-stream) | 2 attempts, pre-first-byte, exclude identity | 2 attempts + auth refresh + empty-retry, pre-first-byte |
| TTL on sessions | none | none (LRU-50 only) | n/a | none (janitor deletes upstream) | **30 min idle TTL + LRU-500** |
| Tools | prompt-injected passthrough | 5-format parser + prompt | n/a | stripped | stripped (out of scope) |

## Appendix B — Open questions (to resolve during implementation spike)

1. Does aistudio 429 ever carry a structured retry deadline (body JSON)? If yes,
   replace the fixed cooldown ladder with upstream-provided deadlines + 1h grace
   (simple-chat's `mute_until + 1h` rule, `client.go:1212-1217`).
2. Passport behavior of the unencrypted `serviceLoginAuth2` form post from a
   datacenter IP — captcha probability. If high, implement the EUI-encrypted variant
   (§2.2) before shipping login.
3. Fastchat (ultraspeed) SSE quirks: mimo-web-proxy notes multiple `usage` events on
   fastchat (`chat.go:156-159`) — confirm last-wins holds for all models.
4. Whether `POST /open-apis/chat/conversation/save` is needed for sticky
   conversations to survive upstream-side GC, or auto-save on first message suffices
   (`mimo-web-proxy` saves async, `web_client.go:260-300`). Test with a 24h-old
   conversation.

> **Status (2026-10-09, live runs recorded in `docs/live-findings.md`).**
>
> - **Q1 — open.** LIVE-007 fired 30 unpaced requests against one account and
>   never reached the limiter (~0.2 req/s: each turn takes ~5 s), so the fixed
>   cooldown ladder in §4.5 stands untested rather than confirmed. Answering it
>   needs concurrency, not a longer sequential burst. `mimo.UpstreamError` now
>   retains the response headers so a future run can read `Retry-After` /
>   `x-ratelimit-*` directly (never echoed to clients, TDD ERRMAP-011).
> - **Q2 — open, partial evidence.** Three live cold logins from this host all
>   reached the identity/OTP step with **zero captcha answers**; that is 0/3,
>   not the 5 samples LIVE-008 wants (it needs five operator-supplied codes,
>   because each cold login sends an SMS — see ADR 0004). No EUI-encrypted
>   variant is implemented.
> - **Q3 — open.** LIVE-009 captured a fastchat (ultraspeed) stream with a
>   **single** `usage` event (total 2418), so last-wins is neither confirmed nor
>   contradicted. Our splitter keeps last-wins (§2.5, SSE-012), which is the
>   safe rule if a later event is cumulative.
> - **Q4 — phase 1 done, phase 2 pending.** LIVE-010 saved a sticky-store
>   snapshot plus the turn transcript; phase 2 (`MIMO2API_LIVE_PHASE=2`) reruns
>   after ≥24 h. Until then the client relies on upstream auto-save and never
>   calls `POST /open-apis/chat/conversation/save`.
