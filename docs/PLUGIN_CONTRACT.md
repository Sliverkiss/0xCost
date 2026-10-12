# OxCost 插件契约（PLUGIN_CONTRACT）

> OxCost 是 2api 网关的**插件宿主框架**：每个 `*-2api` 项目（把某站点的免费 AI
> 服务转成 OpenAI 兼容 API 的网关）作为一个插件接入，宿主统一对外提供
> **OpenAI 协议**入口。
>
> 本文档是宿主与插件之间的**完整契约**：接入（manifest）、运行（sidecar 协议）、
> 行为（验收清单）。插件无论用什么语言实现，满足本契约即可接入。
>
> 方法论吸收并致谢：
> [2api-Template](https://github.com/lza6/2api-Template)（MIT，不变量/伪流式/错误映射）、
> [dsh-our-free-model](https://github.com/Ebony-Vinyl/dsh-our-free-model)（manifest 清单与
> apiVersion 协商模式）、
> [Chat2API](https://github.com/xiaoY233/Chat2API)（负载均衡策略 / 模型映射 /
> 配置式自定义 provider 的机制参考）。

---

## 1. 设计原则

1. **宿主薄、契约厚**：宿主只做注册、路由、聚合、鉴权、进程管理；所有上游差异
   封死在插件里。契约是唯一接口，宿主不感知任何插件内部实现。
2. **Sidecar 第一公民**：插件是独立进程，宿主经 localhost HTTP 与其通信。
   语言无关、崩溃隔离、存量 2api 项目（如 mimo2api）零改造接入。
3. **单一协议**：全线只对齐 **OpenAI 协议**（`/v1/chat/completions`）。
   宿主对外、宿主对插件、插件对上游适配，都是同一套协议语义，不做协议转换层。
4. **零明文密钥**：插件密钥经环境变量或插件私有配置注入；manifest 只允许
   引用（`env:VAR_NAME`），不允许出现任何字面量密钥。
5. **目录真实**：插件声明的模型 id 必须来自实测，宿主不合并、不纠正、不编造。
6. **可验收**：每条契约都是可验证的行为，不是风格偏好。

## 2. 总体架构

```
                 客户端（OpenAI SDK / 各类兼容客户端）
                    │  单一入口，单一 key，OpenAI 协议
                    ▼
┌───────────────────────────────────────────────────────────┐
│  OxCost 宿主（Rust，单二进制，axum + tokio）                 │
│                                                           │
│  protocol 层   OpenAI chat completions / SSE（通用层）       │
│  core 层       注册表 · 模型映射 · 路由 · 负载均衡 · 熔断 · 限流│
│  contract 层   manifest 解析 / 校验 / apiVersion 协商        │
│  sidecar 层    进程管理（spawn/健康/重启）+ HTTP 客户端       │
└───────┬───────────────────┬───────────────────┬───────────┘
        │ localhost HTTP     │                   │
        ▼                   ▼                   ▼
┌──────────────┐    ┌──────────────┐    ┌──────────────┐
│ 插件 A        │    │ 插件 B        │    │ 插件 C        │
│ (Go/Rust/JS) │    │ (任意语言)    │    │ (external)   │
│ mimo2api     │    │ xxx-2api     │    │ 已独立部署的   │
└──────────────┘    └──────────────┘    └──────────────┘
```

Rust workspace 划分：

```
crates/
├── oxcost-protocol/   OpenAI 协议编解码与 SSE 流
├── oxcost-contract/   manifest 模型、解析、校验、版本协商
├── oxcost-core/       注册表、模型映射、路由、负载均衡、熔断、限流
└── oxcost/            bin：axum 装配、配置加载、sidecar 进程管理
```

文件 < 800 行；`cargo clippy -- -D warnings` 零告警；测试离线全绿（mock 上游，
真实上游仅发布前冒烟）。

## 3. 接入契约 —— plugin.toml

每个插件根目录提供 `plugin.toml`（TOML，Rust 生态惯例）：

```toml
manifestVersion = "0.1"          # 本契约版本，必填
id = "dev.oxcost.mimo2api"       # 反向 DNS，全局唯一，必填
name = "mimo2api"                # 展示名，必填
version = "1.0.0"                # 插件语义化版本，必填
apiVersion = "v1"                # 声明兼容的宿主运行契约版本，必填
license = "MIT"                  # 必填

[entry]                          # 运行方式，三选一
mode = "host-spawn"              # host-spawn = 宿主拉起并托管
                                 # external  = 本地已部署的 sidecar（标准运行契约）
                                 # remote    = 远程 OpenAI 兼容端点（零代码接入）
cmd = ["./mimo2api", "serve"]    # mode=host-spawn 时必填；external 时忽略
port = 21223                     # external 模式：sidecar 本地端口
# url = "https://relay.example.com/v1"   # remote 模式必填：上游 OpenAI 兼容 base

[health]
path = "/healthz"                # 健康检查端点（host-spawn / external 必填；remote 免检）
interval_secs = 30               # 检查间隔
timeout_secs = 5

[routing]
enabled = true                   # 可选，默认 true；false = 注册但停用（见 §6 开关语义）
models = [                       # 声明提供的模型（必须实测真实存在；
                                 #   「插件名/模型名」前缀 = name = id 尾段）
  "mimo2api/mimo-v2.6-pro",
  "mimo2api/mimo-v2.6-flash",
]
unknown_model = "pass"           # pass=未声明模型也透传上游 | fallback="mimo2api/mimo-v2.6-pro" | reject

[capabilities]                   # 插件提供的能力组（缺省只有 chat；分层见 §4）
chat = true                      # /v1/chat/completions（默认能力）
# embeddings = true              # /v1/embeddings
# images = true                  # /v1/images/generations（及 edits/variations）
# audio_speech = true            # /v1/audio/speech（TTS）
# audio_transcription = true     # /v1/audio/transcriptions（STT，multipart）
# audio_translations = true      # /v1/audio/translations（译为英文，multipart）
# video = true                   # /v1/videos（异步任务：提交→轮询→取结果，Sora 模式）
# moderations = true             # /v1/moderations（内容安全分类）
# responses = true               # /v1/responses（实验性：OpenAI 旗舰接口，含托管工具）
# web_search = true              # chat 内搜索（核心）：web_search_options / web_search_status
                                 #   / 模型名 :online 后缀 三种开关；结果以 OpenAI 标准
                                 #   annotations(url_citation) + 扩展 citations 字段回传
# files = true                   # /v1/files CRUD（实验性；batches 的前置）
# batches = true                 # /v1/batches（实验性：异步批量，依赖 files）
# realtime = true                # /v1/realtime/sessions + WebSocket（实验性：语音实时）

[auth]                           # external/remote 必填；host-spawn 可整体省略
keys = "env:MIMO2API_API_KEYS"   # 密钥引用。只允许 env: 前缀；禁止任何字面量密钥；
                                 #   host-spawn 模式由宿主自动生成注入（见 §4 鉴权）

[limits]                         # 可选；不填则继承宿主默认
max_inflight = 8                 # 单插件并发上限
cooldown_secs = 60               # 连续失败后的熔断冷却

[source]                         # 可选但强烈建议（供应链可追溯）
repository = "https://github.com/Sliverkiss/mimo2api"
```

**校验规则**（宿主加载时强制，不满足拒绝注册）：
- `id` 全局唯一，格式 `^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)+$`
- `manifestVersion` 与 `apiVersion` 在宿主支持列表内（宿主启动时打印支持矩阵）
- `routing.models` 非空且无重复；插件名前缀必须与 `id` 尾段一致
- `auth.keys` 出现时必须以 `env:` 开头；清单中出现疑似密钥字面量（`sk-` 等）直接拒绝；
  `host-spawn` 模式可整体省略 `[auth]`（宿主自动生成密钥注入）
- `entry.mode = external`：宿主不管理进程生命周期，仅做健康检查与路由
- `entry.mode = remote`：`entry.url` 必填（http/https）；免 `/healthz`，宿主以
  轻量 chat 探测代替健康检查
- `entry.mode = remote` 的**零代码接入**：任何已在运行的 OpenAI 兼容服务
  （免费中转站、其他 2api 项目）不写一行代码即可成为插件——只填清单，宿主直接
  转发；其行为契约按上游实际表现验收（允许豁免「错误映射归档」一条，但目录
  真实性验收不可豁免）

## 4. 运行契约 —— sidecar 接口

插件作为 sidecar 进程，必须在 `127.0.0.1:<port>` 上提供（**全部为 OpenAI 协议**；
`remote` 模式无此节要求，上游即契约）：

| 端点 | 要求 |
|---|---|
| `POST /v1/chat/completions` | 【chat·核心】`stream: true/false` 均支持；SSE 帧符合 OpenAI 规范 |
| `POST /v1/chat/completions`（search） | 【web_search·核心】同端点的能力增强：接受 `web_search_options` / `web_search_status`(disabled\|auto\|enabled) / 模型名 `:online` 后缀；命中搜索的结果以消息 `annotations`(url_citation) + `citations` 数组回传 |
| `POST /v1/embeddings` | 【embeddings·核心】OpenAI 规范 |
| `POST /v1/images/generations` 等 | 【images·核心】OpenAI 规范（generations/edits/variations 按需实现） |
| `POST /v1/audio/speech` | 【audio_speech·核心】TTS，返回音频流 |
| `POST /v1/audio/transcriptions` | 【audio_transcription·核心】STT，multipart 上传 |
| `POST /v1/audio/translations` | 【audio_translations·核心】音频译为英文，multipart 上传 |
| `POST /v1/videos` + `GET /v1/videos/{id}` | 【video·核心】异步任务：提交返回任务 id，轮询状态，完成后取结果（Sora 模式） |
| `POST /v1/moderations` | 【moderations·核心】内容安全分类，OpenAI 规范 |
| `POST /v1/responses` | 【responses·实验性】OpenAI Responses API（含 web_search/file_search 等托管工具与引用标注；参考 openresponses.org 互操作规范） |
| `POST /v1/files` 等 | 【files·实验性】文件 CRUD（目的域：batches/大文件 STT） |
| `POST /v1/batches` + `GET /v1/batches/{id}` | 【batches·实验性】异步批量任务，依赖 files 能力 |
| `POST /v1/realtime/sessions` | 【realtime·实验性】语音实时会话（WebSocket 透传） |
| 模型目录 | 返回与 `plugin.toml` 声明一致的真实目录，且为宿主的**运行时真相源**（宿主按 TTL 动态拉取聚合，见 §5 动态模型目录）；目录变化时宿主缓存自动跟进 |
| `GET /healthz` | `200 {"status":"ok"}` 为健康 |

**能力分层与裁剪规则**：能力分**核心**（稳定 OpenAI 标准，契约完整承诺）与
**实验性**（接口仍在演进，仅要求「透传语义 + 目录真实 + 透传上游错误」，
不承诺行为兼容；`[experimental]` 能力可能随契约大版本调整）。`[capabilities]`
未声明的能力，对应端点宿主**不路由**（返回 `404`），插件可不实现；声明了就
必须实现该能力组全部验收项。多模态请求（如 chat 里的 `image_url` 内容分片）
不属于独立能力，仍归 `chat`。`batches` 依赖 `files`，宿主校验依赖关系。

**错误语义**（宿主依赖此映射对外统一报错，错误体一律 OpenAI 格式
`{"error": {"message": ..., "type": ..., "code": ...}}`，插件不得透传上游哨兵文本）：

| 上游情况 | 插件响应 |
|---|---|
| 额度/登录墙哨兵（含 HTTP 200 但文本命中哨兵特征） | `429`，type `rate_limit_error` |
| 上游空响应 / 非 2xx | `502`，type `upstream_error` |
| 空 `messages` / 非法参数 | `400`，type `invalid_request` |
| 流式中途出错 | 以尾帧表达（`finish_reason` + `[DONE]`），不裸断连 |

**伪流式**：上游不支持流式时，插件切块定时下发 SSE，模拟打字机（首帧延迟 =
上游响应时间）。上游支持流式则必须真流式。

**鉴权（双层归一）**：
- **客户端 → 宿主**：单一 key（宿主配置），对外永远只有这一把，插件 key 池
  对客户端不可见。
- **宿主 → 插件**：用**插件自己的密钥**调插件，不透传客户端 key（客户端 key
  是宿主层的，与插件域无关）。密钥来源按形态分：
  - `host-spawn`：宿主**自动生成**随机密钥，spawn 时经 env 注入插件进程——
    清单可整体省略 `[auth]`，运营零手工；
  - `external` / `remote`：清单 `env:` 引用解析注入（已是独立部署，key 在其
    自有配置中）。
- 插件侧：校验 `Authorization: Bearer`；未配置 key 时仅接受来自 `127.0.0.1`
  的请求（本地开发零配置）。

## 5. 宿主对客户端的承诺（统一外部接口）

| 项 | 承诺 |
|---|---|
| 入口 | 单一地址、单一 key，OpenAI 协议：`POST /v1/chat/completions`、`GET /v1/models`、`GET /healthz` |
| 模型映射 | 宿主支持请求模型名的**别名/通配符映射**（`*`、`前缀*`、`*后缀`），可绑定目标插件；映射发生在路由解析之前，映射后的名字按下一行规则路由 |
| 模型路由 | `插件名/模型名` 精确路由；`/v1/models` 聚合全部已注册插件的健康目录 |
| 动态模型目录 | 插件的 `/v1/models` 是**运行时真相源**，manifest.models 仅为**声明基线**：宿主启动拉取一次，之后按 TTL（默认 300s，可配，带 jitter）刷新并聚合缓存；刷新失败→继续用上次缓存（stale-while-error），基线中缺失的模型需连续 2 个周期缺失才摘除，基线外新增模型默认信任；目录变更产生事件供路由/负载均衡层消费（M3） |
| 能力路由 | 宿主按 `/v1/<能力>/...` 端点族转发到声明了该能力的插件（核心+实验性能力同规则）；同一能力多插件时同样适用负载均衡与熔断；无人声明时 `404` |
| 负载均衡 | 同一模型被多个插件声明时分发，策略三选一：**round-robin**（轮询）/ **fill-first**（填满优先）/ **failover**（优先首选插件，故障才转移）；支持请求级粘性（preferred 插件）；故障自动摘除 |
| 未知模型 | 宿主按目标插件的 `unknown_model` 策略处理；无匹配插件时 `404 model_not_found` |
| 熔断 | **插件级**：连续失败达阈值进入冷却，期间该插件模型从目录隐藏，冷却后半开探测 |
| 鉴权 | 宿主强制校验外部 key；插件侧 key 池对外不可见 |
| 日志 | 不泄露任何上游/下游密钥 |

## 6. 生命周期与故障

```
注册 → 健康探测 → Serving ⇄ 熔断/半开 → 移除
                 └──────→ Disabled（人工停用，可再启用）
```

- **启用/停用（人工运营开关，与熔断正交）**：`routing.enabled = false` 或运行时
  切换（管理 API / 配置热载）：
  - **停用**：插件进入 `Disabled`——立即从路由与 `/v1/models` 目录移除、停止
    健康探测；host-spawn 插件同时停掉进程（释放资源），external/remote 仅停止
    转发、不触碰对方进程/服务
  - **启用**：回到 注册→探测 流程，健康后恢复 Serving
  - **与熔断的区别**：熔断是自动故障处理，冷却结束自动半开恢复；开关是人工
    运营决策（源不稳/配额耗尽/维护中），**不会自动恢复**——关了就是关了，
    直到再次启用
- **host-spawn**：宿主负责拉起、健康循环、崩溃重启（退避上限可配）、优雅退出
  （SIGTERM 宽限期默认 10s）。插件进程退出即视为故障，走熔断流程。
- **external**：宿主只做健康探测与路由；探测失败即摘除，恢复即回归。
- **remote**：宿主只做探测（轻量 chat 探测）与转发；失败即摘除，恢复即回归。
- **账号池边界**：插件内部的**多账号池、账号级失败计数与账号冷却**
  （如 mimo2api 的账号轮询）属于插件内部事务，宿主契约不感知、不管理；
  宿主只维护**插件级**熔断。两层互不越界：插件不依赖宿主重试语义，
  宿主不假设插件内部有账号池。插件私有状态（账号池、会话缓存等）由插件自选
  存储引擎（文件 / SQLite / Redis 等均可），不进宿主进程、不入 git；存储凭据
  只经环境变量注入。
- **升级**：同 `id` 新 `version` 注册 = 替换；路由中的在途请求走完旧实例。
- **移除**：清单中删除即注销；宿主目录立即同步。

## 7. 行为契约 —— 插件验收清单

插件合并进 `plugins/` 或注册到宿主前逐条验收（来源：2api-Template INVARIANTS，
按 OpenAI 单协议 sidecar 场景裁剪）：

- [ ] `plugin.toml` 通过宿主校验，`auth.keys` 为 env 引用
- [ ] 声明的每个能力组端点按 §4 语义可用（核心能力完整验收，实验性能力按实验标准），`GET /v1/models`、`/healthz` 齐全；未声明的端点不实现
- [ ] 模型目录实测真实，附 E2E 报告（哪些模型真可用/失败原因）
- [ ] 契约归档：插件目录内 `PROTOCOL.md` 含上游端点/头/请求体/响应结构与证据
- [ ] 错误映射符合第 4 节表格；限流检测基于响应文本（上游 200+哨兵很常见）
- [ ] 鉴权行为符合第 4 节；CORS 默认关闭
- [ ] 测试离线全绿：流式/非流式、错误映射（429/502/400）、鉴权、未知模型（mock 上游）
- [ ] 无硬编码密钥；日志不泄露密钥
- [ ] 宿主侧：路由/模型映射/负载均衡三策略/熔断行为有集成测试（mock sidecar 与 mock remote 上游）

## 8. 版本化与演进

- `manifestVersion` / `apiVersion` 语义：**主版本不兼容、次版本向后兼容**。
  宿主支持多版本并存，对不支持的清单拒绝注册并说明原因。
- 协议演进保持**加法**：未来如需其他入口协议（如 Anthropic 格式），以宿主侧
  独立转换模块追加，插件契约不变——所有形态的插件永远只写 OpenAI 协议。
- **形态路线图**：`host-sdk`（进程内 Rust 插件，M3 实现）→ 动态加载
  （`dylib`/WASM 沙箱，按需评估）→ 插件市场/分发机制（远期）。每步都以
  「契约接口不变」为前提。
- 本契约变更走 PR；对存量插件的破坏性变更必须 bump 主版本并给迁移指南。
- 实验性字段放 `[experimental]` 表内，随时可能移除，不进兼容承诺。

## 9. 第一个插件：mimo2api

- 形态：`entry.mode = "external"`（现有 docker compose 部署原样保留）
- 已满足项：`POST /v1/chat/completions`（流式/非流式）、账号池（插件内部事务，
  宿主无感）、`/healthz`、Upstash 存储、环境变量密钥
- 待补项：`plugin.toml`、错误映射对齐第 4 节、契约文档 `PROTOCOL.md`
- **不重写**：语言无关契约的 design intent——成熟网关零改造接入；Rust 用于
  新写插件（可复用 `oxcost-protocol` crate），不用于重写存量

## 10. 插件形态与演进路线（性能完美形态）

契约接口对四种插件形态**完全一致**，插件可跨形态迁移：

| 形态 | 进程 | 开销 | 适用 |
|---|---|---|---|
| `host-sdk` | **编译进宿主**（一等公民） | 零进程、零跳、最优性能 | 开源 Rust 插件，性能主力 |
| `host-spawn` | 宿主拉起的 sidecar | ~10-20MB RSS/个；localhost 一跳 ~0.2ms | 任意语言插件 |
| `external` | 独立部署的 sidecar | 同上（宿主不管理进程） | 存量 2api（mimo2api） |
| `remote` | 无进程，纯转发 | 零本地开销 | 外部 OpenAI 兼容端点 |

- **开源即架构**：宿主本体 AGPL-3.0 开源；host-sdk 插件编译进宿主、随宿主同
  AGPL——法律零摩擦。进程内形态是**性能主力**（零进程、零跳、零序列化边界）。
- **host-sdk 实现时机**：M3 立起 SDK trait 层（插件注册/能力声明/请求处理接
  口），不再等首个真实 Rust 插件；性能优先。
- **sidecar 的定位**：隐私与异构的后卫——敏感插件（逆向契约、账号运营）走
  sidecar/external 进程隔离，隐私边界（§11）不受 host-sdk 开源影响（AGPL 只
  约束编译进宿主的部分）；任意语言插件与存量 2api 也走此形态。
- **形态选择 = 分插件而治**：开源且性能敏感走 host-sdk；敏感/异构走
  sidecar；存量走 external；外部端点走 remote。

## 11. 插件仓库与隐私边界

1. **宿主仓库保持纯净**：公开宿主仓库只含框架代码与合成示例 fixture
   （`plugins/example-*`），不含任何真实插件实现。
2. **插件独立仓库**：每个插件有自己的仓库；涉及逆向契约、账号池运营细节的
   插件使用**私有仓库**，经 git submodule 挂到 `plugins/<name>/`，或本地检出
   后以 `--plugins <dir>` 指向加载。`[source].repository` 记录出处。
3. **sidecar = 许可证隔离**：进程边界使插件构成独立作品，宿主 AGPL 不传染；
   私有插件合法合规。
4. **永不入库清单**（对一切仓库生效）：上游抓包证据、账号凭据、cookies、
   账号池数据、`.env`。插件私有状态只存本地（见第 6 节账号池边界）。
5. **诚实边界**：私有仓库防内容泄漏，不防仓库本身误公开；宿主公开文档描述
   通用方法论（如 2api-Template 公开内容），被保护的是具体上游契约与运营细节。
