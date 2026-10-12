# OxCost 重构 Spec v1.0（R1 轮）

> 目标：从「能跑」到「健壮、统一、可运维」。你点的四个问题（共享编译、
> 插件偷工减料、审查缺失、spec 散装）逐条落账，外加数据库对齐。
> 执行方式：分阶段派 Claude，每阶段独立验收+交叉审查，审查清单白纸黑字。

---

## Part A — 基础设施与真机验证（先做，因为后续阶段都要在它上面跑）

### A1 共享编译缓存 + 清理冗余
- `/root/.cargo/config.toml` 设全局 `target-dir = /root/cargo-target`，
  或装 sccache（按内容去重，跨项目命中更高）——**二选一，先试 sccache**
- 清掉 main 的 4.6G target 与全部残留验证克隆
- 效果度量：重构期间所有并行 worktree 只编译一次依赖

### A2 宿主配置文件实装（--config 桩转正）
- TOML 配置：监听地址/端口、OXCOST_API_KEYS、**按插件 id 的 enabled 覆盖**
  （config > manifest 默认，运行时读取，改配置重启生效——热加载不做，YAGNI）
- 插件开关语义按既定定义：关=对外不存在（路由/目录/探测三断，healthz 仅运维可见）
- 验收：config 覆盖优先级测试（config.enabled=false 覆盖 manifest true，反之亦然）

### A3 真机验证（每个插件一张 live 报告，mock 测试再厚也替代不了）
- **mimo-rust**：Upstash 取一个测试账号 → `MIMO_LIVE_E2E=1` 跑 live parity
  （同请求打 Rust 插件与 Go 127.0.0.1:21223 oracle，比对 status/type/code）；
  **谨慎项**：Go 实例还在用同一批账号——先小流量单账号验证，完整池切换等 cutover
- **simple-chat**：sc-seed 从 Upstash 导入（PC-41 read-only 已验 schema 兼容）
  → live 一轮 reasoning 对话 + 搜索开关对话；**cutover 前提**：停 Go simple-chat
- **tokenharbor**：等浏览器解锁账号（唯一人工依赖，不阻塞其他项）
- **mimo2api (external)**：已真机验证过，回归跑一遍 E2E 脚本即可
- 产出：`docs/LIVE_REPORT.md`——每插件 live 结果、偏差、修复项

### A4 门禁升级（写进 pre-merge 脚本 `scripts/gates.sh`，人工+Claude 都必须过）
- clippy 默认集 `-D warnings`（已有）+ **pedantic 子集**（must_use/unwrap 于
  非测试代码禁用等，允许少量 allow 但须注释理由）
- **cargo-audit**（依赖 CVE）+ **cargo-deny**（license/重复依赖/ban）
- **gitleaks 全历史扫描**（替代手动 grep，规则进 pre-merge）
- fmt + test 不变
- CI 语义：合并 main 前本地必须跑 gates.sh 全绿

---

## Part B — 插件架构统一（核心重构）

### B1 流式升级：ChatOutcome 缓冲 → 真·流式
- `ChatOutcome::Sse(Vec<u8>)` → `BoxStream` 变体（保留缓冲变体兼容非流式）
- 三插件全部改造为增量转发：首 token 延迟从「上游全程完」降到「上游首 chunk」
- SC 报告过 commit 边界问题：流式模式下重试只能发生在首个字节发出前——
  该语义写进 SDK 文档并配测试
- 契约 §5 修订：流式转发语义（pass-through vs re-frame）定稿
- **先在 oxcost-sdk 出 trait v2 设计稿（RFC 文档），三插件作者视角评审后再动代码**

### B2 capability 完整矩阵（不许桩交付）
- `web_search`：SDK trait 加 `web_search` hook（TH 已裁 reserved，SC/MIMO 有
  实现基础）——MIMO 的 citations 透传 + SC 的 standalone `/v1/web_search` 落实
- 每个声明的 capability 必须有真实现 + 契约 §4/§7 验收测试；声明不做的必须
  显式 reserved-unsupported（manifest 里可见，不是注释里）
- 产出：`docs/CAPABILITY_MATRIX.md`——四插件 × 13 能力组，真实现/预留/不支持三态

### B3 插件数据库对齐（你本轮点名）
- 现状：三插件各自发明 SQLite schema（TH accounts 表 / MIMO 池表 / SC Account
  形态+park 字段），字段名、时间格式（RFC3339 vs epoch）、状态机值域都不一致
- 统一：`oxcost-sdk` 出 **storage 规范 crate**（或 trait + 参考实现）：
  - **表结构规范**：accounts(id, credentials..., state, health, lease_expiry,
    parked_until, quarantine, created_at, updated_at)——字段名/类型/时间格式
    （统一 RFC3339 UTC）定死
  - **状态机规范**：状态值域统一（healthy/parked/quarantined/disabled），迁移
    规则与租约语义统一（复用 SC 的 R3 latch 语义做基准——它最严格）
  - **每个插件一个 `oxcost-storage` 适配层**：三插件现有 schema 迁移到统一规范
    （迁移脚本 + 旧库转换测试）
- rusqlite 0.37 定死（本轮已吃过版本分叉的亏），cargo-deny 防再犯
- 验收：三插件的库用同一套 SQL 语义审查清单过一遍

### B4 插件生命周期复核
- 注册→探活→服务→禁用→注销全状态机复查（M1 的 DisabledIsFrozen 语义保持）
- enabled 开关的来源优先级：config > manifest（A2 落地后验证）
- 三插件 × 生命周期状态机的一致性测试矩阵

---

## Part C — 文档与审查重整

### C1 全景 Spec 重写（取代散装任务书）
- `docs/SPEC.md`：系统架构、四形态、契约全文索引、数据流图、运维手册
  （配置/开关/账号/cutover 流程）、三插件文档、已知限制与 roadmap
- 每插件 README 统一模板：功能矩阵、env 契约、cutover 手册、live 验证状态

### C2 交叉审查（防浑水摸鱼的制度保障）
- 每阶段交付 → 第四个 Claude 独立审查（diff 审查+设计质疑），我复核
- 审查清单：契约符合度矩阵、capability 矩阵、parity 清单未勾项、
  clippy/audit/deny/gitleaks 全绿、live 报告真实性抽查
- 审查产出进 `docs/REVIEW_LOG.md`，逐项签收

---

## 执行顺序与派工

| 波次 | 内容 | 前置 |
|---|---|---|
| 1 | A1+A2+A4（基建） + spec 评审 | 无 |
| 2 | A3 真机验证（mimo-rust / sc / mimo2api 回归） | A1 |
| 3 | B3 数据库对齐 | A2（schema 规范入 sdk crate） |
| 4 | B1 流式（RFC 先行）+ B2 capability | B3 |
| 5 | B4 + C1 + C2 全量审查 | 全部 |

每波次：Claude 实现 → 我独立验收 → 交叉审查 → 合并。人工依赖项
（TH 浏览器解锁）不阻塞任何波次。
