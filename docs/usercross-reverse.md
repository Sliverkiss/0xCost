# UserCross Reverse-Engineering Findings (mimo2api login blocker)

> **CORRECTION (2026-10-09): the central premise of this document is wrong.**
> `/fe/service/userCross` (chunk `UserCross.<hash>.chunk.js`, webpack id 9976)
> is the **cross-region personal-data-transfer consent page**, not the
> repeated-phone page: it reads only `userId`/`needClose` and POSTs
> `/pass2/userCrossAgreement/update`. The `repeatedPhone` contracts recorded
> below (§sendTicket/verifyTicket) are genuine source, but they belong to
> module 30390 of a different chunk (upgrade-user / repeated-phone route).
> The filenames had been fetched as `<id>.<hash>.chunk.js`, which 404s — the
> runtime builds them as `<name>.<hash>.chunk.js`. Verified chain and the
> corrected attribution: **`docs/login-chain-reverse.md`**.

Date: 2026-10-08
Source: pure static analysis of the mi-account SPA chunks at `/tmp/michunks`
(46 real chunks of 955; 908 downloads failed — the fetcher doubled the
`static/` prefix and hit an S3 "Object Not Found" page, region `awsusor1`).
Analysis inputs: `/tmp/runtime.js`, `/tmp/main.js`, `/tmp/hop2.html`,
`/tmp/usercross.html`, `/root/mimo2api/references` (4 repos).

---

## 0. Executive summary

The account triggers **UserCross** (repeated-phone / cross-account risk
control), a flow that is **structurally different** from the new-device OTP
flow we already implemented in `internal/mimo/otp.go`. The two must not be
conflated:

| | New-device OTP (implemented) | UserCross (this flow) |
|---|---|---|
| Trigger | `securityStatus=128` + notificationUrl → `/identity/*` | notificationUrl → `bizType=UserCross` → `/fe/service/userCross` |
| Identity APIs | `/identity/list` works (flag 4/8) | `/identity/list` returns **10001** (no identity session) |
| Verify endpoints | `/identity/auth/verifyPhone` etc. | `/pass2/repeatedPhone/sendTicket` / `verifyTicket` |
| Params needed | `context` from notificationUrl | `userId, auth, callback, sign` from the hop-2 URL |
| Completion | verify → location → serviceLogin → STS | verifyTicket → response `.location` = **callback URL with auth** → STS |

The Go client can complete UserCross **without a browser** if it:
1. follows the notificationUrl chain manually (cookies into jar),
2. captures `auth/callback/sign` from the hop-2 **redirect Location header**
   (they ride in the `Location` of `GET /pass2/redirect?...`'s successor,
   not necessarily in the final page URL),
3. replays `POST /pass2/repeatedPhone/sendTicket` then `verifyTicket` with
   form encoding + `X-Requested-With: XMLHttpRequest`,
4. on success follows `response.location` (the callback with `auth=` — the
   serviceLoginAuth2 callback into aistudio) and runs the existing
   `stsExchange`.

The `sign` param format is server-generated (`2&V1_passport&<sig>`); the SPA
never generates it — it only passes it through. `token` (SbNqreE9...) is a
one-time hop token from the same redirect chain. The first `sendTicket`
**without captcha (icode="") may succeed**; captcha (87001/20031) is a retry
loop driven by risk scoring, not a hard precondition.

---

## 1. Which chunk renders /fe/service/userCross

- Runtime chunk-name map (extracted from `/tmp/runtime.js`):
  `9976 → "UserCross"` → file `9976.34c899eb.chunk.js`.
- **That chunk download failed** (183-byte S3 error file; fetcher requested
  `static/static/js/9976...`). What we know without it:
  - Module `30390` (in chunk **4793.02a8247d**, successfully downloaded) is
    the UserCross page hook — it is the page-level logic (form steps, API
    sequencing, param reads, success redirect). Chunk 9976 is almost
    certainly the lazy route wrapper that imports module 30390 (plus i18n
    strings and the React component tree).
  - The shell at `/tmp/usercross.html` is byte-identical to `/tmp/hop2.html`
    — **it is a static CDN-served HTML with no server-injected pageData**.
    Everything the page needs comes from the URL query, cookies, or runtime
    API calls.
- Routing (from `/tmp/main.js` module 27028): the pathname regex
  `/(\/fe)?\/service\/(identity|recyclePhone|verifyCode|lpLogin|childAccount|sns)|(authExtra\/recyclePhone)|userCross|fido|…/g`
  matches `userCross` — **any path containing "userCross" forces mobile
  bundle mode** (`h.tq` selects mobile entry chunk 5008 vs desktop 3068).
  Both entry chunks failed to download; the route table itself lives there,
  but the page behavior is fully derivable from module 30390.
- Router stack: react-router with `TH()` = `useLocation`-equivalent (module
  64880, not on disk), `k6()` = navigate, `Yq()` = pageData context store
  (module 99026, not on disk). SPA routes preserve the search string when
  navigating internally. The pageData context is populated per-page by the
  route wrappers (e.g. `/identity/*` pages fetch their pageData); the
  UserCross hook reads **only** URL params — no pageData fetch, no
  userinfo/start call.

## 2. Page-load behavior of the UserCross page (module 30390)

```js
// 4793.02a8247d.chunk.js, module 30390 "IC" (page hook)
var S = (0,l.Z)();            // location (configValue.searchParams)
P = S.searchParams.get("userId") ?? ""
T = S.searchParams.get("auth") ?? ""
F = S.searchParams.get("callback") ?? ""
I = S.searchParams.get("sign") ?? ""
A = S.searchParams.get("countryListOnlyCN") ?? ""
```

- **No load-time API call.** The page does NOT call `/pass2/userinfo/start`;
  it does not fetch pageData from the server. It renders from URL params
  directly. The `userinfo/start` hop is purely a **server-side 302
  redirect step** in the notificationUrl chain (it carries the `bizType`,
  `sign`, and `token` params and issues the next Location).
- The page is a 4-step wizard driven by a local `step` state:
  - step 1: phone input (callingCode + phone; `countryListOnlyCN=true`
    locks the list to +86 and prefills callingCode "+86");
  - step 2: SMS code input (ticket) — 60s countdown state exists;
  - step 3: success view — displays bind result, uses
    `data.bindType === "SAFE_PHONE"` to choose the view;
  - step 4: confirm-bind variant (reached on error code **25005** which
    returns `values.bindInfo` — phone already bound elsewhere; user must
    confirm takeover. `recyclePhoneAllowed` flips to `true`).
- On load the only network touch is the SMS quota check:
  `POST /pass/sms/quota` (module 73329 export `K9`) with
  `{address: <fullPhone>, contentType: "150007", userId}` — used to decide
  whether to allow resend / show the "couldn't get SMS" helper. Not on the
  critical path.
- On success (`verifyTicket` code=0): the hook reads `u.location` and
  `u.data.bindType`, sets step 3, stores `location` in state `D`, and the
  success view's continue button does `window.location.href =
  sanitized(D)` (module 60374 `a` strips `javascript:` — URL sanitizer, not
  a signer). The `confirm` URL param is used by the separate `S()` hook's
  `bindPhoneAddress` action (validates the target hostname against
  `account.xiaomi.com` before redirecting).

## 3. Where auth/callback/sign come from

All four params are read **from the page URL query**. But our captured final
URL was `/fe/service/userCross?needClose=false&userId=1000000001` — missing
`auth/callback/sign`. Provenance chain:

- `GET /pass2/redirect?sid=passport&callback=<long-sign>` → **302** whose
  Location is
  `/pass2/userinfo/start?_locale=zh_CN&bizType=UserCross&callback=&data={"n":false}&sid=passport&sign=2%26V1_passport%26<sig>&token=SbNqreE9...`
- `GET` that userinfo/start URL → **302** whose Location is
  `/fe/service/userCross?...`. **The missing params (`auth`, `callback`,
  `sign`) are on THIS redirect Location** — our capture only recorded the
  final page URL after the browser had already landed (a browser shows the
  final URL; the intermediate Location is what actually carried the
  params). The Go client must read the `Location` header of each hop (never
  auto-follow), capture the query of the last redirect, and merge it with
  the final page URL query (`needClose=false&userId=...`).
- Alternative source if a hop is missed: the `token=SbNqreE9...` query param
  on userinfo/start is a one-time exchange token; there is **no** SPA-side
  code exchanging it (no `userinfo` API call exists in any downloaded
  chunk — confirmed by grep). So the redirect chain is the only path.
- `callback` on the UserCross page is the **serviceLoginAuth2 callback**
  (the aistudio login callback URL), NOT the notificationUrl's own
  `callback` param (which is empty in the hop-2 URL). After verifyTicket
  succeeds, `response.location` is built server-side as the callback URL
  with `auth=<authToken>` appended — this is the STS handoff back into
  aistudio. The SPA merely `window.location.href`'s to it.
- `sign` (`2&V1_passport&<sig>`) and `auth` are opaque server-issued tokens;
  the SPA treats them as pass-through. Never client-generated.

## 4. Full request/response contracts

### 4.1 Response dialects

All passport endpoints answer with `&&&START&&&` + JSON (XSSI prefix) or
`@json:` + JSON; module 86704/73329 `y`/`m7` unwrap both. Go:
`parseXSSI` / `parseLooseJSON` already handle this.

### 4.2 GET /pass2/redirect — hop 1

```
GET https://account.xiaomi.com/pass2/redirect?sid=passport&callback=<long-sign>
Cookie: (passport session cookies from serviceLoginAuth2 response)
→ 302 Location: /pass2/userinfo/start?_locale=zh_CN&bizType=UserCross&callback=&
    data={"n":false}&sid=passport&sign=2%26V1_passport%26<sig>&token=<tok>
```
No special headers beyond standard browser-ish ones. The jar must contain
the serviceLoginAuth2 session cookies (Set-Cookie of the auth response).

### 4.3 GET /pass2/userinfo/start — hop 2

```
GET https://account.xiaomi.com/pass2/userinfo/start?<above query>
→ 302 Location: /fe/service/userCross?needClose=false&userId=<uid>&
    auth=<authToken>&callback=<urlencoded aistudio callback>&sign=<sign>
```
Server-side only. May set additional cookies into the jar. The `token` is
consumed here (one-time). **If our Go client auto-follows redirects, it will
land on the SPA HTML with the final URL in the address bar — and the
intermediate Location (carrying auth/callback/sign) is lost.** Never
auto-follow.

### 4.4 POST /pass2/repeatedPhone/sendTicket

Form-encoded POST (axios instance `p` in module 84285; Content-Type
`application/x-www-form-urlencoded; charset=UTF-8`; global axios default
`X-Requested-With: XMLHttpRequest` set in main.js module 37024).

Fields (module 30390 `pe`, built as `r` then passed through `_X`):
```
userId=<uid>                        # from URL
phone=+86xxxxxxxxxxx                # callingCode concat (module 38222 jM:
                                    #   +86 → bare phone, else cc+phone;
                                    #   hook builds "+86"===cc ? phone : cc+phone
icode=                              # "" first attempt; captcha icode on retry
recyclePhoneAllowed=false           # true only on step-4 confirm-bind path
# + cookie passthrough appended by _X (module 86704):
passport_ph, usercenter_ph, passportsecurity_ph, passportapi_ph,
passport_h5_ph, passToken   (from document.cookie, "" if absent)
```
Response (code=0): ticket sent to phone. The hook advances to step 2.
Response 25005: `values.bindInfo` — phone bound to another account; step 4
confirm-bind with `recyclePhoneAllowed=true`.
Response 87001/20031: captcha required → captcha retry loop (§6).

### 4.5 POST /pass2/repeatedPhone/verifyTicket

```
userId=<uid>
icode=                              # "" unless captcha retry
ticket=<sms code>
phone=+86xxxxxxxxxxx
auth=<authToken>                    # from URL (hop-2 Location)
callback=<callback>                 # from URL
sign=<sign>                         # from URL
recyclePhoneAllowed=true            # hardcoded true in the hook
# + _X cookie passthrough (passToken etc.)
```
Response code=0:
```
{ code: 0, location: "<callback URL with auth=...>", data: { bindType: "SAFE_PHONE" | ... } }
```
The page redirects to `location`. That URL is the aistudio callback →
completes login exactly like the OTP flow's step 7/8: follow it (cookies
land), then the aistudio side issues its session. In our Go flow this is
`stsExchange(loc)` — same shape as the password-success path.

### 4.6 POST /pass/sms/quota (aux, optional)

`{address: <fullPhone>, contentType: "150007", userId}` — returns
`{info: <int>}` (remaining SMS quota for this number). Cosmetic in the UI;
skip in Go or use for resend-gating.

### 4.7 Headers

- `Content-Type: application/x-www-form-urlencoded; charset=UTF-8` (both
  repeatedPhone POSTs)
- `X-Requested-With: XMLHttpRequest` — **mandatory** (main.js sets it on the
  global axios defaults; the reference login.mjs documents that omitting it
  on identity endpoints hard-fails 66108 — treat it as required everywhere)
- `Referer: https://account.xiaomi.com/fe/service/userCross?...` — the SPA
  runs same-origin; set it to be safe.
- Cookie: full jar (serviceLoginAuth2 session + any hop cookies; the `_X`
  passthrough values are ALSO in the form body — belt and suspenders).

### 4.8 Cookie-jar evolution

```
serviceLoginAuth2 ─┬─ jar: passport session cookies (passToken NOT yet set
                   │         at UserCross time — that's why the flow exists)
/pass2/redirect    ─┼─ may set hop cookies
/pass2/userinfo/start ─ may set hop cookies; consumes token
repeatedPhone/*    ─┴─ verification session cookies (server binds the
                        userId+auth+sign tuple to the jar)
verifyTicket OK    → follow location (callback+auth) → aistudio Set-Cookie
                     (serviceToken etc.) → stsExchange
```

## 5. Error codes and retry semantics (module 12752 + hook logic)

| code | meaning | action |
|---|---|---|
| 0 | ok | advance |
| 87001 / 20031 / 70014 / 1200212 | captcha wrong/required | run captcha retry loop |
| 10001 / 550015011 | system error | abort |
| 10012 | invalid verify request | abort |
| 10017 / 20014 | bad param | abort |
| 25005 (sendTicket) | phone already bound elsewhere | step-4 confirm-bind, `recyclePhoneAllowed=true`, resend |
| 25006 / 66006 | password-ish error | abort |
| 20003 | (register flow) | abort |
| 21314 / 21327 / 21317 | token expired | re-run login from serviceLoginAuth2 |
| 350008 | login frozen | abort |
| 70022 | SMS quota exhausted ("NoChance-SMS") | abort / surface to operator |
| 70016 | user error (tolerated by `w` handler) | surface |
| 110021001 / 110045011 / 66108 | param invalid (66108 = missing X-Requested-With signature) | fix headers |
| 302 (code field) | redirect demand | follow `data.location` |
| 401 (code field) | not logged in | follow `loginUrl` |

Captcha retry loop (module 78214 `useCaptcha` `_8`): on 87001/20031 the
wrapper loads the infosec SDK, runs `initMiverify`, on success sets cookies
`vToken` / `vAction` (domain-scoped, 1-day) and retries the API with the
SDK-provided `icode` — **loop continues while the SDK keeps succeeding and
the API keeps answering 87001/20031**. Human-visible interactive slider.

## 6. Captcha interplay — when is it required?

- The infosec captcha SDK (`https://captcha-cdn01.infosec.xiaomi.com/
  mcfe--captcha-static-sdk/prod/v3/static/js/v.js` — module 86611; the
  legacy domestic CDN is `static-verify.sec.xiaomi.com/v2/js/v.js`) is
  initialized with `initMiverify({ k: "8027422fb0eb42fbac1b521ec4a7961f",
  locale, errorAction: true, ... })`.
- **CAPTCHA_APP_KEY 8027422... is the mi-account web appkey for the verify
  SDK** — it scopes the slider widget + fingerprint to the account.xiaomi.com
  origin. It is NOT sent in any HTTP request field we replay; the SDK
  exchanges it server-side for the `icode` it returns.
- First `sendTicket` with `icode=""` is the normal path — captcha is only
  demanded when risk scoring flags the request (87001/20031). For a Go
  client with a clean jar and consistent UA this typically passes. If it
  doesn't, there is **no programmatic bypass**: the slider requires the SDK
  + a real browser fingerprint. Mitigation: keep the UA constant across the
  whole flow, reuse the jar, don't parallelize logins, add jitter.
- `icodeType: "antispam"` (used by sendTicket/verifyTicket via the hook's
  `E(u, {action: "bind-phone", icodeType: "antispam", captchaDisable: true,
  uid})`) tells the SDK which action channel to report ("bind-phone"). The
  `captchaDisable: true` flag suppresses the page's own proactive captcha
  UI — captcha only appears reactively via 87001/20031 retry.
- vToken/vAction cookies: the SDK sets these post-solve; they accompany the
  retried request. A Go client cannot synthesize them; if forced into the
  captcha loop the only options are (a) an operator-driven browser session,
  or (b) waiting out the risk window.

## 7. What remains uncertain & curl-able probes

Uncertainties (mostly due to the 908 failed chunk downloads):

1. **Exact final-URL query of the userinfo/start 302** — we infer
   auth/callback/sign ride on it; must verify by manual redirect-follow.
   (Confidence: high — module 30390 reads them from the URL and no other
   source exists in any downloaded chunk.)
2. Whether the UserCross route wrapper (chunk 9976) performs any extra
   prefetch (e.g. `/pass2/config` for region data — module 86604 shows the
   config fetch pattern with `_locale/sid/_uRegion` params; the UserCross
   hook itself doesn't call it, but the provider may mount it app-wide).
3. Whether `verifyTicket`'s success response ever omits `location` (the
   hook reads it unconditionally; a missing location would leave the page
   stuck on step 3 — likely the server always sends it on code=0).
4. Whether the `data` param on userinfo/start (`{"n":false}`) matters for
   the Go client (probably a UI hint; pass through verbatim).

Probe list (curl, run in order, `-c jar -b jar`, NO `-L`):

```bash
# P0: hop 1 — expect 302 to userinfo/start
curl -sv 'https://account.xiaomi.com/pass2/redirect?sid=passport&callback=<LONG_SIGN>' \
  -b jar -c jar -H 'User-Agent: <same UA as login>' -o /dev/null

# P1: hop 2 — expect 302 to /fe/service/userCross; CAPTURE Location query
curl -sv 'https://account.xiaomi.com/pass2/userinfo/start?_locale=zh_CN&bizType=UserCross&callback=&data=%7B%22n%22%3Afalse%7D&sid=passport&sign=<SIGN>&token=<TOKEN>' \
  -b jar -c jar -o /dev/null

# P2: sendTicket first attempt, no captcha
curl -s 'https://account.xiaomi.com/pass2/repeatedPhone/sendTicket' \
  -b jar -c jar \
  -H 'Content-Type: application/x-www-form-urlencoded; charset=UTF-8' \
  -H 'X-Requested-With: XMLHttpRequest' \
  -H 'Referer: https://account.xiaomi.com/fe/service/userCross?needClose=false&userId=<UID>' \
  --data-urlencode 'userId=<UID>' \
  --data-urlencode 'phone=+86<PHONE>' \
  --data-urlencode 'icode=' \
  --data-urlencode 'recyclePhoneAllowed=false' \
  --data-urlencode 'passport_ph=' --data-urlencode 'usercenter_ph=' \
  --data-urlencode 'passportsecurity_ph=' --data-urlencode 'passportapi_ph=' \
  --data-urlencode 'passport_h5_ph=' --data-urlencode 'passToken='
# expected: &&&START&&&{code:0,...} or 87001/20031 (captcha) or 25005 (bound)

# P3: verifyTicket with real SMS code
curl -s 'https://account.xiaomi.com/pass2/repeatedPhone/verifyTicket' \
  -b jar -c jar \
  -H 'Content-Type: application/x-www-form-urlencoded; charset=UTF-8' \
  -H 'X-Requested-With: XMLHttpRequest' \
  -H 'Referer: https://account.xiaomi.com/fe/service/userCross?needClose=false&userId=<UID>' \
  --data-urlencode 'userId=<UID>' --data-urlencode 'icode=' \
  --data-urlencode 'ticket=<SMS_CODE>' --data-urlencode 'phone=+86<PHONE>' \
  --data-urlencode 'auth=<AUTH_FROM_HOP2>' \
  --data-urlencode 'callback=<CALLBACK_FROM_HOP2>' \
  --data-urlencode 'sign=<SIGN_FROM_HOP2>' \
  --data-urlencode 'recyclePhoneAllowed=true'
# expected: &&&START&&&{code:0, location:"<callback?...auth=...>", data:{bindType:"SAFE_PHONE"}}
```

Verification of the final STS: GET the returned `location` with the jar
(manual redirects), confirm aistudio Set-Cookie lands, then compare against
the existing `stsExchange` contract.

## 8. Amendment plan for internal/mimo (Go) — pseudo-signatures

New file `internal/mimo/usercross.go`:

```go
// ErrUserCrossPending: Login hit the UserCross risk-control demand.
type ErrUserCrossPending struct{ Challenge *UserCrossChallenge }

// UserCrossChallenge: one started repeated-phone verification.
type UserCrossChallenge struct {
    ID        string    // "uc-<rand>"
    UserID    string    // from the hop-2 URL
    Phone     string    // full phone (+86...) used for sendTicket
    Notify    string    // masked phone for admin UI
    ExpiresAt time.Time // ~10 min

    passBase string            // https://account.xiaomi.com
    auth     string            // hop-2 auth token
    callback string            // hop-2 callback (aistudio)
    sign     string            // hop-2 sign
    jar      map[string]string // verification-session cookies
}

// StartUserCross walks the notificationUrl chain (redirects followed by
// hand — auth/callback/sign live on the hop-2 Location header), then sends
// the SMS ticket. sendTicket captcha result is returned in the challenge
// state so the admin endpoint can surface 87001/20031.
func (c *Client) StartUserCross(ctx context.Context, notificationURL, phone string, seedJar map[string]string) (*UserCrossChallenge, error)

// SubmitUserCrossCode posts verifyTicket with the SMS code and, on code=0,
// follows response.location (callback+auth) and runs stsExchange —
// same completion contract as SubmitOTP.
func (c *Client) SubmitUserCrossCode(ctx context.Context, ch *UserCrossChallenge, code string) (*Session, error)
```

Changes to `internal/mimo/login.go`:

```go
// Login(): in the code==0 && location=="" && notificationUrl!="" branch,
// classify the notificationUrl BEFORE starting OTP:
//   - parse query; if it contains a callback/sign pair (or, robustly: hand-follow
//     hop 1 and inspect the hop-2 Location query for "bizType=UserCross")
//     → StartUserCross
//   - else → existing StartOTP path (identity flow)
// The captured response's callback=<long-sign> with sid=passport and the
// bizType=UserCross marker on hop 2 is the discriminator.
func (c *Client) classifyNotification(ctx context.Context, nu string, jar map[string]string) (kind string, hop2Query url.Values, err error)
```

Changes to `internal/mimo/otp.go` (or usercross.go, shared plumbing):

```go
// hopFollow: GET a URL with manual redirects, returning each hop's
// Location (max N hops, host-allowlisted). Needed by both StartUserCross
// and classifyNotification.
func (c *Client) hopFollow(ctx context.Context, jar map[string]string, start string, maxHops int) (finalURL string, hops []string, err error)
```

Admin/API surface (mirror of the OTP endpoints from the prior session's
plan — pending store + pool Enable):

```go
// internal/api: POST /admin/usercross/{challengeID}/submit {code}
// pool: Enable(accountID, session) once SubmitUserCrossCode returns a Session
```

Tests (mock passport, mirroring otp_test.go):
- UC-001: code=0 + notificationUrl → hop chain 302→302 with bizType=UserCross
  → ErrUserCrossPending (not ErrOTPPending)
- UC-002: StartUserCross captures auth/callback/sign from hop-2 Location
- UC-003: sendTicket 87001 → challenge state "captcha-required", no crash
- UC-004: sendTicket 25005 → retry with recyclePhoneAllowed=true succeeds
- UC-005: verifyTicket code=0 + location → stsExchange called with that location
- UC-006: verifyTicket wrong code → error surfaced, challenge still valid
- UC-007: notificationUrl host not allowlisted → refused (security mirror of OTP-010)

Config: `MIMO2API_USERCROSS_AUTO` (default off — the SMS code requires an
operator, same posture as MIMO2API_OTP_AUTO).

## 9. Source index (for future greps)

- `4793.02a8247d.chunk.js`
  - module 84285 — repeatedPhone/upgradeUser API wrappers
  - module 30390 — UserCross page hook (params, steps, send/verify calls,
    25005 confirm-bind, success redirect)
  - module 78214 — useCaptcha (87001/20031 retry loop, vToken/vAction)
  - module 29936 — captcha action-channel map ("bind-phone" etc.)
- `5721.bfcaa3ca.chunk.js`
  - module 86704 `_X` — cookie passthrough into form body
  - module 73329 — axios instance, &&&START&&&/@json: unwrap, error handler,
    /pass/sms/quota
  - module 86611 — captcha SDK loader, initMiverify + appkey, v.js URLs
  - module 60374 — URL sanitizer (javascript: strip)
  - module 86604 — config hook (/pass2/config pattern, not used by UserCross)
- `main.js` module 37024 — global X-Requested-With, mobile/desktop split
- `main.js` module 27028 — userCross path regex → mobile mode
- `runtime.js` — chunk map: 9976="UserCross" (download failed), 5008/3068
  entries (failed), 4793/5721 present
- `references/xm2api/lib/login.mjs` — startOTP/otpSubmit (new-device flow;
  the Go otp.go mirrors it); X-Requested-With 66108 note

### Param provenance table

| Param | Where its value comes from |
|---|---|
| `userId` | hop-2 Location query (userinfo/start 302); also the account's passport userId |
| `auth` | hop-2 Location query — server-issued one-time verification token, bound to the session cookies |
| `callback` | hop-2 Location query — the aistudio serviceLoginAuth2 callback URL (originates from OUR login probe's loginUrl callback param, round-tripped through passport) |
| `sign` | hop-2 Location query — `2&V1_passport&<sig>`, server-generated, originated from the hop-1 callback long-sign |
| `token` | hop-1 Location query (userinfo/start) — one-time hop token, consumed by hop 2 |
| `phone` | user input on the page (step 1); callingCode concat rule: +86 → bare phone, else cc+phone |
| `ticket` | the SMS code received on the phone |
| `icode` | "" on first attempt; infosec captcha SDK result on 87001/20031 retry |
| `recyclePhoneAllowed` | false initially; true on 25005 confirm-bind path and hardcoded true in verifyTicket |
| `passport_ph`…`passToken` (form fields) | document.cookie passthrough (module 86704 `_X`) — "" when absent |
| `needClose` | hop-2 Location query — UI hint (show close button), ignore in Go |
| `countryListOnlyCN` | hop-2 query — UI hint (lock country list to +86), ignore in Go |
| `confirm` | page query — the bindPhoneAddress redirect target for the email-bind variant (module 30390 `S` hook), validated against account.xiaomi.com hostnames |
