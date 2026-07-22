# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增

- 暂无。

### 变更

- 暂无。

### 修复

- 暂无。

### 移除

- 暂无。

## [0.2.0] - 2026-07-22

### 新增

- 0.2.0 认证授权与管理端骨架（后端核心，见 `docs/specs/0.2.0-auth-core.md`、`0.2.0-user-management.md`、`0.2.0-repository-acl.md` 与 `docs/adr/0008-auth-session-model.md`）：
  - 持久化底座：`internal/config` 配置加载 + `internal/persistence` sqlx 连接、内置迁移器与 `0001_init.sql`（user/api_token/revoked_token/repository/acl 建表）；`/readyz` 注入 SQLite ping 与 blob 目录可写自检。
  - 认证授权（FR-06/07/08）：argon2id 口令哈希、无状态 JWT HS256 会话（登出经 `revoked_token` 短期黑名单）、API Token（仅存 sha256 摘要，明文仅签发时返回一次）；`Bearer` 统一鉴权中间件（先 JWT 后 Token 摘要）。
  - 管理员自举改为网页端点 `POST /api/v1/auth/bootstrap`（仅 `user` 表为空时开放，去除环境变量令牌，已初始化恒 409）。
  - 用户管理（FR-09）：用户 CRUD 与口令修改 / 重置（管理员改任意、普通用户仅改自己）。
  - 仓库与 ACL（FR-10）：仓库 CRUD、可见性、`/repositories/{name}/acl` 读写与 ACL 授权判定接入鉴权（越权 403、未认证 401）。
  - 新增 `/api/v1/status` 运行时状态端点；CLI 扩为子命令分发器，新增 `admin reset`（离线重置 / 创建管理员）与 `status`（在线探测 / 离线静态输出）。
  - `api/openapi.yaml` 扩展至 `/api/v1` 管理面全端点与 schema（含嵌套 `Error` 信封），`oapi-codegen` 重生成接口；devmock 覆盖新端点，`openapi-typescript` 重生 `schema.gen.ts`，契约一致性测试保持绿。
- 0.2.0 管理端 Web（FR-11，对 devmock 完成，见 `docs/specs/0.2.0-web-admin.md` 与 `docs/adr/0003-frontend-stack.md`）：
  - `apps/web` 全页面：登录/自举（据 `/api/v1/status` 自动切换）、仪表盘、用户、访问令牌、仓库、仓库 ACL；React 18 + Mantine 7 + i18next（中文）+ react-router v6。
  - typed API client（`Bearer` 注入 + `ApiError` 归一化）+ 鉴权上下文（登录/自举/登出、路由守卫、会话与用户快照持久化）+ `useAsync`/`AsyncBoundary` 统一加载/错误/越权态。
  - `packages/ui` 迁入 Mantine 7：`AppProvider`（MantineProvider + 主题）、`PageHeader`、加载/空/错误/越权四类状态态组件与设计令牌拆分。
  - `packages/devmock` 新增 MSW 双端拦截：内存态 CRUD `store` + `msw.ts` handlers（`*/` 前缀通配 origin）+ 浏览器 `worker`（仅 dev 启动）与 Node `server`（集成测试），并导出 `./schema` 子路径供 web 类型级复用；MSW 钉 `2.7.6`。
  - 测试：devmock 契约/MSW 12 + `packages/ui` 组件 6 + `apps/web` 集成 9（testing-library + MSW Node server）全绿；`vite build` 产 dist 供单二进制内嵌，生产构建不含 MSW/devmock。
- 0.2.0 组件 / 业务模式验收站（FR-12，见 `docs/specs/0.2.0-wiki.md`）：
  - `apps/wiki` 重建为 React 18 + Mantine 7 独立静态验收站：`AppShell` 画廊 + 展台注册表，四类展台——设计令牌、页头 `PageHeader`、四类状态态（加载/空/错误/越权）、关键交互（`useForm` 表单校验 + `notifications` 全局通知 + `@mantine/modals` 危险操作确认弹窗，与管理端删除流程同源）。
  - 复用 `packages/ui` 的 `AppProvider` 与组件、并与管理端一致挂载 `ModalsProvider`，核验共享主题/组件/确认弹窗在验收站与管理端同源无重复实现；脱离后端运行，不内嵌进后端二进制。
  - 测试：`apps/wiki` 组件/交互 7（Gallery 3 + sections 4）全绿；`vite build` 产独立 dist。
- 0.2.0 管理端 Web 视觉系统对齐旧项目控制台外壳（FR-11 细化，对齐 `docs/adr/0003-frontend-stack.md`）：
  - `apps/web` 外壳重写为 `AppShell layout="alt"` 可折叠分段侧栏（品牌 logo + 版本号置顶、浏览/管理分段、收起态仅图标 + Tooltip、footer 折叠按钮），导航仅接线 0.2.0 已有页面。
  - 新增密度令牌 `theme/density.ts` 与 `global.css`（`scrollbar-gutter: stable`）；登录卡与仪表盘 KPI 卡样式对齐旧项目。
  - 配色对齐旧项目：`packages/ui` 主题主色改为 Mantine 原生蓝、品牌蓝 `#228be6` 仅用于 logo、`AppProvider` 色彩模式改为 `auto`（跟随系统深浅色）。
  - 新增依赖 `@tabler/icons-react`（外壳图标）。

### 变更

- `apps/wiki`（组件 / 业务模式验收站）随 0.2.0 引入 Mantine 7 组件后重新纳入 pnpm 工作区。
- 依赖新增：`modernc.org/sqlite`、`github.com/jmoiron/sqlx`、`github.com/golang-jwt/jwt/v5`、`golang.org/x/term`；`golang.org/x/crypto`（argon2）转 direct。
- 统一错误响应为嵌套信封 `{"error":{"code","message"}}`；`/readyz` 503 体与 OpenAPI `Error` schema 同步调整。

## [0.1.0] - 2026-07-20

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
