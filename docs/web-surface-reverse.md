# Web 面能力逆向（语音 / 检索 / Claw / 目录）

状态：**真机实测**（现役测试账号，userID `2652042633`，2026-10-09）。账号标识不写入仓库（docs/TDD.md §6.7）。
本文只记已实测的事实；未实测的以 `未验证` 标注。桌面面（`mimo-server-cn.xiaomimimo.com`）
本轮明确不做，结论仅备查（见 §9）。

素材：`aistudio.xiaomimimo.com` SPA（`main.911869f9.chunk.js` + 435 个懒加载 chunk，
取自 `chunkmap.json` 的 `<id>.<hash>.chunk.js`），探针脚本
`~/.hermes/cache/scratch/mimo_surfaces/{probe_surfaces2,probe_desktop,probe_tts}.py`。

---

## 0. 结论速览

| 面 | 上游协议 | 状态 |
|---|---|---|
| TTS 语音合成 | `conversation/save` → `tts/v2/generate` → `tts/generateStatus` → 公共 FDS URL | **实测通过**（WAV 145,964 B 落盘） |
| ASR 语音识别 | `resource/genUploadInfo` → `PUT` → `asr/recognize` → `asr/recognizeStatus` | **实测通过**（TTS→ASR 闭环逐字一致） |
| 联网检索 | chat `modelConfig.webSearchStatus` + `doc`/`web_search` SSE 事件 | **实测通过**（12 条结果，含正文） |
| 模型目录 | `GET /open-apis/bot/config` | **实测通过**（24 模型 + 开关位 + 音色表） |
| MiMo Claw | `/open-apis/user/mimo-claw/{create,status,quotaUsage,tokenPlanUsage}` + `wss:///ws/proxy` | 控制面实测（账号当日**未领取**，`status=NOT_CREATED`） |
| 用量细节 | `usage.nativeUsage.*`（cached/reasoning tokens） | 实测通过 |
| RTC 实时语音 | `/open-apis/chat/liveKit/getRtcConfig` | 未验证 |

---

## 1. 传输与鉴权（关键）

- Base：`https://aistudio.xiaomimimo.com`
- 会话 cookie（`xiaomichatbot_*`）**单独不够**：`/open-apis/bot/chat`、`/open-apis/tts/*`、
  `/open-apis/asr/*` 都必须**再带 `?xiaomichatbot_ph=<ph>` 查询参数**，否则 401。
  `/open-apis/user/mi/get` 之类的探活不需要 ph（这就是"cookie 看起来有效却 401"的原因）。
  ph 值 = 会话 cookie 里 `xiaomichatbot_ph` 的 URL 编码值。
- 出站指纹与 SPA 一致：UA + `sec-ch-ua*` + `sec-fetch-*` + `Origin` + `Referer` +
  `x-timeZone` + `Accept: application/json, text/plain, */*` + `X-Requested-With: XMLHttpRequest`
  （见 `internal/mimo/client.go` `setBrowserHeaders`，已对齐）。
- 上游错误信封：HTTP 200 时也可能 `{"code":<非0>,"msg":"..."}`；`code:400` + `msg` 为业务错
  （如 `conversation not exist`、`modelCode cannot be empty`）。

## 2. 会话（conversation）生命周期 —— TTS/ASR 的前置条件

`POST /open-apis/chat/conversation/save`（`type ∈ chat|tts|asr|tts-vd|tts-vc`，即页面类型）

- **创建**：body **不带 `id`**，带 `{conversationId:<32hex 客户端自造>, title, type}`
  → `200 {"code":0,"data":{"id":9655605,"conversationId":"<回显>","creator":"2652042633","createTime":"2026-10-09 12:48:56","title":"asr-probe","type":"asr"}}`
- **更新**：带上数字 `id` 即更新该行（同 type 复用时返回同一 `id`）。
- 带 `id:0` 或引用不存在的会话 → `{"code":200,"msg":"参数校验失败，错误信息：会话不存在"}`。
- `POST /open-apis/chat/conversation/list {pageInfo:{pageNum,pageSize}}`
  → `{"code":0,"data":{"total":N,"pageNum":1,"dataList":[...]}}`。
  **纯 chat 不会创建会话实体**（实测 chat 后 total=0），所以 TTS/ASR 之前必须显式 save。
- `POST /open-apis/chat/dialog/list {queryParam:{conversationId,...}}` 列对话明细。
- 会话实体 schema（SPA `d.QK`）：`{id:number, conversationId:string, title:string,
  type:string, tpsMetrics?:string|null, createTime:string, updateTime:string}`。

## 3. TTS —— 文本转语音

1. `POST /open-apis/tts/v2/generate?xiaomichatbot_ph=<ph>`
   ```json
   {
     "conversationId": "<§2 创建得到的 id>",
     "msgId": "<毫秒时间戳字符串>",
     "content": {
       "messages": [
         {"role": "user", "content": "请用自然、亲切的语气朗读下面的文字。"},
         {"role": "assistant", "content": "你好，这是一次语音合成测试。"}
       ],
       "audio": {"format": "wav", "voice": "冰糖"}
     },
     "modelConfig": {"modelCode": "mimo-v2.5-tts"}
   }
   ```
   - `content.messages` **必须是两条**（SPA 侧是二元组）。**第二条**是要朗读的正文。
     SPA 会把正文里的圆括号/方括号/花括号统一改写成 `[` `]`（控制标记语法）。
   - 角色不敏感（`user+assistant` 与 `user+user` 均成功）；`modelConfig.scene` 可省
     （`VOICE_COMMAND` 也成功）。
   - `modelCode` 必须是 `bot/config` 里真实存在**且未被取代**的（未知名 → `{"code":400,"msg":"modelCode cannot be empty"}`；已被取代的 id → `{"code":400,"msg":"model: [mimo-v2-tts] not support"}`，2026-10-09 真机）。
  - **`redirectTo`（2026-10-09 真机取证）**：`modelConfigListNg` 的每一行可能带 `redirectTo`，指向平台真正提供的 id。当前账号 25 行里 **18 行已废弃**，含链式（`mimo-v2-flash-studio` → `mimo-v2-flash` → `mimo-v2.6-flash`）。SPA 用它构造 `deprecatedModelRedirectMap`，发请求前先归位；我们同样归位（ADR 0011），并且**不再把这些 id 列进 `/v1/models`**。
  - chat 面对已废弃 id 的拒绝形态不同：不是 400，而是复用繁忙帧 `服务器繁忙，请稍后再试`（真机同一分钟交替 3 轮：当前 id 3/3 成功、废弃 id 3/3 繁忙）。
2. 响应：`{"code":0,"data":{"taskId":16063236,"conversationId":"...","msgId":"1791521173154","status":"generating"}}`
3. `GET /open-apis/tts/generateStatus?taskId=<n>&xiaomichatbot_ph=<ph>`
   → `{"code":0,"data":{"status":"success","audioUrl":"https://alsgp0.fds.api.xiaomi.com/chatbot-prod/multimedia/2652042633/...","audit":{"status":"PASS","blocked":false}}}`
   - `status ∈ generating|success|failed`；`audit.status ∈ PASS|PROCESSING|...`（PROCESSING = 审核中，SPA 继续轮询）。
   - 合成 ~1–2 s 完成；实测 WAV 145,964 B（21 字）。
   - `audioUrl` 是**公共 FDS 直链**，无需 cookie 即可下载（WAV 头 `RIFF`）。
4. 其它 TTS 端点（SPA 存在，未逐个实测）：`tts/generateStatus`、`tts/download`、
   `tts/generateStyle`（音色设计 `tts-vd`）、`tts/generateSpeechText`、音色克隆 `tts-vc`。
5. 音色表：`GET /open-apis/bot/config` → `data.ttsVoices[] = {id,name,description,avatar,previewAudio?}`；
   本账号 8 个（如 `冰糖`、`Mia`、`茉莉`、`Chloe` …）。

## 4. ASR —— 语音转文本（需先上传音频）

1. `POST /open-apis/resource/genUploadInfo?xiaomichatbot_ph=<ph>` `{"fileName":"a.wav","fileContentMd5":"<md5hex>"}`
   → `{"uploadUrl":"https://alsgp0.fds.api.xiaomi.com/chatbot-prod/multimedia/<uid>/a.wav?GalaxyAccessKeyId=...","resourceUrl":"<同上>","objectName":"multimedia/<uid>/a.wav"}`
2. `PUT <uploadUrl>`，头 `Content-Type: application/octet-stream`，body 为文件原始字节
   （带 `content-md5` 时**必须与声明值一致**）→ `200`。
3. `POST /open-apis/asr/recognize?xiaomichatbot_ph=<ph>`
   ```json
   {"conversationId":"<§2 创建，type=asr>","msgId":"<毫秒>",
    "audioUrl":"<resourceUrl>","language":"zh",
    "modelConfig":{"modelCode":"mimo-v2.5-asr"}}
   ```
   → `{"code":0,"data":{"taskId":16063666,"status":"generating",...}}`
4. `GET /open-apis/asr/recognizeStatus?taskId=<n>` → `{"status":"success","text":"你好，这是一次语音合成测试。","audit":{...}}`
5. 其它：`POST /open-apis/resource/parse {queryParams:{fileUrl,objectName,model}}`（文档解析，空文件 `code=6002`）、
   `GET /open-apis/resource/genNewUrl?oldFileUrl=`（旧 URL 换新）。

**闭环证据（实测）**：TTS 合成 `你好，这是一次语音合成测试。` → 上传 wav → ASR 返回
`你好，这是一次语音合成测试。`（逐字一致，约 2 s）。

## 5. 联网检索（chat 内）

- 请求：`modelConfig.webSearchStatus ∈ disabled|auto|enabled`（SPA 枚举 `RATIO/TRUNCATE` 对应
  `tip_ratio`/`tip_truncate` 事件，属截断提示）。
- 模型能力位：**本账号实测 `modelConfigListNg[]` 只有 `{model,name,pageType,redirectTo,intro}`，
  没有 `features`**。能力位在 `modelConfigList[]`（chat 行）：`webSearchDefaultStatus`
  (`auto`|`disabled`|缺省)、`thinkingDefaultOn`、`showWebSearch`（实测全为 `false`，是 UI 开关，
  **不代表不支持检索**）。SPA 里的枚举是 `DISABLED=0 / ENABLED=1 / ENABLED_BY_DEFAULT=2`，
  本项目的 `capabilities.web_search` 即按此语义从 `webSearchDefaultStatus` 推导。
- **实测**：即便 `webSearchStatus:"disabled"`，上游仍会返回检索事件（三种模式都观察到），
  即该字段不构成硬闸门。
- SSE 事件序列：`dialogId` → `doc` → `web_search` → `message`… → `usage` → `finish`：
  - `event:doc` `data:{"content":"webSearch"}` —— 检索开始标记
  - `event:web_search` `data:[{...}]` —— **结果数组**，每项：
    `{id:"2026-10-09_<32hex>_2", text:"<网页正文，可达数十 KB>", metadata:{}(**对象**),
      name:"<标题>", url:"<原文>", displayUrl:"mimo.mi.com", snippet:"<摘要>",
      usedContent:false(**JSON 布尔**)}`
  - **实测（2026-10-09，两次真机抓取，本项目踩过的坑）**：`usedContent` 是布尔而不是
    `"True"` 字符串、`metadata` 是对象而不是 `"{}"` 字符串。字段类型写错会让**整个数组
    反序列化失败** → 结果被静默丢弃（曾导致"检索有结果但引用为 0"）。因此解析器按字段
    容错：字段怪形状只损失该字段，不损失该条引用，更不失败整轮。
  - 一次检索最多 16 条（早期抓包见 12 条）；单帧 payload 实测 **94 KB**（其中正文占绝大部分）。
  - 正文 `text` **不向前透出**（数十 KB 会撑爆客户端上下文），只在 debug 日志里记录长度。

## 6. 用量扩展字段

`usage` 事件除 `prompt_tokens/completion_tokens/total_tokens` 外含
`nativeUsage.prompt_tokens_details.cached_tokens` 与
`nativeUsage.completion_tokens_details.reasoning_tokens`。

## 7. 模型目录 / 开关位

`GET /open-apis/bot/config?xiaomichatbot_ph=<ph>`（需 ph）→ `data`：

- `modelConfigListNg[]`：24 个模型，含 `mimo-v2.6-pro`/`-flash`、`mimo-v2.5-*`、
  7 个 TTS 模型、1 个 ASR、`clawm-alpha`/`clawl-alpha`（Claw 两档）；
  每项含 `features.{webSearch,thinking,...}`、`pageType`、`generation` 默认参数。
- `ext`：`enableClaw` `enableUltraspeed` `enableVoice` `enableCustomVoice`
  `isForeignVersion` `enableV26`。本账号实测：`enableClaw=false`、
  `enableUltraspeed=true`、`enableVoice=false`、`enableCustomVoice=true`。
- 其它：`claw{enabled,recommendQuestions,welcomeExamples,fileUpload,chatQrCodes}`、
  `voiceConfig.voice[]`、`ttsVoices[]`、`recommendQuestionsNg`、`ttsShowCase`。

## 8. MiMo Claw（每日领取制）

- 控制面（`/open-apis/user` 前缀）：
  - `GET /open-apis/user/mimo-claw/status` → `{"status":"NOT_CREATED","requestResourceType":"FREE","resource":{...},"billing":{"edition":"FREE","tokenPlan":{"status":"NONE"}}}`
  - `POST /open-apis/user/mimo-claw/create` —— **领取当日额度**（每账号每天 3 小时）
  - `GET /open-apis/user/mimo-claw/quotaUsage` → `{"exists":false,"maxBytes":0,...}`
  - `GET /open-apis/user/mimo-claw/tokenPlanUsage` → `{"status":"NONE"}`
- 数据面：`GET /open-apis/user/ws/ticket?...` 取票据 → `wss://<host>/ws/proxy`
  （未领取时 `ws/ticket` 直接 400 `Mimo Claw资源当前不可用`）。
- 握手（SPA bundle 里读到的字面量）：
  ```json
  {"minProtocol":3,"maxProtocol":4,
   "client":{"id":"cli","version":"mimo-claw-ui","platform":"<...>","mode":"cli"},
   "role":"operator",
   "scopes":["operator.admin","operator.read","operator.write","operator.approvals","operator.pairing"],
   "caps":["tool-events"],"userAgent":"<...>","locale":"<...>"}
  ```
  服务端先发 `event: connect.challenge {nonce}`，客户端再发 `connect` 请求，回 `hello`；
  之后是方法调用/响应/事件（含 tool-events）。prompt 里用 `<mimo-files>` JSON 块引用文件。
- 账号侧另有 `/mimo-claw/{create,status,destroy,restart,bindApiKey}` 这组字面量出现在
  小米账号 passport SPA（`com.xiaomi.account.pc.miclaw`）里，属 Claw 桌面/客户端壳，
  **不在 aistudio 域**（aistudio 上该路径 404）。

## 9. 桌面面（本轮不做，备查）

`mimo-server-cn.xiaomimimo.com` 是 MiMo 桌面/MiClaw 客户端的 OpenAI 兼容面：
`/api/route/chat/completions`（流式 + `audio.format` 出 TTS、`input_audio` 入 ASR、
`usage.seconds`/`audio_tokens`）、`/api/route/images/generations`、`/api/model/list`、
`/api/user/usage`，鉴权用两阶段 SSO（`sid=mimopc`）换 `serviceToken`。

**实测结论：不可复用现有凭据。** 用本账号 passToken 对 `sid=mimopc` 做自动登录一律
`code=70016 登录验证失败`（无 cookie 时该 sid 的 `callback=https://mimo-server-cn.xiaomimimo.com/api/sts`
可正常读出，说明 passToken 是**按 sid 作用域**签发的）。要用桌面面必须做一次 mimopc 登录
（密码 + 短信），归入"短信后面再说"。

## 10. 对本项目的落点（实现映射）

| 网关能力 | 上游 | 备注 |
|---|---|---|
| `POST /v1/audio/speech` | §3 | OpenAI 兼容：`input`/`voice`/`response_format` → 返回音频字节 |
| `POST /v1/audio/transcriptions` | §4 | multipart 上传 → 上传上游 → 识别 |
| `GET /v1/audio/voices` | §7 `ttsVoices` | 音色目录（OpenAI 无此端点，扩展） |
| chat 引用透出 | §5 | 把 `web_search` 结果并入响应（`annotations` 风格）/ SSE 扩展事件 |
| `web_search` 请求开关 | §5 | 客户端可选 `web_search_status: disabled\|auto\|enabled` |
| `GET /v1/models` 真实目录 | §7 | 用 `bot/config` 覆盖静态表，含能力位 |
| usage 细节 | §6 | `prompt_tokens_details.cached_tokens` / `completion_tokens_details.reasoning_tokens` |
| 运维：Claw 状态/领取 | §8 | `GET /v1/admin/claw/status`、`POST /v1/admin/claw/claim` |
| 运维：会话管理 | §2 | TTS/ASR 前置会话按账号缓存复用（避免每次 save） |

---

## 11. chat 通道 vs ultraspeed（fastchat）通道

### 11.1 路由规则（真机确认）

- 模型 id `mimo-v2.6-pro-ultraspeed-studio` **必须**走 `/fastchat/open-apis/bot/chat`；
  走普通通道 `/open-apis/bot/chat` 会被上游以 `event:error {"type":"text","content":"模型名称错误"}`
  拒绝（HTTP 200 + SSE error 帧，实测）。
- 反向：fastchat 通道**接受**普通模型（`mimo-v2.6-pro` 实测 200），只是没意义。
- fastchat 通道同样返回 `event:dialogId`，即**有服务端会话状态**，`previousDialogueId`
  链式续聊可用（实测 6 轮链式上下文正常）。

### 11.2 时延实测（同账号、同一账号会话、短问句）

| 通道 / 模型 | TTFB | 总耗时 | 事件 |
|---|---|---|---|
| `/fastchat/...` + `mimo-v2.6-pro-ultraspeed-studio` | **1.40 s** | 1.54 s | `dialogId`×1, `usage`×5, `message`×2, `finish`×1 |
| `/open-apis/bot/chat` + `mimo-v2.6-pro`（基线） | 5.44 s | 5.77 s | 同上 |
| `/open-apis/bot/chat` + ultraspeed 模型 | 0.60 s | 0.60 s | `error`×1（模型名称错误） |

注意 **fastchat 通道会连发 5 个 `usage` 事件**（普通通道 1 个）——适配器必须按
"last-wins" 处理，不能用"首个 usage 即最终值"。

### 11.3 SPA 常量（真机 bundle 提取）

- `ultraspeedModelCode`（导出名 `E_`）= `"mimo-v2.6-pro-ultraspeed-studio"`；
  其模型档案 `Gm`：名称 `MiMo-V2.6-Pro UltraSpeed`，`pageType: chat`，
  `features.webSearch = DISABLED`、`features.thinking = DISABLED`、`scene.enabled=false`。
- 默认档案 `Yi`：`{name:"MiMo Default", model:"mimo-v2-flash"}`。
- `ultraspeedQueryLimitLength = 5e4`（**50,000 字符**上限，按 rune 计与 JS UTF-16 长度等价）。
- `ultraspeedContextRoundLimit = 3`（见 §11.4）。
- 请求体构造（两个通道完全一致）：
  ```js
  modelConfig = {enableThinking, webSearchStatus, model, temperature?, topP?}
  payload     = {msgId, conversationId, query, isEditedQuery,
                 previousDialogueId, sceneType: payloadSceneType,
                 params: payloadSceneParams, modelConfig, multiMedias}
  ```
  其中 `webSearchStatus = (webSearchEnabled && 附件数===0) ? sessionWebSearchMode : DISABLED`
  —— **带附件时不检索**，这条要在检索开关实现（M19）里复刻。

### 11.4 "只保留 3 轮"是客户端策略，不是服务端上限（实测）

实验（fastchat + ultraspeed，同一账号）：

| 方式 | 第 1 轮 | 第 6 轮回答 | 结论 |
|---|---|---|---|
| 链式：同一会话 6 轮、逐轮带 `previousDialogueId` | 植入"幸运数字 4913" | `4913` ✓ | 服务端保留 **≥6 轮** |
| 全量重放：每轮**新开会话**，把历史拼进 `query` | 同上 | `4913` ✓ | 客户端自管上下文同样可行 |

因此：
1. `ultraspeedContextRoundLimit: 3` 只是 Studio 前端喂上下文的策略，**服务端不截断**。
2. 网关**不继承**这 3 轮限制：流式继续用服务端链式（sticky，省 token），
   非流式/缓存缺失时用全量重放（本网关 `replayCap` 轮），并在 fastchat 通道
   按 50,000 字符预算裁剪。
3. 两条路径都经真机验证，可按成本/质量自由取舍。

### 11.5 端到端（经本网关）

`/v1/chat/completions` 对 ultraspeed 模型自动路由 `/fastchat/...`；6 轮对话（流式）
第 6 轮仍能取回第 1 轮植入的事实（脚本 `gw_ultra_e2e.py`）。

### 11.6 上游 in-band 错误分类（`event:error` 的 content）

上游把两种性质完全不同的情况都塞在同一个 `event:error` 帧里，必须**按内容分类**处理：

| `content` | 性质 | 实测 | 正确处置 |
|---|---|---|---|
| `服务器繁忙，请稍后再试` | **瞬时**平台限流 | 第 4/5 轮繁忙，第 6 轮即恢复正常；同一段测试里延迟在 0.7s–126s 抖动 | 800ms 后有界重试一次；仍繁忙 → 503 `upstream_busy` + `Retry-After: 2`；**账号保持 available**（是平台负载，不是账号故障） |
| `模型名称错误` | **确定性**：该通道拒绝此模型 id | ultraspeed 模型打到普通通道必现 | 400 `model_not_available_on_channel`，**不重试**，并打 warn（说明网关路由/目录与上游不一致） |
| 其他 | 该轮失败 | — | 失败并透出（历史行为） |

回归：`internal/mimo/sse_test.go` SSE-014（分类）、`internal/api/upstream_errors_test.go`
BUSY-001/BUSY-002/MODEL-001（重试与映射，含"账号不被降温"）。
