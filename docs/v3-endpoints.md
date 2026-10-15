# v3 endpoint inventory — full re-reverse of the live SPA (2026-10-16)

Source: `runtime-main.94df3f32.js` + `main.f4f92c2a.chunk.js` + all 448 async
chunks downloaded from `https://aistudio.xiaomimimo.com/` (the current
production bundle). Method: string-extract every `"/open-apis/..."` prefix and
`.post/.get("/...")` pair, then read each client module in full.

## Channel architecture (module 4816, build-time `s="production"`)

- `aw` (isUltraBuild) = `["ultra","ultra-review","ultra-production"].includes("production")`
  = **false** on this bundle. So `k = ""` (fastchat env prefix) but
  `C = "/fastchat"` (the *hard* ultra channel prefix) is unconditional.
- Build vars: `w = /open-apis/v1/genLoginUrl`, `y = /open-apis/user/mi/logout`,
  `b = xiaomichatbot_ph` (the ph cookie name, also injected as a query param on
  every POST by the base client class — `Ri(wl)` reads the cookie).

## Chat routing (modules 82854 `Jy`/`bX`, 73255 `GY`, 74420 `_`)

- `isUltraModel(e)` = `e === globalStore.ultraspeedModelCode` (server-supplied
  via `bot/config` → `ultraspeedConfig.modelCode`; bundle fallback constant
  `z = "mimo-v2.6-pro-ultraspeed-studio"`).
- `Jy(model)` = `!isUltraModel(model) && s() ? "v3" : "v2"` — **ultraspeed
  always gets "v2"**; `s()` = reactEnabled flag from `user/mi/get` (fresh
  session: `reactEnabled: true`) AND model.pageType === "chat".
- `GY(model)` = `{version: Jy(model), viaFastchat: isUltraModel(model)}`.
- Version → endpoint (module 80032 `OR`):
  - v3 → `reactCompletions` → `POST /open-apis/react/chat`
  - v2 → `completions(path, {viaFastchat})`:
    - viaFastchat (ultra) → client `A` (prefix `/fastchat`, raw handleError) →
      `POST /fastchat/open-apis/bot/chat`
    - else → base client → `POST /open-apis/bot/chat`
- Retry protocol: on v2 error frame matching `[["平台升级","刷新页面"],
  ["mimostudioupgraded","refreshyourpage"]]` → retry once with v3; on v3 404 →
  retry once with v2 (and flip the persisted reactEnabled probe).

## Live verification with the account's fresh session (2026-10-16)

| Endpoint | Status | Notes |
|---|---|---|
| `POST /open-apis/react/chat` (mimo-v2-flash/pro, sceneType QA) | **200 SSE** | works; consecutive calls answered `event:error 服务器繁忙` (busy throttle) but channel accepts the account |
| `POST /open-apis/bot/chat` (v2 studio) | 451 `????` | channel blocked (BUGS-FOUND #7 correction) |
| `POST /fastchat/open-apis/bot/chat` (ultraspeed) | 451 `临时封禁` (JSON) | same block, fastchat variant |
| `POST /open-apis/react/chat` (ultraspeed model) | 200 + `event:error 模型名称错误` | react channel refuses ultra model ids |
| `POST /open-apis/tts/v2/generate` | 451 `????` | generation-blocked |
| `GET /open-apis/bot/config` | 200 | read path fine |
| `POST /open-apis/chat/conversation/save` | 200 | read-adjacent write fine |
| `POST /open-apis/react/dialog/list` | 200 (code 2002 no-perm for foreign conv) | route exists |
| `GET /open-apis/user/mi/get` | 200 | `reactEnabled: true`, `bannedStatus: TEMPORARY` |

**Upstream truth: there is NO v3 path for ultraspeed, TTS or ASR.** The web's
ultraspeed traffic goes to `/fastchat/open-apis/bot/chat` (v2 payload); TTS is
`/open-apis/tts/v2/generate` + `GET /open-apis/tts/generateStatus`; ASR is
`/open-apis/asr/recognize` + `GET /open-apis/asr/recognizeStatus`. The *only*
v3 (react) surfaces are `POST /open-apis/react/chat` and
`POST /open-apis/react/dialog/list`. All generation channels except react are
451-blocked for this account — the account's TEMPORARY ban covers fastchat+v2
chat, TTS, and (by the same 451 `????` signature) ASR; react is exempt.

## Full endpoint table (SPA → what we use)

| SPA client | Endpoint | Ours |
|---|---|---|
| OR.completions (v2) | `POST /open-apis/bot/chat` | chat (remove after v3-only) |
| OR.completions (fastchat) | `POST /fastchat/open-apis/bot/chat` | ultraspeed (keep — only ultra route) |
| OR.reactCompletions | `POST /open-apis/react/chat` | chat v3 (keep) |
| OR.get history | `POST /open-apis/chat/dialog/list` / `/fastchat/...` / `POST /open-apis/react/dialog/list` | not used (stateless) |
| OR.save | `POST /open-apis/chat/conversation/save` (+`/fastchat` variant, type "ultra"→"chat") | audio.go conversation warm-up (keep) |
| OR.getList | `POST /open-apis/chat/conversation/list` | not used |
| OR.genTitle | `POST /open-apis/chat/conversation/genTitle` | not used |
| OR.feedback | `POST /open-apis/chat/dialog/feedback` (+fastchat) | not used |
| OR.batchDelete | `POST /open-apis/chat/conversation/delete` (+fastchat) | not used |
| bot config | `GET /open-apis/bot/config` | catalog (keep) |
| TTS (829) | `POST /open-apis/tts/v2/generate`, `GET /open-apis/tts/generateStatus`, `POST /open-apis/tts/generateStyle`, `POST /open-apis/tts/generateSpeechText`, `POST /open-apis/tts/download` | audio.go (keep paths; no v3 exists) |
| ASR (80032 ao) | `POST /open-apis/asr/recognize`, `GET /open-apis/asr/recognizeStatus` | audio.go (keep) |
| resource | `POST /open-apis/resource/genUploadInfo`, `POST /open-apis/resource/parse`, `GET /open-apis/resource/genNewUrl` + PUT uploadUrl | attach.go/audio.go (keep) |
| user | `GET /open-apis/user/mi/get`, `GET /open-apis/user/ws/ticket`, mimo-claw suite | login.go (keep) |
| login | `GET /open-apis/v1/genLoginUrl` → account.xiaomi.com serviceLogin → `/sts?sign=` → 307 `/open-apis/user/mi/get` | passport chain (unchanged, verified working) |
| audit | `POST /open-apis/audit/submit` | not used |
| share | `POST /open-apis/share/createShare`, `GET /open-apis/share/getShare/{id}` | not used |
| liveKit | `POST /open-apis/chat/liveKit/getRtcConfig` | not used |
| host-files | `GET /open-apis/host-files/list`, `POST /preview|/download|/download-archive|/delete` | not used |
| metrics | `POST /fastchat/open-apis/chat/dialog/metrics/{save,user/get}` | not used |
| claw channels | `POST /open-apis/user/mimo-claw/channel/logout`, `channel-login/qrcode`, `GET channel-login/status` | not used |
| agreement | `POST /open-apis/agreement/event` | consent (keep) |
