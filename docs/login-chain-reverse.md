# MiMo login chain — verified reverse engineering

Date: 2026-10-09. Method: live HTTP walks against `account.xiaomimimo.com` /
`aistudio.xiaomimimo.com` from this host, plus static analysis of the full
mi-account and MiMo Studio bundles (all 474 mi-account chunks + all 21 Studio
assets downloaded and read), plus a real-browser load (Camoufox) of the page
the chain lands on. Every claim below is either an observed request/response or
a quoted source location. Anything inferred is marked as such.

**This supersedes `docs/usercross-reverse.md`** on the meaning of
`/fe/service/userCross`: that path is the **cross-region personal-data-transfer
consent page**, not the repeated-phone page. See §6.

---

## 1. The chain (observed, in order)

```
GET  https://aistudio.xiaomimimo.com/open-apis/user/mi/get
     → 401 {"code":401,"loginUrl":"https://account.xiaomi.com/pass/serviceLogin?callback=…&sid=xiaomichatbot&_group=DEF…"}

POST https://account.xiaomi.com/pass/serviceLoginAuth2        (form)
     _json=true, callback, sid, qs, _sign, serviceParam, user=<phone>,
     hash=UPPER(MD5(password)), policyName=miaccount, captCode="", _group
     → 200 &&&START&&&{"code":0,"securityStatus":<16|128>,"location":"","notificationUrl":"<…>"}
       Sets: deviceId, pass_ua, uLocale

-- then one of two shapes, decided by securityStatus / the chain target --

A) securityStatus=128, non-mainland egress IP — cross-region consent:
GET  <notificationUrl>                                     → 302
GET  /pass2/userinfo/start?…&bizType=UserCross&sign=2%26V1_passport%26…&token=…&userId=<uid>
                                                           → 302
GET  /fe/service/userCross?needClose=false&userId=<uid>    → 200 SPA page
POST /pass2/userCrossAgreement/update                      (form)
     userId=<uid>, status=1 (=同意), needClose=false
     → 200 &&&START&&&{"code":0,"location":"/pass2/userinfo/check?callback=<…authStart…>"}
GET  /pass2/userinfo/check?callback=…                      → 302
GET  /fe/service/identity/authStart?sid=xiaomichatbot&context=<1046B>
                                                           → 200 SPA page
       Sets: user_info_modification_session

B) plain identity demand — notificationUrl *is* the authStart URL (no hops)

GET  /identity/list?sid=xiaomichatbot&supportedMask=0&_locale=zh_CN&context=<…>
     → 200 &&&START&&&{"code":2,"flag":4,"option":4,"options":[4],"externalId":"<uid>",
                      "retrieveType":"bind","trustCheckBox":false,"applyToken":"…"}
GET  /identity/auth/verifyPhone?_flag=4&_json=true         (X-Requested-With)
     → 200 &&&START&&&{"maskedPhone":"+86 182******16","code":0,"contentType":160040}
POST /identity/auth/sendPhoneTicket?_dc=<ms>               (form retry=0, icode="", _json=true)
     → 200 &&&START&&&{"code":0,"data":{"wt":59},"desc":"成功"}      ← SMS dispatched
POST /identity/auth/verifyPhone?_dc=<ms>                   (form _flag=4, ticket=<code>,
     trust=true, _json=true)
     → 200 &&&START&&&{"code":0,"location":"/identity/result/check?…"}

-- landing chain (all hops walked by hand, cookies collected at each) --
GET  /identity/result/check                                → 302
GET  /pass/serviceLoginAuth2/end?userId=…&sid=…&callback=… → 302
GET  https://aistudio.xiaomimimo.com/sts?sign=…&followup=… → 307
GET  /open-apis/user/mi/get                                → 200 {"code":0,…}
       Sets at the upstream hop: userId, cUserId, xiaomichatbot_ph,
       xiaomichatbot_serviceToken, xiaomichatbot_slh
```

Evidence: `[2] auth2 code=0 sec=16 …`, `[3.0] 200 …/identity/authStart`,
`[6] identity/list → code=2 flag=4`, `[sendTicket] {"code":0,"data":{"wt":59}}`,
`[verify] code=0 location=set`, hops 302→302→307→307→200, then
`/open-apis/user/mi/get → 200 {"userId":"2652042633","idc":"China","bannedStatus":"NOT_BANNED"}`.

### 1.1 Properties worth pinning

- **Consent is one-shot per verification session.** A second
  `userCrossAgreement/update` POST on a consumed chain answers
  `{"code":21301,"desc":"认证失败"}`. Once agreed from an egress IP, later
  logins from it skip shape (A) entirely (`securityStatus=16`, hop chain is a
  single 200 on `/identity/authStart`).
- **The intermediate `Location` carries what the page URL does not.** The
  page URL is only `/fe/service/userCross?needClose=false&userId=…`; `sign`,
  `token` and the identity `context` appear exclusively on hop Locations
  (`/pass2/userinfo/start` and the `callback=` parameter of the consent
  response). Any client that auto-follows redirects loses them.
- `securityStatus` observed values: `128` (verification demand) and `16`.
  `0` on a completed `serviceLogin`.
- The identity step is **not** the repeated-phone flow: `/identity/list`
  answers `flag=4` (phone) with the account's own number.

## 2. Session cookie shape (observed)

The browser sends, for `aistudio.xiaomimimo.com` (Camoufox capture, real
session):

```
cUserId=<…>; userId=<uid>; xiaomichatbot_serviceToken="…"; xiaomichatbot_slh="…"; xiaomichatbot_ph="…"
```

Live subset test against `/open-apis/user/mi/get`:

| cookie set | result |
|---|---|
| browser set (above) | 200 |
| `userId` + `serviceToken` (unquoted) | 200 |
| `xiaomichatbot_ph` only | 401 |
| none | 401 |

⇒ `userId` + a service token is the minimum; the full browser set is what we
now compose (`sessionFromJar`), so the fingerprint matches reality.

## 3. Chat surface (observed + source)

Studio HTTP layer (`main.911869f9.chunk.js`, class with `getHeaders()`):

```js
getHeaders(){ return { "Accept-Language": locale, "x-timeZone": Intl.DateTimeFormat().resolvedOptions().timeZone } }
```

plus axios defaults `X-Requested-With: XMLHttpRequest` (main chunk) and the
browser's own headers (UA, accept, referer, sec-fetch-*, cookies).

Chat payload (same chunk, message-send path):

```js
n = { enableThinking, webSearchStatus, model };
if (temperature !== undefined) n.temperature = temperature;
if (topP !== undefined) n.topP = topP;
h = { msgId, conversationId, query, isEditedQuery, previousDialogueId,
      sceneType: payloadSceneType, params: payloadSceneParams,
      modelConfig: n, multiMedias };
r.OR.completions(h, controller, { viaFastchat: model === ultraspeedModelCode });
```

- `previousDialogueId` = the **last dialog id of the previous message**, not
  the previous message id.
- Endpoint `/open-apis/bot/chat`, or `/fastchat/open-apis/bot/chat` for the
  ultraspeed model (the app builds the prefix from a feature flag).
- Streaming parser: `TextDecoderStream` + a line splitter; thinking markers are
  `"<think>\0"` / `"</think>\0"`.
- Errors: `429` → rate-limited toast; `451`/`461` on `/open-apis/bot/chat`,
  `/open-apis/chat*`, `/open-apis/tts` → ban handling (461 permanent).
- The SPA also requires a one-time **product** agreement before the first chat
  in the UI (`isAgreed` / `isClawDisclaimerAgreed` in `/open-apis/user/mi/get`);
  the API itself answered 200 for us while `isAgreed:false`, so this is a UI
  gate only.

## 4. Our client vs. reality (2026-10-09 audit)

| Concern | Reality | Before | Now |
|---|---|---|---|
| auth2 form | phone + UPPER(MD5(pw)) + callback/sid/qs/_sign/serviceParam/policyName/captCode/_group, X-Requested-With | same | unchanged ✓ |
| verification chain | consent → userinfo/check → authStart → identity | `StartOTP` went straight at the notificationUrl, no hop walking, no consent | `consent.go`: hand-walked chain + consent POST + sid/context extraction ✓ |
| consent failure | 21301 认证失败 | n/a (step missing) | surfaced with code; no silent retry ✓ |
| consent policy | must agree to log in from a non-mainland IP | n/a | `MIMO2API_CROSS_REGION_AGREE` (default true; false → operator action) ✓ |
| identity UA | same browser UA end to end | `MiClaw/1.0` hardcoded in `ssoDo` | unified UA ✓ |
| masked notify | in the `verifyPhone?_json=true` answer, **not** in `/identity/list` | read only from `/identity/list` → empty `notify` | read from the trigger answer ✓ |
| landing chain | 4 hops; session cookies planted at the **upstream** hop | followed 1 hop, then two `/pass/serviceLogin` round-trips → `code=70016`, login failure | whole chain walked, session composed from the jar ✓ |
| session cookie | cUserId; userId; serviceToken spellings; slh; ph (quoted) | userId; serviceToken; serviceToken(quoted); ph | browser-fidelity set ✓ |
| chat payload | see §3 | same shape | unchanged ✓ |
| 429/451/461 | rate-limit / ban | handled | unchanged ✓ |

Regression tests: `internal/mimo/consent_test.go`
(UC-001..007 + OTP-CHAIN-001); mock passport now serves the live-shaped
consent chain and landing chain.

## 5. Still open

1. Whether the identity verification can ever be skipped (trusted device):
   `trust=true` is sent on submit, but the first login from a new
   `deviceId` always demanded a code. Untested: whether keeping the same
   `deviceId` cookie across logins removes the demand.
2. `461` handling per *account* vs per *model* — not exercised live.
3. `MIMO2API_CROSS_REGION_AGREE=false` has no operator flow (no admin
   endpoint to submit `status=1`); today it just disables the account.
4. The `sceneType`/`params` payload fields (`payloadSceneType`,
   `payloadSceneParams`) are only populated for scene-enabled models
   (e.g. translation); their server-side effect is unverified.

## 6. Correction to `docs/usercross-reverse.md`

- Chunk `UserCross.34c899eb.chunk.js` (webpack id 9976, route
  `/fe/service/userCross`) is the **cross-region consent page**: it reads only
  `userId`/`needClose` and POSTs `/pass2/userCrossAgreement/update`.
- Module 30390 in chunk `4793.02a8247d` (repeatedPhone form hook,
  `/pass2/repeatedPhone/sendTicket|verifyTicket`, `25005` confirm-bind) is a
  **different route family** (upgrade-user / repeated-phone). Its contracts in
  that document are real, but the route attribution — and the whole
  "UserCross = repeated phone + SMS" reading — was wrong.
- Chunk filenames come from the runtime's *name* map plus the JS *hash* map:
  `({id:"Name"}[id]||id) + "." + {id:"hash"}[id] + ".chunk.js"`. Fetching
  `9976.<hash>.chunk.js` 404s; the file is `UserCross.<hash>.chunk.js`. That
  mistake is why the previous pass analysed a module from a different chunk.
