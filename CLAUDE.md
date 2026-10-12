# CLAUDE.md

## 项目定位

OxCost 是一个 AI gateway 聚合网关框架：一个入口，聚合多个上游 AI 提供商与模型，
通过负载均衡进行流量分发，主打快速、稳定、可持续服务。

核心能力：

- **多模型聚合** — 统一 API 背后接入多种上游模型
- **负载均衡** — 在多个提供商与渠道之间分发流量

## 技术栈

- **Rust**（stable，axum + tokio，workspace 多 crate）
- 常用命令：`cargo build` / `cargo test` / `cargo clippy -- -D warnings` / `cargo fmt --check`
- 文件 < 800 行；测试离线全绿（mock 上游，真实上游仅发布前冒烟）

## 架构一句话

插件宿主框架：每个 `*-2api` 项目作为 sidecar 插件接入，宿主统一对外提供
OpenAI 协议入口。完整契约见 [docs/PLUGIN_CONTRACT.md](docs/PLUGIN_CONTRACT.md)。

## 运行环境

- **纯本地开发**：代码与提交只在本机 `/root/oxcost`，不推 GitHub 远程
- 构建前 `source ~/.cargo/env`

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
