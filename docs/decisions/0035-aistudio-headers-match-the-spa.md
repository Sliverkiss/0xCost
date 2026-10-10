# 0035 — The aistudio surface sends exactly what the Studio SPA sends

Status: accepted (2026-10-10)

## Context

SDD §2.4 documented the chat request's header set from an assumption: that the
Studio SPA's HTTP layer was axios-based, so `Accept: application/json, text/plain,
*/*` and `X-Requested-With: XMLHttpRequest` were "axios defaults" it must be sending.
Both were set on every aistudio-surface request (`internal/mimo/client.go`), on top
of the headers the SPA's own client does contribute.

The whole bundle was then read rather than argued about: `main.js` (753 KB) plus
every lazy chunk the runtime maps (452 files, 17.8 MB) were downloaded from the
page origin and grepped. Three facts settle it:

- **The SPA is not axios.** `axios` and `application/json, text/plain, */*` appear
  **0 times** in the entire bundle. Its client is a small `fetch` wrapper whose
  base class merges `{...this.headers, ...getHeaders(), ...callers}`; child
  instances pass only `{prefix, handleError}`, so `this.headers` is empty and the
  only headers the client itself adds are `getHeaders()`'s
  `{"Accept-Language": locale || "zh-CN", "x-timeZone": <IANA>}` plus
  `Content-Type: application/json` when the payload is not FormData/Blob/File.
- **`X-Requested-With` appears 0 times** in the bundle. Neither does
  `Authorization`. Identity on this surface is the Cookie (the client is
  constructed with `credentials: "same-origin"`) plus the `xiaomichatbot_ph`
  **query parameter** that the overridden `post()` appends.
- The rest of the wire header set (`Accept: */*`, `Origin`, `Referer`,
  `sec-fetch-*`, `sec-ch-ua*`, User-Agent) is browser-added, and we already
  reproduce it.

So the gateway was sending two headers its own reference client never sends. An
extra header is not "harmless completeness": it is the one place a fingerprint
differs from the client the platform sees all day, which is exactly the category of
mistake the device-identity incident already taught this project.

## Decision

1. **The aistudio-surface header set is exactly**: `Content-Type:
   application/json` (JSON bodies only), `Accept: */*`, `Accept-Language: zh-CN`,
   `x-timeZone: <configured zone>`, `Origin`/`Referer` = the Studio base, Chrome
   `User-Agent` with a matching `sec-ch-ua`, `sec-ch-ua-mobile`,
   `sec-ch-ua-platform`, `sec-fetch-dest/mode/site`, and the account's `Cookie`.
   Nothing else. In particular **no `X-Requested-With` and no `Authorization`**.
2. **The identity surface keeps its own set.** There `X-Requested-With` is
   mandatory — the platform answers `66108` without it
   (`docs/usercross-reverse.md`) — so `passport.go`, `otp.go` and `consent.go` keep
   setting it. The two surfaces now have deliberately different functions
   (`setAistudioHeaders` vs the passport set); the old name
   `setPlatformXHRHeaders` described a mechanism (XHR) this surface does not use.
3. **Absence is asserted, not just presence.** `fidelity_test.go` gained a
   `forbiddenBrowserHeaders` list checked on both the chat call and the validation
   probe, so a future "let's add a header for safety" cannot land silently.
4. **The correction is doc-anchored.** `docs/web-surface-reverse.md` §11.7.1
   carries the grep table (keyword → hit count) that this decision rests on, so the
   next reader can re-derive it instead of re-arguing it.

## Consequences

- The upstream request is now indistinguishable, at the HTTP-header layer, from the
  request the SPA itself makes for the same action. What remains different is only
  what an out-of-browser client necessarily has: an IP that is not the user's, and
  a request rate.
- The claim in SDD §2.4 that these two headers were "axios defaults" is corrected in
  place rather than deleted, so the wrong reasoning is not re-introduced by someone
  reading the old revision.
- Streaming is unaffected by this change: the SPA's `completions()` sends
  `isStreaming: true` as a **client-side** flag that changes only how the response
  body is read (return the `Response` vs `res.text()` + `JSON.parse`). It adds no
  header, no query parameter and no body field — the upstream answers SSE either
  way, so one header set covers both transports (ADR 0034).

## Alternatives rejected

- **Keep both headers "because they never hurt".** They never helped either, and
  the reference client proves neither is required. The cost of keeping them is
  precisely the signal we are trying to remove.
- **Drop `X-Requested-With` everywhere, including the identity surface.** It is
  mandatory there (`66108`); the surfaces are not interchangeable and one shared
  function is what produced this mistake.
- **Wait for a live capture of the SPA instead of reading the bundle.** We do not
  run a browser against the platform (that is a deliberate boundary here), and the
  bundle is the same artifact the browser executes — the grep answers the question
  completely, including the negative.
