# M20 任务书：MiMo Claw 控制面（状态 / 额度 / 领取）

对应 `docs/web-surface-reverse.md` §8。**本任务只做控制面**；agent 数据面（`wss://<host>/ws/proxy`
的握手与方法调用）需要"领取当日额度"后真机抓包，协议细节尚未捕获，**明确不在本任务范围**
（等 M20b，另行出任务书）。

## 目标

让运维/客户端能查看并领取 Claw 每日额度，且不误报：

- `GET /v1/admin/claw/status` → 聚合 `mimo-claw/status` + `quotaUsage` + `tokenPlanUsage`
- `POST /v1/admin/claw/claim` → 调 `mimo-claw/create` 领取当日额度（默认关闭，需显式开关/确认）
- `GET /v1/admin/claw/ticket` → 取 `/open-apis/user/ws/ticket`（为后续 M20b 准备；未领取时返回
  上游的 `Mimo Claw资源当前不可用` 语义，映射成明确的 409/503 + 可读 reason，不要 500）

## 上游（已实测）

```
GET  /open-apis/user/mimo-claw/status?xiaomichatbot_ph=<ph>
     → {"code":0,"data":{"status":"NOT_CREATED|...","requestResourceType":"FREE",
                          "resource":{...},"billing":{"edition":"FREE","tokenPlan":{"status":"NONE"}}}}
POST /open-apis/user/mimo-claw/create?xiaomichatbot_ph=<ph>     // 领取当日 3 小时
GET  /open-apis/user/mimo-claw/quotaUsage?xiaomichatbot_ph=<ph>  → {"exists":false,"maxBytes":0,...}
GET  /open-apis/user/mimo-claw/tokenPlanUsage?xiaomichatbot_ph=<ph> → {"status":"NONE"}
GET  /open-apis/user/ws/ticket?xiaomichatbot_ph=<ph>
     → 未领取：400 "Mimo Claw资源当前不可用"
```

## 实现要求

1. `internal/mimo/claw.go`：`ClawStatus`、`ClawQuota`、`ClawClaim`、`ClawTicket`，
   全部复用现有 `Client` 传输层（ph 参数 + 指纹头 + `UpstreamError` 分类）。
   上游字段**原样保留**在结构体里（`resource`/`billing` 用 `json.RawMessage` 或宽松 map），
   避免因为上游加字段而解析失败。
2. `internal/api/claw_admin.go`：三个端点，沿用 `s.auth` 与现有错误体/请求校验工具。
   - `claim` 必须是 **POST + 显式开关**（配置项 `MIMO2API_CLAW_ALLOW_CLAIM`，默认 false；
     关闭时返回 403 并说明原因），因为领取会消耗账号当日 3 小时额度。
   - `claim` 成功后**重新拉一次 status** 并在响应里返回新状态（不要让调用方自己再猜）。
   - 上游 `Mimo Claw资源当前不可用` → 409/503 + `reason: "claw_not_claimed"`（或等价明确码）。
   - 不在日志里打印 cookie/passToken。
3. 按账号选择：多账号时可指定 `account_id`（沿用现有 admin 接口的账号寻址方式）；缺省账号池首个可用账号。
4. 测试（mock 需真实形状）：
   - CLAW-001 status 三字段聚合正确（含上游多出字段不炸）
   - CLAW-002 claim 开关关闭 → 403 且**不产生上游调用**（断言 mock 计数为 0）
   - CLAW-003 claim 成功 → 触发一次 status 刷新（断言上游调用序列）
   - CLAW-004 ticket 未领取 → 409/503 + 明确 reason（不是 500）
   - CLAW-005 未授权请求 → 401
5. 真机（`MIMO2API_LIVE=1`）：
   - LIVE-CLAW-001：只读 status/quota/ticket，断言与当前账号真实状态一致（本账号现在应为未领取）。
     **不得**在 live 套件里调用 claim（会消耗额度）——claim 只允许人工/运维显式触发。
6. 文档：`docs/SDD.md`/`docs/TDD.md`/`README.md` 增补；ADR
   `0009-claw-claim-is-operator-gated.md`（为什么 claim 默认关闭、为什么 live 套件不领取）。

## 硬约束

同 M18/M19：真凭据只在 git-ignored `data/`；零新依赖；不改登录链；
`make fmt vet test test-race cover-check hygiene` 全绿并贴真实输出；提交信息英文。
