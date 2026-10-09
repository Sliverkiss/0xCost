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

0. **Device-anchor priming.** Navigate to the login URL like a browser (one free
   GET, following the 302 from the legacy page to the account SPA) before posting
   the form, presenting the stored device identity and keeping whatever the page
   issues back: `deviceId=wb_<uuid>`, `pass_ua=web`, `uLocale`, `JSESSIONID`. This
   is the anchor the new-device trust attaches to; without it every login looks
   like new hardware (ADR 0013, `internal/mimo/passport.go`).
1. **Login context probe.** `GET {aistudio}/open-apis/user/mi/get` with
   `redirect: manual` → 401 body carries `loginUrl`
   (`https://account.xiaomi.com/pass/serviceLogin?callback=...&sid=xiaomichatbot`).
   Parse `callback, sid, qs, _sign, serviceParam, _group` from it
   (`xiaomi-login.js:123-152`).
2. **Password auth.** `POST https://account.xiaomi.com/pass/serviceLoginAuth2` with
   form fields `sid, callback, qs, _sign, serviceParam, user, hash, _json=true, ...`
   (`xiaomi-login.js:206-228`). The request carries Chrome's HTML-form header set
   (`Accept: text/html…`, client hints, `sec-fetch-mode: navigate`, the page's
   `Origin`/`Referer`, `Upgrade-Insecure-Requests`) — see `passport.go`; the
   `identity/*` hops use the SPA's XHR set instead. Both were three lines short of
   what a browser sends until ADR 0013.
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
   `xiaomi-login.js:242-246`), the derived cookie string + `ph`, and the
   **device identity** (`deviceId` / `pass_ua` / `uLocale` — see the amendment
   below and §2.5).

> **Amendment (2026-10-09, reverse + live experiment).** The passport plants a
> *device* identity alongside the session: `serviceLoginAuth2` answers with
> `Set-Cookie: deviceId, pass_ua, uLocale`, and the identity verification page
> carries the «remember this device» flag as the form field `trust` (initialised
> from the server's `trustCheckBoxSelected`; we already submit `trust=true`).
> Replaying those cookies does **not** by itself skip the new-device demand
> (three-attempt live experiment: cookieless / full replay / deviceId-only all
> answered `code=0` + `notificationUrl`), so the trust must first be established
> by one completed verification *bound to that device*. What the client owes the
> trust is continuity: without a stored device it presents `deviceId=""` on every
> login, the passport mints a fresh one, and the trust can never attach. Accounts
> therefore persist `device` and replay it on every login/renewal hop
> (`internal/mimo/device.go`, `api/session_apply.go`); the server's freshly issued
> value wins, exactly as a browser lets `Set-Cookie` overwrite what it sent.

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
2. `GET {loginUrl}&_json=true` with `Cookie: userId=<id>; passToken=<rt>` (the request
   carries the stored device identity and the browser's navigation headers, ADR 0013) →
   `&&&START&&&{code:0, location:"https://aistudio.xiaomimimo.com/sts?sign=..."}`.
   `code 70016` = passToken dead → **asynchronous password re-login** (ADR 0014), not
   eviction. Note the response echoes the passToken it was sent: a renewal refreshes the
   session and never the credential, whose 30-day clock only a full login resets
   (docs/live-findings.md DEVICE-004).
3. `GET location` → Set-Cookie plants fresh `serviceToken` / `ph` / `userId`.
4. Cache to disk (`data/session-cache.json` in the reference; we persist per-account in
   the accounts store, §4.7).

Keepalive cadence: every `MIMO2API_SESSION_REFRESH_HOURS` (**12 h**, ADR 0014). A
renewal failure is classified: a dead credential (70016, captcha, SMS rate limit)
schedules the asynchronous recovery of ADR 0014 — single flight per account, 5-minute
backoff after a failure, and the requesting call answers `account_action` with a retry
hint instead of blocking on a login chain.

Session validation: `GET /open-apis/user/mi/get` → `{"code":0,"data":{"userId":...}}`
(`PROTOCOL.md:12`; `mimo-web-proxy/internal/mimo/web_client.go:305-328` also checks
business `code==0` inside the 200 body). `GET /open-apis/user/info` is dead (410) —
`web_client.go:302-304`.

The renewal hop carries the stored device identity too (`Cookie: userId=…;
passToken=…; deviceId=…`), and a `passToken` the landing chain rotates in
replaces the stored one instead of being written over by it — the platform
re-uses the credential today, but dropping a fresh one would silently forfeit the
refresh (`internal/mimo/device.go`, TDD DEV-006).

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

#### 2.4.1 Session extensions (proposed, streaming only)

Client→gateway extensions that let a caller pin which conversation it is
continuing, so identity stops being a guess over message content
(`docs/specs/session-identity.md` REQ-1/REQ-16):

| Direction | Surface | Meaning |
|---|---|---|
| in | header `X-Mimo-Session-Id` | explicit session id (highest precedence) |
| in | body `session_id` | same extension, for clients that cannot set headers |
| out | header `X-Mimo-Session-Id` | the effective id, so the client can pin it next turn |
| out | header `X-Mimo-Session-Mode` | `chain` \| `replay` \| `fork` (debug/ops value) |

**Streaming only.** `stream:false` is a one-shot request: no identity is
resolved, the store is not read or written, no session header is returned, and a
supplied session id is ignored with a debug note (REQ-10; `NOSTICK-003` remains
the permanent contract). The OpenAI `user` field is deliberately not an identity
source — it is usually a per-request nonce.

Additive only: with no new fields and no new headers, the request behaves
exactly as today (REQ-17).

### 2.5 SSE response format (the **upstream's**, not ours)

> This is the format MiMo answers our chat POST with; the client parses it frame by
> frame and collects the turn. The gateway no longer **emits** SSE to its own
> clients (ADR 0017) — do not read this section as a client-facing contract.


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
| `error` | Upstream error, `content` = message | **Classified** (see §7.1): transient throttle → one retry after 800ms; model-name rejection → 400, never retried; anything else → fail turn |
| `sensitive_query` / `sensitive_title` | Content-moderation block | Fail turn with content-filter error |
| `web_search` | Search results: a JSON array (live: ≤16 entries, one frame ≈94 KB; `usedContent` is a **JSON boolean**, `metadata` an **object**) | Parse with per-field tolerance (ADR 0007) → citations/annotations. The page body (`text`) is not forwarded |
| `doc`, `tip_ratio`, `tip_truncate` | Search/parse progress | Ignore (nothing client-visible) |
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

mimo2api: the account's own catalog is the authority (M19, live-verified):

- `modelConfigListNg[]` carries `{model, name, pageType, redirectTo, intro}` —
  live-verified to have **no** `features` field. `pageType` ∈
  `chat | tts | tts-vd | tts-vc | asr` (live: 14/3/3/3/1 of 24 entries).
- Capability defaults live in `modelConfigList[]` (the chat rows):
  `webSearchDefaultStatus` (`auto`/`disabled`/absent), `thinkingDefaultOn`,
  `nToken`, `showWebSearch` (a UI switch — live-verified `false` everywhere, so
  it does **not** mean "search unsupported").
- `/v1/models` unions rows per id across accounts (capabilities at their
  maximum) and merges the static list as a floor that is marked
  `source:"static"`. `/v1/models/{id}` reads the same source, so an id the static
  table does not know still resolves.
- Capability semantics follow the Studio's own enum
  (`DISABLED=0 / ENABLED=1 / ENABLED_BY_DEFAULT=2`): `capabilities.web_search`
  is 0 for non-chat page types, 1 when searchable but off by default, 2 when
  `webSearchDefaultStatus` is `auto`/`enabled`. `capabilities.audio` collapses
  the TTS variants onto `tts` and maps `asr` to `asr`.
- Caching is per account (`MIMO2API_CATALOG_TTL_S`, default 10m) and a failed
  read degrades to the static floor instead of failing the endpoint.

### 2.7 Audio surface (TTS/ASR)

Live-verified protocol (`docs/web-surface-reverse.md` §3/§4):

- TTS: `POST /open-apis/tts/v2/generate` (needs a real conversation entity, which
  `POST /open-apis/chat/conversation/save` without an `id` creates) → poll
  `GET /open-apis/tts/generateStatus?taskId=` → fetch the returned audio URL.
  `taskId` is a JSON **number**.
- Formats: `wav` and `mp3` only, selected by `content.audio.format`.
  **The upstream refuses spoken text over 1024 characters** and so does the
  gateway: `/v1/audio/speech` answers 400 `input_too_long` before any upstream
  work. Splitting and rejoining longer text was tried and removed — a piece near
  the cap makes the upstream fail the task itself (ADR 0010).
- Synthesis cost grows faster than the text and is load-dependent (live: 12.8s
  for 128 characters, 17.8s for 384, >220s for 700), so the synthesis deadline
  scales with length.
- ASR: `POST /open-apis/resource/genUploadInfo` → `PUT` → `POST /open-apis/asr/recognize`
  → poll `/open-apis/asr/recognizeStatus`.

### 2.8 Rate limits & anti-bot considerations

- Upstream HTTP semantics: `302/401` session expiry (body carries `loginUrl`), `429`
  rate limit, `451/461` account restricted — 461 permanent (`PROTOCOL.md:132-139`).
- Per-account parallelism is limited upstream (simple-chat's `parallel_chat_limit`
  analogue; MiMo expresses it as 429). We cap per-account in-flight requests instead of
  discovering the limit by being banned.
- Query size caps (ADR 0015, measured 2026-10-09):
  - **ultraspeed / fastchat: 50,000 chars**, enforced client-side before any
    account is touched. This is a real, sharp limit — the Studio's own
    `ultraspeedQueryLimitLength: 5e4` (live SPA, docs/web-surface-reverse.md §11) —
    confirmed by the platform refusing a 60,000-char message with
    `query is too long`. The older reference-implementation figure (45,000;
    48,750 OK / 50,000 rejected, `mimo-web-proxy/internal/router/multimodal.go:130-137`)
    is superseded by the Studio constant.
  - **every other channel: 102,400 chars**, the measured ceiling of the query
    text (probed to the boundary: exactly 102,400 served, 102,415 refused; the same
    for ASCII and CJK, so it counts characters and does not move with content).
    The previous 90,000 was an unmeasured guess and rejected work the platform
    serves. Because the platform refuses anything above 102,400 as well, mirroring
    it locally saves a wasted account call without risking a false rejection.
    Verified end to end: 20 × 6,000 chars (120,000 total) returned the model's own
    account of all 20 parts (~112,000 chars read) in 258 s — the cost of splitting
    is latency (~13 s per part), not capacity. The ceiling counts the **query text
    the platform receives**: the planner
    splices system messages into the first user query (API-017), so a client
    system prompt — or the protocol prompt the tool layer injects (1,355 chars
    for one tool, 5,222 for thirty) — spends the same budget. The pre-flight
    check therefore validates the composed first query
    (`adapter.ComposedFirstQueryLen`) and the error names how much of it was
    preamble.
    Refusals that still come from upstream (its wording `too long`,
    `query is too long`, `长度不能超过`) are **translated** into
    `400 context_too_long` with an actionable message, whether they arrive as an
    HTTP error body or as a mid-stream `error` event, and never penalize the
    account.
  - `MIMO2API_MAX_QUERY_CHARS` overrides both caps (0 = per-channel defaults);
    every local rejection is logged with size, cap, model and role.
- Ultraspeed context: the Studio caps its *client-side* ultraspeed context at
  `ultraspeedContextRoundLimit: 3`, but the channel itself keeps more — a live
  6-round chained conversation on `/fastchat/open-apis/bot/chat` still recalled a
  round-1 fact, and a fresh-conversation full replay did too (reverse doc §11).
  The gateway therefore keeps server-side chaining (sticky) plus its own replay cap
  instead of inheriting the Studio's 3-round truncation.
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

### 4.3 Sticky-session store — **DELETED (ADR 0017)**

> Historical: the store was streaming-only (ADR 0009) and the deployment served
> non-streaming, so it was deleted rather than kept behind a switch. Kept here as
> the record of the design. Every request is a full replay now; see §5.3 and SDD §5.7.


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

#### 4.3.1 Session identity and isolation (target design)

Full rationale, requirements and acceptance plan:
`docs/specs/session-identity.md`. This subsection is the engineering summary;
the rules above (§4.2 fingerprint, §4.3 store) describe what is deployed today
and stay valid until the milestone lands.

**Why change it**: identity today is derived *only* from message content, so
two callers whose first five non-system messages match are indistinguishable and
"newest wins" can hand one caller another's dialogue frontier; the 200-rune
truncation plus 5-message window makes lossy matches possible; concurrent
requests for the same head silently fork; and non-streaming clients are excluded
from chaining entirely (`NOSTICK-003`), even though the upstream keeps
server-side context (`web-surface-reverse.md` §11.1).

**Identity ladder** (exactly one result per request, and streaming requests
only, REQ-1/REQ-10):

1. explicit — `X-Mimo-Session-Id` → body `session_id`;
2. derived — `sha256(credential_id ‖ model ‖ system ‖ tools ‖ first non-system
   message)`, opaque 32-hex, namespaced by the calling API key so identical
   prompts from two tenants cannot collide (REQ-3/REQ-4);
3. fresh — mint a new session id, returned in `X-Mimo-Session-Id`.

**Entry shape** (target, §4.3 shape extended):

```
session_key     # ladder result, scoped by credential
account_id      # affinity: pinned while usable
conversation_id # upstream conversation
prev_dialog_id  # chain frontier
frontier_hash   # canonical hash of the stored frontier (verification, not lookup)
msg_count       # replay guard (strictly increasing)
inflight        # fork policy input
created_at, updated_at
```

**Verification before chaining (INV-1)**: a stored session may be chained onto
only when `frontier_hash` matches a *prefix* of the current history and the
message count strictly increased. Verification covers the whole frontier rather
than a truncated window, so editing history near the start invalidates the
session (falling back to replay) instead of silently matching a lookalike.

**Fork policy (REQ-8/REQ-9, decided: wait-then-fork)**: while a session has an
in-flight request, a second streaming request for it waits up to
`MIMO2API_SESSION_WAIT_MS` (default 2000) for the chain to free; if the bound
expires it forks — a new session id and a fresh conversation
(`MIMO2API_SESSION_FORK_POLICY=fork` skips the wait). A fork is reported in
`X-Mimo-Session-Mode` and the debug log; the original chain is untouched.
Interactive streams may wait briefly for their own chain, never indefinitely.

**Non-streaming is one-shot (REQ-10, decided)**: `stream:false` never resolves
an identity and never touches the store, so `NOSTICK-003` stays the permanent
contract. A client that alternates `stream` true/false therefore gets a chained
turn followed by a fresh one — deliberate: a one-shot request must not advance
or fork a chain it does not own.

**Account migration (REQ-11)**: if the pinned account is unavailable, the
session moves by minting a **new** `conversationId` and replaying the full
history; the chain frontier is not carried across accounts and the migration is
logged.

**Observability (REQ-15)**: one debug line per decision —
`session=<id> source=<explicit|derived|fresh> decision=<chain|replay|fork|migrate>
frontier=<n> history=<n>` — with no message content and no credentials.

**Invariants** (INV-1..INV-6): no chain without verified ownership; session
state scoped to (credential, session id); no content stored; at most one live
chain writer per session; losing state degrades to a full replay, never to a
wrong answer or a cross-session leak; every decision reconstructible from its
log line.

**Unchanged by this design**: idle TTL and LRU semantics (§4.3), the replay path
and its client-visible behaviour, error mapping (§7), and the mid-stream commit
boundary rule (§7.3).

### 4.4 Concurrency & in-flight caps

Per-account semaphore, default `max_inflight = 2` (simple-chat
`DefaultMaxInflight = 2`, `pool.go:18` — same upstream parallelism concern). Acquire
waits up to `queue_wait` (default 15s; simple-chat uses 30s, `pool.go:162-163`, we
default shorter because our clients are interactive). Timeout → `429 pool_busy` with
`Retry-After` (error mapping from simple-chat `server.go:923-959`).

**A lease is capacity for one request, never stickiness (ADR 0019).** It names the
account the request is talking to (so every replayed turn keeps the same upstream
conversation) and occupies one in-flight slot; it is not shared between requests and
carries no conversation state. Each lease carries the TTL it was issued with
(`MIMO2API_LEASE_TTL_S`, default 120s): the request path applies it as a per-turn
context deadline, and the pool reaper — swept every 5s by the janitor — force-releases
slots past their TTL and cancels the upstream call they were holding
(`Lease.ArmExpiry`). `Release` is idempotent, and the slot is handed back **before**
the client is written to. `MIMO2API_SLOT_HOLD` decides when: `last-byte` (default) or
`first-byte`, where the slot goes back the moment the upstream starts answering.

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

### 4.5.1 Failure classes and per-class ladders (target design)

Spec: `docs/specs/account-pool.md` §4. Today one counter (`failStreak`) and one ladder serve
`429`, `5xx`, transport failures and stalls together — `pool.go:392` documents it as
"Transport errors are treated the same" — so a local network blip escalates an account's
cooldown exactly like an upstream 5xx, and afterwards nothing records which it was.

The target design gives each class its own counter, its own base and its own cap:

| Class | Signals | Own ladder (base → cap) | Client-facing |
|---|---|---|---|
| `transport` | connection error, timeout, SSE idle-stall, EOF mid-body | 15s → 5min | none (retry on another account) |
| `upstream-5xx` | 5xx, non-SSE 200 body | 30s → 10min (unchanged) | 503 `upstream_busy` after retries |
| `throttle-429` | 429 | 60s → 10min (unchanged) | 429 `upstream_rate_limited` |
| `service-busy` | `event:error` "服务器繁忙，请稍后再试" | none — no pool action | 503 `upstream_busy` + `Retry-After: 2` |
| `content` / `model-rejected` | other `event:error`, `sensitive_query`, "模型名称错误" | none — no pool action | 400/422 as today |
| `account-auth` | 401/302 pre-first-byte after renewal | none — disabled | 502 `upstream_auth` |
| `account-dead` | STS `70016`, dead passToken, OTP demand, captcha | none — disabled (permanent only for a dead passToken) | 502 as today |
| `risk-control` | 451/461 | none — disabled, **form unverified** (§4.6.1) | 502 `account_restricted` |

Rules:

- **No cross-class escalation**: a `transport` failure after three `upstream-5xx` failures
  starts the `transport` ladder at its own base (REQ-6).
- **Every cooldown records its class** and the upstream status that caused it, so §4.9 can
  report *why* an account is cooling (REQ-5).
- **`ReportSuccess` clears every counter**; a successful renewal clears counters but never a
  live cooldown, which is time-owned (REQ-6/REQ-7).
- **The upstream's own `Retry-After` is never read**, not even internally: ladders are the
  single source of cooldown timing (owner decision 2026-10-09, spec §10.2). It also stays
  absent from client responses (ERRMAP-011).

### 4.5.2 Risk-control signals: fail closed, recorded, never assumed

451/461 are **not classified** by us: the reference implementation calls 461 permanent
(`PROTOCOL.md:132-139`), our own docs record the form as unexercised
(`docs/login-chain-reverse.md:169`), and we have never observed one live. Until evidence
exists:

- the account stays disabled (fail closed) and is not retried inside the request;
- the event is recorded with account id, egress label, model, channel, upstream status and
  body digest, timestamp, and how many other accounts on the same egress hit a
  risk-control signal inside the configured window;
- **`permanent` is not set on an unclassified signal** — it is set only for a dead
  passToken, where we do have account-scoped evidence (REQ-10, INV-5);
- the classification is obtained **passively**: whatever 451/461 events occur naturally are
  correlated across accounts/egresses/models. Nothing in the gateway may deliberately
  induce a risk-control response to study it (REQ-9).

### 4.6 Health transitions summary

| Upstream signal | Pool action |
|---|---|
| 200 + SSE completes | success — reset failure counter |
| 429 | cooldown ladder (60s → 10min) |
| 401 / 302 (pre-first-byte) | STS refresh + retry once; second failure → disabled |
| STS `70016` / passToken rejected | disabled (needs re-login; admin surface or manual file edit) |
| 451 | disabled with reason (account restricted) |
| 461 | disabled (fail closed); event recorded for classification; **no permanent flag** until the form is known (§4.5.2) |
| 5xx (non-SSE 200 body) | cooldown ladder 30s → 10min |
| transport / connection error / stream stall > 180s (`chat.go:62,112-120`) | cooldown ladder 15s → 5min, its **own** counter (§4.5.1) |
| `event:error` "服务器繁忙，请稍后再试" (transient upstream throttle) | one retry after an 800ms pause; account stays available (platform load, not an account fault); still busy → 503 `upstream_busy` + `Retry-After: 2` |
| `event:error` "模型名称错误" (channel refuses the model id) | 400 `model_not_available_on_channel`, no retry, log a warning (our routing/catalog disagree with upstream) |
| `event:error` (other) / `sensitive_query` mid-stream | fail the request; no pool action (content-level, not account-level) |
| Empty response, no error | one retry with fresh `msgId` + fresh `conversationId` (`mimo-web-proxy/internal/handler/chat.go:283-324` empty-response rotation) |

### 4.7 Account storage: file or Upstash (ADR 0018)

Two interchangeable backends behind one interface (`internal/accounts/backend.go`).
The runtime always works on the in-memory set: the backend is read **once** at
startup and written through on every mutation, so a request path never touches
storage.

| backend | chosen by | format |
|---|---|---|
| `FileBackend` (default) | nothing set | `MIMO2API_ACCOUNTS_FILE` (default `data/accounts.json`), mode 0600, atomic temp+fsync+rename, directory created on demand |
| `RedisBackend` (Upstash) | `DS_REDIS_HOST` (+ `DS_REDIS_TOKEN`) | one JSON key per account, `mimo2api:accounts:<id>`, plus an index set `mimo2api:accounts:index` |

Why a second backend: the hosted deployment (Belmo, ADR 0018) has no durable file
system, and a container restart must not cost a re-login of the whole pool. A
configured-but-unreachable Redis is **fatal at startup** rather than a silent
fallback to an empty file — the operator named a source of truth.

The Redis client is a ~200-line RESP2 implementation in
`internal/accounts/redis.go` (stdlib only: no go-redis dependency). It refuses a
plaintext `redis://` URL to a non-loopback host, because account passwords and
session cookies live in that store (`DS_REDIS_INSECURE=1` accepts it for a local
server). Seeding a hosted pool from a local file:
`mimo2api -accounts import data/accounts.json`.

#### Managing the store (ADR 0020)

A pluggable store removed the file people used to edit, so the same binary grew a
management surface: `mimo2api -accounts <command>`, resolved against whichever
backend the environment selects (it calls the same `openStore` the server does,
and prints the backend it acted on).

| command | effect |
|---|---|
| `list` | one line per account: id, enabled, egress label, credential presence, session age, device anchor present |
| `show <id>` | the full record with secrets replaced by presence and length |
| `enable <id>` / `disable <id> [reason]` | flip the flag, recording why |
| `set-egress <id> <label>` | pin or clear the network-path label (§4.8) |
| `rm <id>` | delete the account from the store |
| `import <file>` / `export <file>` | upsert a local file into the store / dump the store to a 0600 file |
| `verify` | connect, count, report — non-zero when the store is empty |

Store API: `Put` (upsert) and `Remove` (delete) alongside the read/`Update`/
`ReplaceAll` pair; `Backend.DeleteAccount` on both backends. Redis deletes the
index member before the record key, so an interrupted delete leaves an invisible
key instead of an index entry that `Load` would trip over. Membership and flag
changes take effect on the next start — the store is read once at boot — while
runtime state (renewal, rotation, cooldown) writes through immediately.

#### File format

`data/accounts.json` (mode 0600, like simple-chat `json.go:54-56`):

```jsonc
{
  "accounts": [
    {
      "id": "<phone>",           // stable pool identity
      "phone": "<phone>",
      "password": "<redacted>",  // passport login; see §8
      "enabled": true,
      "label": "test",
      "egress": "hk1",            // optional egress LABEL only; endpoint+credentials
                                  // come from MIMO2API_EGRESS_HK1 in the environment (§4.8)
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


### 4.8 Per-account egress (target design)

Spec `docs/specs/account-pool.md` §7. An account may carry an **egress label**; accounts
without one share the default egress `direct`.

- **The file holds the label, the environment holds the secret**: `egress: "hk1"` in
  `accounts.json`, and the endpoint (with any credentials) in `MIMO2API_EGRESS_HK1`. The
  label is validated at startup against `[a-z0-9-]{1,16}`; a label with no matching
  environment variable **fails startup**, listing every missing name (REQ-18). Silently
  falling back to `direct` would send an account out of the egress the operator believes it
  is behind.
- **Applied to every upstream call for that account**: chat (normal and fastchat), TTS,
  ASR, `bot/config`, renewal and the recovery probe. A partially applied egress would
  produce exactly the mixed signals §4.5.2 has to diagnose (REQ-19).
- **Isolation, not rotation**: no proxy pools, no health-scored egresses, no automatic
  switch on failure (REQ-20). An account whose egress is braked waits, or the operator
  intervenes.
- Implementation note: today all accounts share one `http.Client` (`client.go:79`) over
  Go's standard transport. Egress is applied per account through an injected dialer/proxy
  function keyed on the request's account, not by varying the global fingerprint (which
  stays a constant by design, SDD §2.4).

### 4.9 Pool observability: `GET /v1/admin/pool`

Spec §6, owner decision 2026-10-09. A **separate authenticated** endpoint, authenticated
like `/v1/admin/otp/submit`. `/healthz` keeps its four aggregate numbers frozen — the
Docker healthcheck subcommand parses that body.

Per account: masked id, label, enabled, state (`available | cooldown | disabled`),
in-flight vs capacity, EWMA first-byte latency, cooldown end **and its class + reason**,
disabled reason and whether it is permanent, per-class failure counters (§4.5.1), last
error code + timestamp, egress label, credential age.

Suppressed by design: no cookie, no `ph`, no password, no proxy endpoint or credentials, no
upstream body beyond a status/error digest (REQ-14). Risk-control events (§4.5.2) appear in
the same view with their correlation counts, because that view is the instrument the
passive classification depends on (REQ-15).

## 5. API surface

### 5.1 Endpoints

| Route | Behavior |
|---|---|
| `POST /v1/chat/completions` | Chat, always one JSON body: there is no streaming path (ADR 0017). `stream:true` → `400 stream_unsupported`, unconditionally, before any account work. |
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
| `messages` | Turn planning, §5.3. `content` as string or `[{type:"text",...}]` parts (text parts joined, `mimo-web-api/src/openai.js:36-45`). **`image_url` parts are accepted (IMG-*)**: a `data:image/...` URL or an http(s) URL, fetched with the browser identity, uploaded through `genUploadInfo → PUT → resource/parse` and referenced from that turn's `multiMedias` — so an image never spends the 102,400-character query budget. Unknown part types stay ignored, not rejected. |
| `stream` | Streaming vs collected delivery. |
| `temperature`, `top_p` | Forwarded inside `modelConfig` (mimo-web-proxy sends `temperature:0.8, top_p:0.95` defaults, `web_client.go:127-140`); pass through when provided, else omit. |
| `max_tokens` / `max_completion_tokens` | Not supported upstream; ignored (warning header). |
| `tools`, `tool_choice`, `functions`, `response_format`, `n`, `seed`, ... | Stripped, not rejected. `X-Mimo-Warnings` response header lists dropped fields. |
| `reasoning_effort` | **Standard thinking knob.** `none` → `enableThinking:false`; `minimal`/`low`/`medium`/`high` → `true` (the upstream switch is boolean, so the level itself has nothing to map onto). Anything else → 400 `unsupported_reasoning_effort`, before any account work. |
| `web_search_options` | **Standard search knob.** Presence → `modelConfig.webSearchStatus:"enabled"`. `search_context_size` (`low`/`medium`/`high`) is validated and then reported in `X-Mimo-Warnings`, because the upstream switch has no size axis; an unknown size → 400 `unsupported_search_context_size`. |

Thinking and search both lead with the **standard** OpenAI field (see the
table). The two thinking spellings this API carried first — `enable_thinking: true`
and `reasoning: {"type":"enabled"}` (a la simple-chat `openai.go:355-417`) — still
work when `reasoning_effort` is absent; whenever it is present it decides,
including `none` as the explicit off switch. Either opt-in →
`modelConfig.enableThinking: true` → reasoning surfaced as `reasoning_content`
(marker splitting per §2.5). Default: off.

Search: `web_search_options` (presence = search this turn) is the standard
spelling; `web_search_status` remains for the finer `disabled`/`auto`/`enabled`
control and wins when both are given, and the `:online` model suffix stays as the
shorthand. **The model id is never renamed to carry an option.**

Images (`image_url` parts, IMG-*): the bytes travel the attachment path, not the
query — upload (`resource/genUploadInfo` → signed PUT) → parse
(`resource/parse?fileUrl&objectName&model`, retried: the platform answers a
transient business-code failure under load) → one `multiMedias` entry per image on
**the turn whose message carried it** (`mediaType: "image"`, plus the parse result's
`id` and `tokenUsage`). The media item carries the full field set, because the probe
showed a stripped item is refused. **`image_url.detail` is validated
(`auto`/`low`/`high`) and then reported in `X-Mimo-Warnings` under its own clause,
`accepted but not honoured`** — separate from `ignored unsupported fields`, which is
for fields we do not know at all (a caller must not read one line for both). The
channel's parse call takes only `fileUrl`/`objectName`/`model` (the verified shape, and the sibling
client sends nothing else), so the level has nowhere to go — the one thing it must
never do is look applied. Limits are client-facing guards, documented as
guards: `MIMO2API_MAX_IMAGES` (default 10) and `MIMO2API_MAX_IMAGE_BYTES` (default
20 MiB — OpenAI's own vision-input limit). The channel itself advertises 100 MB per
file and 50 files per message. Failures are named 400s — `unsupported_image_url`,
`image_fetch_failed`, `unsupported_image_type`, `image_too_large`,
`too_many_images` — and none of them cools an account down (the client's URL is not
the account's fault). A user message that carries only images is a legal request:
the channel needs a `query`, so `adapter.DefaultImageOnlyQuery` is sent and the
synthesis is documented here rather than hidden.

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
EXPOSE 3000
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
      - "127.0.0.1:21223:21223"
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
`mimo2api healthcheck` subcommand that does `GET http://127.0.0.1:21223/healthz`
and exits 0/1 — a common static-binary pattern.

Config surface — env vars, prefixed `MIMO2API_` to stay collision-free in compose
files (defaults):

```
MIMO2API_HOST=0.0.0.0                # empty API_KEYS forces 127.0.0.1 bind (§5.1)
MIMO2API_PORT=3000                   # else the platform's PORT (Belmo), else 3000 (ADR 0018)
MIMO2API_API_KEYS=                   # comma-separated; empty = open (bind-local)
MIMO2API_UPSTREAM_BASE=https://aistudio.xiaomimimo.com
MIMO2API_ACCOUNTS_FILE=data/accounts.json  # relative default: no /data mount needed (ADR 0018)
DS_REDIS_HOST=                       # Upstash: when set, Redis is the account store (§4.7)
DS_REDIS_TOKEN=                      # its token (required for a bare host / https URL)
DS_REDIS_CA_FILE=                    # extra root CAs for rediss:// (private Redis; never disables verification)
MIMO2API_DEFAULT_MODEL=mimo-v2.6-pro
MIMO2API_REQUEST_TIMEOUT_MS=120000   # client-facing budget for one chat request (ADR 0019; wired)
MIMO2API_SSE_IDLE_TIMEOUT_S=180      # no-event watchdog (mimo-web-proxy chat.go:62)
MIMO2API_SESSION_REFRESH_HOURS=12    # credential keepalive sweep (ADR 0014; NOT the deleted session store)
MIMO2API_MAX_INFLIGHT_PER_ACCOUNT=2
MIMO2API_QUEUE_WAIT_S=15
MIMO2API_X_TIMEZONE=Asia/Shanghai
MIMO2API_USER_AGENT=<Chrome UA, matched to sec-ch-ua>
MIMO2API_AUDIO_POLL_MS=1000          # TTS/ASR status poll cadence (§2.7)
MIMO2API_AUDIO_TTS_TIMEOUT_S=60      # base per-piece deadline; +500ms per character, capped below
MIMO2API_AUDIO_TTS_MAX_TIMEOUT_S=900 # cap on one piece's deadline (ADR 0008)
MIMO2API_AUDIO_ASR_TIMEOUT_S=120
MIMO2API_SPEECH_MAX_CHARS=1024       # clamps to the upstream's per-synthesis cap (ADR 0010)
MIMO2API_TTS_VOICE=                  # empty = the account catalog's first voice
MIMO2API_CATALOG_TTL_S=600           # per-account bot/config cache for /v1/models (§2.6)
MIMO2API_LOG_LEVEL=info              # info|debug; debug adds the routing trace and upstream event names
MIMO2API_COOLDOWN_TRANSPORT_S=15     # per-class ladders (§4.5.1); 5xx/429 keep 30s/60s
MIMO2API_COOLDOWN_CAP_S=600
MIMO2API_POOL_STATS_WINDOW_S=300     # window for correlating risk-control signals (§4.5.2)
MIMO2API_EGRESS_HK1=<endpoint>       # per-account egress (§4.8): the label lives in
                                     # accounts.json, the endpoint and its credentials here
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
| Upstream sick | 5xx, non-SSE 200 body (`upstream.js:208-223`) | 1 retry, another account, pre-first-byte only | `upstream-5xx` ladder 30s → 10min |
| Transport | connection error, timeout, stall > 180s, EOF mid-body | 1 retry, another account, pre-first-byte only | `transport` ladder 15s → 5min, own counter (§4.5.1) |
| Account dead | STS 70016, dead passToken, login captcha/OTP demand | no | disabled (permanent only for a dead passToken) |
| Risk control (form unverified) | 451/461 | no | disabled, event recorded for classification; no permanent flag until the form is known (§4.5.2) |
| Upstream busy (transient) | `event:error` "服务器繁忙，请稍后再试" | 1 retry after 800ms, pre-first-byte only | none — the account is healthy |
| Speech too long | `input` over 1024 characters | no | none — 400 `input_too_long` before any upstream work (ADR 0010) |
| Model rejected | `event:error` "模型名称错误" | no | none — 400 to the client, warn |
| Content-level | other `event:error`, `sensitive_query` | no | none — fail request |
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

### 7.3 Stream interruption — **RETIRED (ADR 0017)**

> Superseded: there is no stream to interrupt between the client and the gateway,
> and the answer is written in one piece. What survives is the client-disconnect and
> upstream-stall classification below (the middle bullet's rule, and the `SSE_IDLE_TIMEOUT_S`
> upstream read timeout) — pinned by DISCONNECT-001. The error-delta rule is gone:
> an upstream failure before the body is written is now an HTTP error (TDD API-021).


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
| Query exceeds channel char cap (§2.8) | 400 | invalid_request_error | context_too_long |
| Gateway deadline: request budget or per-turn lease TTL (§7.6) | 504 | api_error | chat_timeout |

Codes `pool_parked` / `pool_busy` / `no_accounts` borrowed from simple-chat's mapping
(`server.go:923-959`); `Retry-After` always reflects **local** recovery deadlines and
never leaks account identity (simple-chat guarantees the same,
`error_mapping_test.go:17-46`).

**Retired with the streaming path (ADR 0017):** the mid-stream error framing that used
to follow a first byte ("a chunk carrying `delta.content` with the error text, then a
final `finish_reason` chunk and `[DONE]`") no longer exists — there is no first byte to
have committed, so **every** error is a plain HTTP error JSON, pre-response.

### 7.6 Timeouts, budgets and the slot policy (ADR 0019)

Three clocks, all of them ours:

| Clock | Knob | Default | Effect when it expires |
|---|---|---|---|
| Request budget | `MIMO2API_REQUEST_TIMEOUT_MS` | 120s | `504 chat_timeout` naming the budget; no retry, no cooldown, slot returned |
| Turn / slot TTL | `MIMO2API_LEASE_TTL_S` | 120s | the upstream call is cancelled, same 504, slot returned; the reaper reclaims the slot if the handler itself is wedged |
| SSE idle watchdog | `MIMO2API_SSE_IDLE_TIMEOUT_S` | 180s | a real upstream stream problem → transport failure, the account cools |

The upstream HTTP client timeout is **derived** (`lease TTL + 30s`), so a single upstream
turn can only legitimately outlive its own TTL by the connection margin. Anything the
gateway cuts with our own clock is reported as `chat_timeout` and leaves the account
untouched: our deadline is not evidence about the account, and cooling on it would shrink
the pool every time a client sends an oversized replay.

The default budget sits below the ~100s origin read timeout a CDN imposes on a hosted
deployment, so the client gets our error with numbers instead of an intermediary's 524.
Long multi-message replays (~258s measured for 20 × 6,000 chars) therefore exceed it by
design: the honest 504 replaces an unbounded wait, and the supported fixes are fewer
messages per request, raising the knob, or reusing the upstream `dialogId` across requests.

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
| Tools | prompt-injected passthrough | 5-format parser + prompt | n/a | stripped | stripped and reported as dropped (§5.6, ADR 0016) |

## §5.6 Function calling: not offered (ADR 0016, superseding ADR 0012)

MiMo Studio implements no function calling (§2.1: the chat body has no `tools`
field, and `bot/config` advertises no such capability). Emulating it took a
middleware in front of the gateway that injected an XYML prompt protocol and
parsed the call back out of the model's text — and that prompt is charged against
the platform's per-query ceiling (§2.8, ADR 0015): 1,355 characters for one tool,
1,760 for three, 5,222 for thirty. Measured live: 102,000 characters with thirty
tools was **served** on the raw chat path and **refused** through the middleware.

Decision: the middleware is not deployed. The measurement is the whole argument —
on a channel whose usable query text is 102,400 characters, spending up to 5% of
it on a protocol prompt buys fewer tools than the context it costs.

```
client :21223 --(Bearer KEY)--> mimo2api:8080     (one container, one published port)
```

* **`tools` on the wire is accepted, ignored and reported**: the request is served
  as plain chat and `X-Mimo-Warnings` carries `tools ignored: this deployment does
  not implement function calling`. Never a silent downgrade — a client that wants
  tools runs them itself and sends the results back as ordinary messages.
* **One key, one address**: `MIMO2API_API_KEYS` is the only credential and the
  published port reaches the gateway directly, so `/v1/models`, `/v1/audio/*` and
  `/v1/admin/*` need no relay, no `catch_all_proxy`, and no second credential.
* **The vendored tree stays** (`deploy/toolforge/`, PROVENANCE.md) but nothing
  builds or runs it; reviving it is a compose service plus an env var.
* **Historical evidence** (live, before removal): `get_weather(city)` returned
  `finish_reason: tool_calls` with `{"city":"上海"}` and the result fed back
  produced the final answer (docs/live-findings.md, LIVE-TOOLS-001).

## §5.7 Chat transport: no streaming at all (ADR 0017, superseding ADR 0016 §2)

A request carrying `stream: true` is refused with `400 stream_unsupported` before
any pool or account work, with the fix in the message. The refusal is now
**structural**: `MIMO2API_ALLOW_STREAM`, the SSE emission path, the keepalive
ticker, the pre-first-byte commit boundary and the whole conversation store
(`internal/session/`, the `X-Mimo-Session-Id` contract, the fork policy and their
environment variables) are deleted, not disabled. Every request is one collected
answer delivered as a single JSON body, and every request is a **full replay**
(`adapter.PlanFullReplay`) into a fresh upstream conversation — the behaviour that
was already the non-streaming path's contract under ADR 0009.

Two things did **not** change and must not be read as collateral: the upstream is
still SSE (§2.5 describes the format the client parses), and web search, TTS and
ASR are untouched — citations ride in the JSON body and the audio surface never
consulted the switch.

What this buys and what it costs is in ADR 0017. The retired test material and its
replacements are listed in `docs/TDD.md` §2.1.3.

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
