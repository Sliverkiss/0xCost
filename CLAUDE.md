# CLAUDE.md

## 项目定位

OxCost 是一个 AI gateway 聚合网关框架：一个入口，聚合多个上游 AI 提供商与模型，
通过负载均衡进行流量分发，主打快速、稳定、可持续服务。

核心能力：

- **多模型聚合** — 统一 API 背后接入多种上游模型
- **负载均衡** — 在多个提供商与渠道之间分发流量

## 技术栈

- **Go**（规划中，项目尚处早期，代码结构待定）
- 新增 Go 代码时遵循标准 Go 项目布局（如 `cmd/`、`internal/`、`pkg/`），
  使用 `gofmt` 标准格式，导出符号需有 doc comment

## 协议

- **AGPL-3.0**（见 [LICENSE](LICENSE)）
- 任何新增代码文件同样受 AGPL-3.0 约束；禁止引入与该协议不兼容的依赖

## 开发约定

- **Commit message**：遵循 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/)，
  如 `feat:`、`fix:`、`docs:`、`refactor:`、`test:`、`chore:`
- **交流语言**：中文（代码注释、issue、PR、review 均优先使用中文）
- 提交前保持变更最小化，一个 commit 聚焦一件事

## 常用命令

项目初始化阶段，暂无构建/测试命令；引入 Go module 后补充 `go build` / `go test` 相关说明。
