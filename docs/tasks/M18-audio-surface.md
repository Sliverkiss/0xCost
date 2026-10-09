# M18 任务书：语音面（TTS/ASR）接入网关

## 目标

在 `mimo2api` 网关新增 **OpenAI 兼容的语音能力**，上游用**网页面**协议（已真机验证）：

1. `POST /v1/audio/speech` —— 文本转语音（TTS）
2. `POST /v1/audio/transcriptions` —— 语音转文本（ASR，multipart 上传）
3. `GET /v1/audio/voices` —— 音色目录（本项目扩展端点，OpenAI 无此端点）

**权威协议文档：`docs/web-surface-reverse.md` §1–§4 与 §7（音色表）**。该文档是实测结论，
以它为准；不要凭 SPA 压缩代码的猜测改写。所有出站请求必须带 `?xiaomichatbot_ph=<ph>`
（§1），并复用现有 `internal/mimo/client.go` 的浏览器指纹头。

## 已真机验证的上游链路（照此实现）

### 会话（前置）
```
POST /open-apis/chat/conversation/save?xiaomichatbot_ph=<ph>
  body（创建，注意**不带 id**）：{"conversationId":"<32hex>","title":"...","type":"tts|asr"}
  resp: {"code":0,"msg":"成功","data":{"id":9655605,"conversationId":"<回显>",...}}
  - 带数字 id = 更新；id:0 或不存在 → {"code":200,"msg":"参数校验失败，错误信息：会话不存在"}
  - 纯 chat 不会创建会话实体，所以语音前必须显式 save
```

### TTS
```
POST /open-apis/tts/v2/generate?xiaomichatbot_ph=<ph>
  {"conversationId":"<会话>","msgId":"<毫秒时间戳字符串>",
   "content":{"messages":[{"role":"user","content":"<指令>"},
                          {"role":"assistant","content":"<正文>"}],
              "audio":{"format":"wav","voice":"冰糖"}},
   "modelConfig":{"modelCode":"mimo-v2.5-tts"}}
  - content.messages **必须两条**，第二条是正文（SPA 会把 ()（）【】「」{} 归一成 []）
  - 未知名 modelCode → {"code":400,"msg":"modelCode cannot be empty"}
  resp: {"code":0,"data":{"taskId":16063236,"status":"generating"}}
GET /open-apis/tts/generateStatus?taskId=<n>&xiaomichatbot_ph=<ph>
  resp: {"code":0,"data":{"status":"success","audioUrl":"https://alsgp0.fds.api.xiaomi.com/...",
                          "audit":{"status":"PASS","blocked":false}}}
  - status: generating|success|failed；audit.status=PROCESSING 表示审核中，继续轮询
  - audioUrl 是公共直链，可直接 GET 取字节（实测 WAV，RIFF 头）
```

### ASR（三步）
```
POST /open-apis/resource/genUploadInfo?xiaomichatbot_ph=<ph>
  {"fileName":"a.wav","fileContentMd5":"<md5hex>"}
  resp: {"code":0,"data":{"uploadUrl":"...","resourceUrl":"...","objectName":"multimedia/<uid>/a.wav"}}
PUT <uploadUrl>  头 Content-Type: application/octet-stream（带 content-md5 时必须与字节一致）→ 200
POST /open-apis/asr/recognize?xiaomichatbot_ph=<ph>
  {"conversationId":"<type=asr 的会话>","msgId":"<毫秒>","audioUrl":"<resourceUrl>",
   "language":"auto|zh|en","modelConfig":{"modelCode":"mimo-v2.5-asr"}}
  resp: {"code":0,"data":{"taskId":16063666,"status":"generating"}}
GET /open-apis/asr/recognizeStatus?taskId=<n>&xiaomichatbot_ph=<ph>
  resp: {"code":0,"data":{"status":"success","text":"你好，这是一次语音合成测试。","audit":{...}}}
```

## 实现要求

### 1. `internal/mimo` 新文件
- `conversation.go`——`EnsureConversation(ctx, cred, kind string) (conversationID string, err error)`：
  按 (account, kind) 缓存会话 id；创建时自造 32-hex conversationId（与 `client.go:283` 同一 mint 实现）；
  遇 `参数校验失败，错误信息：会话不存在` 或 `conversation not exist` 时**重建一次**再重试；
  并发安全（`sync.Mutex`）；不把会话 id 写进 `data/accounts.json`（进程内缓存即可，重启重建）。
- `tts.go`——`Synthesize(ctx, cred, TTSRequest) (*TTSResult, error)`：generate + 轮询
  `generateStatus`（间隔 1s、上限 60s、`audit=PROCESSING` 不放弃），成功后下载 audioUrl 返回字节
  与 MIME；`status=failed` → 明确错误（不要只回 "upstream error"）。
- `resource.go`——`genUploadInfo` + `Upload(ctx, cred, fileName string, data []byte) (resourceURL string, err error)`。
- `asr.go`——`Transcribe(ctx, cred, ASRRequest) (string, error)`：上传 → recognize → 轮询 `recognizeStatus`
  （间隔 1s、上限 120s）→ 返回 `text`。
- 音色目录：`Voices(ctx, cred) ([]Voice, error)` 从 `GET /open-apis/bot/config` 的 `data.ttsVoices[]` 解析
  （字段 `{id,name,description,avatar,previewAudio?}`）；`bot/config` 走 ph，解析要容忍缺字段。
- 所有上游业务错（`code != 0`）都走现有 `UpstreamError`，错误信息里带上 `msg`；轮询超时单独错误类型。

### 2. `internal/api` 新文件
- `audio.go`，注册（沿用 `server.go` 的 mux + `s.auth`）：
  - `POST /v1/audio/speech`：OpenAI 入参 `{model, input, voice, response_format, speed?}`
    - `input` 必填；`voice` 支持 MiMo 音色名（如 `冰糖`）或 id；缺省用账号 `bot/config` 里第一个音色，
      并把 OpenAI 常见名（`alloy`/`echo`/`nova`…）映射到默认音色（在 README 里写明映射策略）
    - `response_format ∈ wav|mp3`（默认 wav）→ 上游 `audio.format`
    - 返回**音频字节** + `Content-Type: audio/wav` / `audio/mpeg`；错误走现有 OpenAI 风格错误体
  - `POST /v1/audio/transcriptions`：multipart（`file`、`model`、`language`、`response_format`）
    - `response_format ∈ json|text|verbose_json`（默认 json）；json → `{"text":"..."}`，
      verbose_json → 至少含 `{text, language, duration?}`（拿不到就省略而不是造假）
    - 文件大小上限（如 25MB）与空文件校验
  - `GET /v1/audio/voices`：`{"data":[{"id","name","description","avatar"}]}`
- 复用现有 request 校验/错误工具（`internal/api/request.go`、`errors.go`）；不要新写一套。

### 3. 测试
- 扩展 `internal/testutil/mock.go`：新增 conversation/save、tts/v2/generate、tts/generateStatus、
  resource/genUploadInfo、PUT 上传、asr/recognize、asr/recognizeStatus、bot/config 的 handler。
  **响应必须是真实形状**（`{"code":0,"data":{...}}` 信封、真实字段名），不得只造测试要读的字段
  （参考现有 mock 的 XSSI 教训）。
- 单测（写进 `internal/mimo/*_test.go`、`internal/api/audio_test.go`），至少覆盖：
  - TTS 成功（含 AUDIO-001 命名）、轮询中间态、`failed`、超时
  - 未知名 modelCode（`modelCode cannot be empty`）→ 明确错误
  - 会话不存在 → 自动重建一次 → 成功（断言上游恰好 2 次 save）
  - ASR 上传 PUT 非 200、recognize 无 taskId、轮询成功
  - `/v1/audio/speech` 缺 input → 400；未知 voice → 400 或默认策略（择一并在 README 写明）
  - `/v1/audio/transcriptions` 空文件 / 超限 → 400；`response_format=text` 返回纯文本（非 JSON）
  - 认证：无 key → 401（复用现有流程）
- 真机用例追加到 `internal/live`（`MIMO2API_LIVE=1` 门控，复用现有已存会话，**不得触发短信/验证码**）：
  - LIVE-TTS-001：合成一句中文 → 断言返回字节 >1KB 且 `RIFF` 头 → 存到 scratch 目录
  - LIVE-ASR-001：把上一句的 wav 送去识别 → 断言返回文本与原文一致（或高相似）
  - 失败/不可用（如 `enableVoice=false`）时以明确 SKIP 理由跳过，不要伪造通过

### 4. 文档与门禁
- ADR：`docs/decisions/0007-audio-via-web-surface-conversations.md`，说明
  ① 为什么 TTS/ASR 必须"先有会话"② 为什么不用桌面面（passToken 按 sid 作用域，见 reverse 文档 §9）
  ③ 音色映射策略 ④ 轮询超时/重建语义。
- 更新 `docs/SDD.md`（新端点、时序）与 `docs/TDD.md`（新用例 ID 表 + 覆盖率），`README.md`（新端点、
  音色映射、OpenAI 兼容差异）。
- 通过 `make fmt vet test test-race cover-check hygiene`（覆盖率不得低于现有线），
  自跑一遍并把输出贴进总结；不要 `git add -A` 提交构建产物。

## 硬约束
- 真凭据只允许出现在 git-ignored 的 `data/`；不得写进代码/文档/测试/日志（`make hygiene` 会查）。
- 不改登录/OTP/跨境同意链路；不动 `data/accounts.json` 结构。
- 不新增第三方依赖（`go.mod` 目前零依赖，保持）。
- Go 1.25；注释与提交信息用英文；错误要能定位（wrap + 上游 msg）。
- 轮询实现必须响应 ctx 取消。

## 交付总结要求
最后打印：新增/修改文件清单、测试用例数、`make` 各门禁的真实输出、真机用例是否跑通（含耗时与字节数）、
以及任何未能验证的事项（明确写"未验证"，不要含糊）。

## Implementation status (updated)

A first draft of the upstream audio client (`internal/mimo/audio.go`, ~600 lines:
conversation creation, `tts/v2/generate` + `generateStatus`, `genUploadInfo` →
PUT → `asr/recognize` → `recognizeStatus`, result download) was written and
compiles, but the API layer and its tests are **not** done. It therefore lives
outside the repository at
`/root/.hermes/cache/scratch/mimo_staged/audio.go` (scratch is pruned after 72h —
move it back in and finish the task book before that).

Why it is parked: adding ~600 untested lines to `internal/mimo` drops that
package's coverage from 83.5% to 65.9%, below the `cover-check` floor (80%,
docs/TDD.md §6.2). Restore it together with its tests, not before.
