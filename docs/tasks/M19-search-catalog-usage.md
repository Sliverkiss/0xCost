# M19 任务书：联网检索引用透出 + 真实模型目录 + 用量细节

前置：M18（语音面）已落地并验收。本文档对应 `docs/web-surface-reverse.md` §5/§6/§7。

## 目标

1. **联网检索结果不再被丢弃**：把上游 `doc` / `web_search` 事件解析出来，作为**引用**回传给客户端
   （流式与非流式都要），并让客户端能开关检索。
2. **`/v1/models` 反映真实目录**：用上游 `bot/config` 覆盖静态表，带能力位（是否支持检索/思考/TTS/ASR）。
3. **用量细节**：把 `nativeUsage` 的 `cached_tokens` / `reasoning_tokens` 透出。

## 1. 检索开关（入参）

- 新可选字段（`/v1/chat/completions` 请求体）：`"web_search_status": "disabled" | "auto" | "enabled"`，
  默认 **disabled**（保持现状，不额外增加时延/费用）。
- 兼容 OpenRouter 风格模型后缀：`model: "mimo-v2.6-pro:online"` ≡ `web_search_status: "enabled"`；
  后缀解析必须不影响 `/v1/models` 里的真实 id（id 不含后缀）。
- 映射：`disabled→disabled`、`auto→auto`、`enabled→enabled`（上游枚举见 §5），字段落在
  `modelConfig.webSearchStatus`。
- **注意**：实测表明上游即便 `disabled` 也可能返回检索事件；因此解析/透出逻辑**不能**以请求值做前提，
  一律"有事件就透出"。

## 2. 引用透出（出参）

上游事件（已实测）：
```
event:doc        data:{"content":"webSearch"}                       // 检索开始
event:web_search data:[{"id":"...","text":"<网页正文>","metadata":"{}",
                        "name":"<标题>","url":"<原文>","displayUrl":"www.x.com",
                        "snippet":"<摘要>","usedContent":"False"}]
```
实现要求：

- 内部结构：`Citation{Index int; Title string; URL string; DisplayURL string; Snippet string; Used bool}`
  （`Used` 来自 `usedContent == "True"`；`text` 正文默认**不透出**，只在 debug 日志按需截断记录，
  因为它可达数十 KB，会毁掉上下文预算 —— 若未来要透出必须由显式开关控制）。
- **非流式**：在 `choices[0].message.annotations` 输出
  `[{"type":"url_citation","url_citation":{"url","title","snippet","start_index"?,"end_index"?}}]`；
  拿不到位置就省略下标字段，不要编造。另在顶层响应加 `"citations":[...]`（本项目扩展，便于客户端直取）。
- **流式**：在 `[DONE]` 之前追加**一个**扩展 chunk：
  `data: {"id":...,"object":"chat.completion.chunk","choices":[],"citations":[...]}`，
  仅在确有引用时发；文档里写明这是本网关扩展，标准客户端会忽略 `choices:[]` 的块
  （实现时确认现有流的 `choices` 处理不会崩）。
- `include_citations: false`（可选，默认 true）用于关闭透出，避免不想要的 payload 膨胀。
- 解析器要容错：`data` 非法 JSON、字段缺失、`web_search` 是空数组 → 记日志并跳过，不得中断流。

## 3. `/v1/models` 真实目录

> **状态：已实施且真机验证（2026-10-09）**。目录按账号并集 + 静态兜底，`capabilities`
> 依据真机 `modelConfigList`（`modelConfigListNg` 实测**无** `features`）；引用面：
> 非流式 13 条、流式 chunk 11 条真机跑通（详细边界见 ADR 0007 与
> `docs/web-surface-reverse.md` §5）。踩坑：`usedContent` 是布尔、`metadata` 是对象，
> 写错类型会毁掉整个数组——解析已改为逐字段容错。

- 上游：`GET /open-apis/bot/config?xiaomichatbot_ph=<ph>`（§7），解析
  `data.modelConfigListNg[]`（24 个模型）+ `data.ttsVoices[]` + `data.ext.*`。
- 缓存：**按账号**缓存（TTL 默认 10 分钟，可配），失败时回退到现有静态表并记 warning
  （不得因为上游抖动让 `/v1/models` 500）。
- 输出：OpenAI 形状 `{"object":"list","data":[{"id","object":"model","created","owned_by":...}]}`，
  并追加本项目扩展字段（文档写明）：`"capabilities":{"web_search":0|1|2,"thinking":bool,"audio":"tts|asr|none"}`、
  `"page_type"`（页面类型）、`"tasks":["chat"|"tts"|"asr"]`。
- 多账号：按 id 取并集（同一 id 能力取最大值），`owned_by` 用 `xiaomi-mimo`。
- `/v1/models/{id}` 同步使用同一来源；未知 id → 现有 404 行为。
- 现有静态表的去留：**保留作为回退**，但与上游结果不一致时以上游为准（文档里写明）。

## 4. 用量细节

- 从 `usage.nativeUsage.prompt_tokens_details.cached_tokens` / `completion_tokens_details.reasoning_tokens`
  映射到 OpenAI 形状：`usage.prompt_tokens_details.cached_tokens`、
  `usage.completion_tokens_details.reasoning_tokens`（缺失就省略该子对象，不要填 0 假装有）。
- 流式最后一个 usage chunk 与非流式响应都要带。

## 5. 测试

- mock 扩展：`web_search` / `doc` 事件（真实形状，含 12 条、含 `usedContent` 字符串布尔、
  含一条超长 `text`）、`bot/config` 响应（真实字段名）。
- 单测（命名 `WS-*` / `CAT-*` / `USAGE-*`）至少覆盖：
  - WS-001 非流式带引用 → `annotations` + `citations` 正确（3 条，含 Used 判定）
  - WS-002 流式：引用 chunk 出现在 `[DONE]` 之前且只出现一次
  - WS-003 `include_citations:false` → 不出现
  - WS-004 `web_search_status` 三值正确映射；`model:xxx:online` 后缀等价（且不污染 id）
  - WS-005 畸形 `web_search` data（非法 JSON / 字段缺失 / 空数组）→ 流不中断、无引用、不 panic
  - CAT-001 上游目录成功 → 数量/能力位正确；CAT-002 上游失败 → 回退静态表且 `/v1/models` 200
  - CAT-003 多账号并集去重
  - USAGE-001 有 nativeUsage → 两个 details 正确；USAGE-002 无 → 字段省略
- 真机（`internal/live`，`MIMO2API_LIVE=1`，复用已存会话，不触发短信）：
  - LIVE-WS-001：提一个必然检索的问题（如"今天有什么科技新闻"）→ 断言引用条数 > 0 且 url 以 http 开头
  - LIVE-CAT-001：`bot/config` 真机解析 → 断言模型数 ≥ 20 与至少 1 个 TTS 模型
  - 任一失败以明确 SKIP 理由记录，不伪造。

## 6. 文档与门禁

- ADR `0008-citations-shape-and-search-switch.md`：① 为什么默认 disabled ② 为什么用 `annotations` +
  扩展 `citations` ③ 为什么默认不透出网页正文 ④ `:online` 后缀约定。
- 更新 `docs/SDD.md`（事件解析、模型目录缓存）、`docs/TDD.md`（新用例 ID 与覆盖率）、`README.md`
  （`web_search_status`、`:online`、`citations`、`include_citations`、`/v1/models` 扩展字段）。
- `make fmt vet test test-race cover-check hygiene` 全绿并贴真实输出；覆盖率不低于现有线。

## 硬约束

与 M18 相同：真凭据只在 git-ignored `data/`；零新依赖；不改登录链；错误要可定位；轮询/缓存要响应 ctx；
不得 `git add -A` 提交构建产物；提交信息英文。

## Status so far (2026-10-09)

- **`/v1/models` from the real catalog: DONE.** The endpoint merges the
  account's live `bot/config` catalog (`modelConfigListNg`, chat rows) with the
  static floor and falls back to the static list when the upstream read fails
  (MODELS-LIVE-001/002). Live: 15 ids.
- **`nativeUsage` passthrough (`cached_tokens` / `reasoning_tokens`): DONE**
  (already emitted by the adapter; confirmed on live responses).
- **Web-search citations (annotations/citations, stream + non-stream): NOT
  STARTED.** Upstream really returns the events (`web_search` with ≤12 results,
  fields `text/name/url/displayUrl/snippet/usedContent`), but the adapter still
  drops them; this is parsing/policy work, deliberately after the endpoint
  plumbing.
- **`web_search_status` switch + `:online` suffix: NOT STARTED** (needs the same
  plumbing; note the Studio only enables search when the request carries no
  attachments — replicate that rule).
