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
> apiVersion 协商模式）。

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
│  core 层       注册表 · 模型路由 · 负载均衡 · 熔断 · 限流      │
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
├── oxcost-core/       注册表、路由、负载均衡、熔断、限流
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

[entry]                          # 运行方式，二选一
mode = "host-spawn"              # host-spawn = 宿主拉起并托管；external = 已独立部署
cmd = ["./mimo2api", "serve"]    # mode=host-spawn 时必填；external 时忽略
port = 21223                     # 插件监听端口（host-spawn 可省略，宿主动态分配）

[health]
path = "/healthz"                # 健康检查端点，必填
interval_secs = 30               # 检查间隔
timeout_secs = 5

[routing]
models = [                       # 声明提供的模型（必须实测真实存在）
  "mimo/mimo-v2.6-pro",
  "mimo/mimo-v2.6-flash",
]
unknown_model = "pass"           # pass=未声明模型也透传上游 | fallback="mimo/mimo-v2.6-pro" | reject

[auth]
keys = "env:MIMO2API_API_KEYS"   # 密钥引用。只允许 env: 前缀；禁止任何字面量密钥

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
- `auth.keys` 必须以 `env:` 开头；清单中出现疑似密钥字面量（`sk-` 等）直接拒绝
- `entry.mode = external` 时宿主不管理进程生命周期，仅做健康检查与路由

## 4. 运行契约 —— sidecar 接口

插件作为 sidecar 进程，必须在 `127.0.0.1:<port>` 上提供（**全部为 OpenAI 协议**）：

| 端点 | 要求 |
|---|---|
| `POST /v1/chat/completions` | `stream: true/false` 均支持；SSE 帧符合 OpenAI 规范 |
| `GET /v1/models` | 返回与 `plugin.toml` 声明一致的真实目录 |
| `GET /healthz` | `200 {"status":"ok"}` 为健康 |

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

**鉴权**：插件校验宿主转发的 `Authorization: Bearer`；未配置 key 时仅接受来自
`127.0.0.1` 的请求（本地开发零配置）。

## 5. 宿主对客户端的承诺（统一外部接口）

| 项 | 承诺 |
|---|---|
| 入口 | 单一地址、单一 key，OpenAI 协议：`POST /v1/chat/completions`、`GET /v1/models`、`GET /healthz` |
| 模型路由 | `插件名/模型名` 精确路由；`/v1/models` 聚合全部已注册插件的健康目录 |
| 负载均衡 | 同一模型被多个插件声明时轮询/权重分发，故障自动摘除 |
| 未知模型 | 宿主按目标插件的 `unknown_model` 策略处理；无匹配插件时 `404 model_not_found` |
| 熔断 | 插件连续失败达阈值进入冷却，期间该插件模型从目录隐藏，冷却后半开探测 |
| 鉴权 | 宿主强制校验外部 key；插件侧 key 池对外不可见 |
| 日志 | 不泄露任何上游/下游密钥 |

## 6. 生命周期与故障

```
注册 → 健康探测 → Serving ⇄ 熔断/半开 → 移除
```

- **host-spawn**：宿主负责拉起、健康循环、崩溃重启（退避上限可配）、优雅退出
  （SIGTERM 宽限期默认 10s）。插件进程退出即视为故障，走熔断流程。
- **external**：宿主只做健康探测与路由；探测失败即摘除，恢复即回归。
- **升级**：同 `id` 新 `version` 注册 = 替换；路由中的在途请求走完旧实例。
- **移除**：清单中删除即注销；宿主目录立即同步。

## 7. 行为契约 —— 插件验收清单

插件合并进 `plugins/` 或注册到宿主前逐条验收（来源：2api-Template INVARIANTS，
按 OpenAI 单协议 sidecar 场景裁剪）：

- [ ] `plugin.toml` 通过宿主校验，`auth.keys` 为 env 引用
- [ ] `POST /v1/chat/completions` 流式/非流式均可用，`GET /v1/models`、`/healthz` 齐全
- [ ] 模型目录实测真实，附 E2E 报告（哪些模型真可用/失败原因）
- [ ] 契约归档：插件目录内 `PROTOCOL.md` 含上游端点/头/请求体/响应结构与证据
- [ ] 错误映射符合第 4 节表格；限流检测基于响应文本（上游 200+哨兵很常见）
- [ ] 鉴权行为符合第 4 节；CORS 默认关闭
- [ ] 测试离线全绿：流式/非流式、错误映射（429/502/400）、鉴权、未知模型（mock 上游）
- [ ] 无硬编码密钥；日志不泄露密钥
- [ ] 宿主侧：路由/聚合/熔断行为有集成测试（mock sidecar）

## 8. 版本化与演进

- `manifestVersion` / `apiVersion` 语义：**主版本不兼容、次版本向后兼容**。
  宿主支持多版本并存，对不支持的清单拒绝注册并说明原因。
- 协议演进保持**加法**：未来如需其他入口协议（如 Anthropic 格式），以宿主侧
  独立转换模块追加，sidecar 契约不变——插件永远只写 OpenAI 协议。
- 本契约变更走 PR；对存量插件的破坏性变更必须 bump 主版本并给迁移指南。
- 实验性字段放 `[experimental]` 表内，随时可能移除，不进兼容承诺。

## 9. 第一个插件：mimo2api

- 形态：`entry.mode = "external"`（现有 docker compose 部署原样保留）
- 已满足项：`POST /v1/chat/completions`（流式/非流式）、账号池、`/healthz`、
  Upstash 存储、环境变量密钥
- 待补项：`plugin.toml`、错误映射对齐第 4 节、契约文档 `PROTOCOL.md`
