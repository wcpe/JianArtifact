# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增

- 初始化 SDD 项目脚手架：PRD / ARCHITECTURE / API / ROADMAP / OPERATIONS / SECURITY / 决策记录（ADR）。
- 防漂移治理规则（`.claude/rules/`）、演进与维护指南、版本与许可（MIT）、工程化配置。
- monorepo 工程骨架（pnpm workspace + Turborepo + Makefile + Go Task + go.work）与部署编排骨架（`deploy/`）。
- 0.1.0 工程基座落地（见 `docs/specs/0.1.0-foundation.md`）：
  - 后端单二进制：`api/openapi.yaml` 设计优先 + oapi-codegen 生成接口/类型；Gin `/healthz`、`/readyz` 返回 `HealthStatus`；`//go:embed` 内嵌前端产物 + SPA 回退；`CGO_ENABLED=0` 静态编译；`healthcheck` 子命令供容器探活。
  - 前端可构建工作区：React + Vite + TS strict 的 `apps/web`；`packages/ui` 设计令牌；`packages/devmock` 以 openapi-typescript 生成类型 + ajv 运行时校验实现 devmock↔OpenAPI 契约一致性（mock 漂移即测试失败）。
  - 统一质量门 `scripts/check.sh`：前端 format/lint/typecheck/test/build + 后端 gofmt/vet/golangci-lint/`go test -race`/govulncheck/build + 契约一致性。
  - 部署骨架：多阶段 `Dockerfile`（基础镜像经 `ARG` 参数化，默认 distroless nonroot）可离线构建；容器 `/readyz` 探活通过。

### 变更

- Go 工具链升级至 `go1.26.5`；`apps/wiki` 暂移出 pnpm 工作区（延后至 0.2.0）。

### 修复

- 修复依赖与标准库漏洞（升级 `quic-go`、`golang.org/x/net` 及 Go 工具链），`govulncheck` 零告警。
- 修复 `.gitignore`：全局 `dist/` 规则会连带排除后端 embed 占位目录，导致 `//go:embed all:dist` 在纯净检出时无法编译；改为先重新纳入目录再放行占位 `index.html`。

### 移除

- 暂无。

> 发版时把"未发布版本"段切成 `## [X.Y.Z] - YYYY-MM-DD`，再新建空的"未发布版本"段。
