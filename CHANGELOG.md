# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增

- Nexus 迁移域地基（FR-21/22/23 部分，见 `docs/specs/0.4.0-migration-foundation.md` 与 `docs/adr/0012-nexus-migration-state-machine.md`）：
  - `0003_migration.sql` 新增 `migration_task` 表；`MigrationTaskRepo` + `domain.MigrationService` 状态机骨架（创建 **planned**、显式 **start**、cancel/resume 守卫、启动时 **running→failed**）。
  - OpenAPI 管理面：`/api/v1/migrations` 列表/创建、`/{id}`、`/start`、`/resume`、`/cancel`、`/report`、`/discover`；仅 admin；凭据仅 `credentialRef`。
  - devmock 内存任务状态机与契约测试对齐；OPERATIONS 补充迁移凭据引用约定。
- Nexus 三来源发现（FR-20 / FR-21 计划预览，见 `docs/specs/0.4.0-migration-discover.md`）：
  - `internal/migration/discover`：`OnlineREST`（Nexus REST 仓库列表 + 有限页资产估算）、`OfflineDir`（夹具布局）、`OfflineBundle`（manifest + content）。
  - `POST /migrations/discover` 同步发现，成功落库 **planned** 返回 `taskId`+`plan`；失败不落库；不自动 start。
- Nexus 迁移异步执行（FR-21/22，见 `docs/specs/0.4.0-migration-execute.md`）：
  - `internal/migration/runner`：planned→start 后台搬运、冲突 skip/overwrite/fail、checkpoint 断点、协作 cancel、启动 running→failed。
  - offline_bundle/offline_dir 流式 Put；wiring 注入 Runner。
- Nexus 迁移报告与切换增量（FR-23，见 `docs/specs/0.4.0-migration-report-cutover.md`）：
  - `GET .../report` 组装 totals/failures/cutover checklist；`POST .../finalize` 仅 completed，差量复制并写 `report.delta`。
- 管理端迁移向导（见 `docs/specs/0.4.0-migration-web-wizard.md`）：
  - 侧栏「迁移」入口（admin）；列表 / 向导（discover→planned→显式 start）/ 详情（轮询、取消、续传、finalize、报告与 cutover 清单）。
- 迁移真机可用性增强：
  - `online_rest` 执行：按 plan 枚举 Nexus assets 并流式下载 `downloadUrl` 写入 hosted。
  - `sourceConfig.includeRepositories`：发现/执行仅处理名单内仓库（避免全量占盘）。
  - 仓库删除级联清理 asset 元数据（复测前可删仓重建）；补充删除单测。

### 变更

- 暂无。

### 修复

- 暂无。

### 移除

- 暂无。

## [0.3.0] - 2026-07-23

### 新增

- Raw hosted 协议纵切贯通（FR-13 部分 / FR-18，见 `docs/specs/0.3.0-raw-hosted.md` 与 `docs/adr/0009-protocol-auth-and-blob-layout.md`）：
  - `internal/blobstore` 文件系统内容寻址存储：sha256 两级分片布局 `<root>/ab/cd/<hash>`，临时文件 + 边写边算哈希 + 原子 rename 落盘，相同内容天然去重（`config` 补建 `blob/tmp` 目录）。
  - `0002_asset.sql` 迁移新增 `asset` 表（`UNIQUE(repository_id, path)` + 外键级联）与 `AssetRepo`（`Upsert`/`GetByPath`/`DeleteByPath`）；`domain.AssetService` 编排「校验 raw-hosted → 流式入 blob → upsert 元数据」的发布 / 拉取 / 删除。
  - `internal/protocol` Raw handler + router：`GET|HEAD|PUT|DELETE /repository/{repo}/{path...}`，GET 回写 `Content-Type`/`Content-Length`/`ETag`(=blob sha256)；错误映射 401/403/404/409；经 `httpserver.WithProtocolRoutes` 注册于契约路由之后、SPA 回退之前。
  - 鉴权：auth 中间件补 `Authorization: Basic` 解析（token 作 password，为空则 username），仅接受 `jat_` API Token（不启用口令登录），Bearer 行为不变；协议端点复用 `CanAccess`（public read 匿名放行）。
- 仓库配置基座：proxy/group 契约扩展与校验（FR-13 proxy/group 前置，见 `docs/specs/0.3.0-repository-config.md`）：
  - proxy/group 配置复用 `repository` 表既有 `config` JSON 列（`{"remoteUrl":...,"members":[...]}`），不新增迁移；`repository` 新增 `RepositoryConfig` 结构与 `DecodeConfig`/`EncodeRepositoryConfig` 编解码，`RepoRepo.Create` 增 config 入参、`GetByName`/`List` 读回 config、新增 `UpdateConfig`。
  - `domain.RepositoryService.Create`/`Update` 改为结构化配置签名并新增 `validateConfig`：hosted 两字段须空、proxy 必填合法 http/https `remoteUrl`、group 必填 `members`（成员均存在、同 format、禁自引用），违规返回新增语义 `ErrValidation`（经 `writeDomainErr` 映射 400）。
  - `api/openapi.yaml` 的 `Repository`/`CreateRepositoryRequest`/`UpdateRepositoryRequest` 增可选 `remoteUrl`/`members`，`oapi-codegen` 与 `openapi-typescript` 重生成；devmock store/handlers 透传字段并补 npm-proxy 种子 `remoteUrl`；web 仓库创建表单据 `type` 条件渲染 remoteUrl（proxy）与 members 多选（group）+ i18n 文案。
- Raw proxy/group 回源与聚合读（FR-13 proxy/group 全 / FR-17 / FR-18 复验，见 `docs/specs/0.3.0-raw-proxy-group.md` 与 `docs/adr/0010-proxy-cache-singleflight-group-routing.md`）：
  - 新增 `internal/upstream` 回源客户端：`Client.Fetch(ctx, baseURL, path)` 流式返回上游内容 + 响应头，上游 404→`ErrNotFound`、非 2xx→`StatusError`、支持 `IsTimeout` 超时判定；超时可配（`JIAN_UPSTREAM_TIMEOUT` 秒，默认 30s），`config`/wiring 装配注入 `AssetService`。
  - `domain.AssetService` 新增 `Resolve(ctx, repoName, path)` 按 `repo.Type` 分派读路径：hosted 读本地；proxy 命中即返回、未命中经 upstream 回源**流式**落 blob（不整体入内存）并 upsert 缓存后返回；group 按 `members` 有序递归解析、首个命中即返回、全未命中→404（深度上限遏制环引用）。以 `golang.org/x/sync/singleflight`（键 `repoID\x00path`）收敛并发首次回源，同一未命中路径只回源一次。
  - 新增领域错误 `ErrUpstream`（→502）、`ErrUpstreamTimeout`（→504）；`protocol/raw.go` 的 `GET/HEAD` 改用 `Resolve`（支持 hosted/proxy/group 读），`writeAssetErr` 补 `upstream_error`/`upstream_timeout` 映射；`PUT`/`DELETE` 仍仅 hosted（proxy/group 写→409）。proxy 暂不做缓存失效/TTL、GC 仍不即时。
- Maven 全类型 hosted/proxy/group（FR-14，见 `docs/specs/0.3.0-maven.md`）：
  - `/repository/:repo/*artifactPath` 端点改由单一路由 + `protocol.Dispatcher` 按仓库 `format` 分派（`maven`→`MavenHandler`，其余→`RawHandler`）；`RegisterRoutes` 签名泛化为 `artifactHandler` 接口。制品字节的发布/拉取/删除与鉴权复用既有内容寻址存储与 `AssetService.Resolve`（hosted 本地 / proxy 回源缓存 / group 有序聚合），`MavenHandler` 经 Go 嵌入 `RawHandler` 仅覆盖 GET。
  - Maven 语义补两点：校验和文件 `.md5/.sha1/.sha256` 缺失时据去后缀的底层制品字节现算摘要以 `text/plain` 返回（已部署则原样优先命中）；group 的 `maven-metadata.xml` 按 `members` 有序合并各成员 `versions`（去重）并重算 `latest`/`release`/`lastUpdated`，全成员皆无→404。
  - `domain.AssetService.Put` 校验由 raw-hosted 放宽为 **hosted-only**（格式路由上移至协议层）；无新增表/迁移。SNAPSHOT 唯一时间戳版本按路径原样存取，group metadata 版本顺序采成员出现顺序而非语义化比较（边界见 spec）。
- npm registry 全类型 hosted/proxy/group（FR-15，见 `docs/specs/0.3.0-npm.md` 与 `docs/adr/0011-npm-registry-layout-and-tarball-rewrite.md`）：
  - 新增 `internal/protocol/npm.go` + `RegisterNpmRoutes`：registry 基址 `<server>/npm/:repo/`，单 catch-all `/:repo/*rest` 在 handler 内解析 packument（`GET <pkg>`）/tarball（`GET <pkg>/-/<file>`）/publish（`PUT <pkg>`），支持 scoped `@scope/name`；与 `/api/v1`、`/repository`、SPA 回退互不冲突。`NpmHandler` 经 Go 嵌入 `RawHandler` 复用鉴权与制品存取。
  - 复用内容寻址 blob + `asset` 表（无新增表）：packument 整份文档存于路径 `<pkg>`、tarball 存于 `<pkg>/-/<file>`。publish 解码 `_attachments` base64 落各 tarball、与已存 packument 合并（versions/dist-tags/time 逐键 last-writer-wins）后覆盖写；install 经 `AssetService.Resolve` 拉取。
  - 服务端统一把 packument 各版本 `dist.tarball` 重写为 `<请求基址>/npm/<本仓>/<pkg>/-/<原文件名>`（依 `X-Forwarded-Proto`/`Host`）：proxy 回源上游 packument/tarball 并缓存、group 合并成员 packument（versions 并集、dist-tags 首成员优先）并经有序命中回落 tarball，客户端始终经本仓拉取。publish 仍仅 hosted（proxy/group 写→409）。
- 制品浏览与使用说明（FR-16，见 `docs/specs/0.3.0-browse-usage.md`）：
  - 新增只读管理面端点 `GET /api/v1/repositories/{name}/assets`（分页 + 可选 `prefix` 前缀过滤，返回 `AssetList{items:[AssetSummary{path,size,hash,contentType?,updatedAt}],total}`，按 path 升序）与 `GET /api/v1/repositories/{name}/usage`（据 format/type 返回 `UsageInfo{format,type,snippets:[UsageSnippet{title,description?,code}]}`）；均经 `requireRepoRead` 授权（admin/`CanAccess(read)`，public 匿名可读），未认证 401、越权 403、仓库不存在 404。
  - `AssetRepo` 增 `ListByRepo`/`CountByRepo`（SQLite `LIKE ... ESCAPE '\'` 前缀过滤，转义 `%`/`_`/`\`）；`RepositoryService` 增 `ListAssets`/`Usage`，`buildUsage` 据 format/type 组装 maven/npm/raw 客户端接入片段（writable=hosted 才含写入片段），对外基址由 `X-Forwarded-Proto`/`Host` 推断（maven/raw→`<base>/repository/<name>`、npm→`<base>/npm/<name>/`）。
  - `api/openapi.yaml` 增两端点与 `AssetSummary`/`AssetList`/`UsageInfo`/`UsageSnippet` schema 及 `prefix` 参数，`oapi-codegen` 与 `openapi-typescript` 重生成；devmock store 补 `assets` 种子与 `listAssets`/`usage`（`buildUsage` 与后端一致）、msw 增两 handler、契约一致性测试覆盖新 schema。
  - `apps/web` 新增仓库详情页 `RepositoryDetailPage`（路由 `/repositories/:name`）：制品浏览表（路径/大小/哈希/更新时间 + 前缀过滤）与使用说明卡片（`CopyButton` 可复制），仓库列表页增「浏览」入口 + i18n `repoDetail` 文案。
- 原生客户端真机验收脚手架（FR-19，见 `scripts/e2e-smoke.mjs`）：跨平台 Node 冒烟脚本（`node scripts/e2e-smoke.mjs`，仅依赖 Node 内置 fetch）扩 0.3.0 全链路 roundtrip——Raw hosted 发布/拉取、制品浏览 + 使用片段（含 prefix 过滤与权限）、Maven hosted deploy/resolve（含缺失校验和现算）、npm publish/install（含 packument tarball 重写与字节一致）；原生 `mvn`/`npm` 客户端按可用性自动探测提示，proxy/group 外网回源经 `--include-proxy` 可选开启。发版前已完成 curl/mvn/npm 原生客户端真机互通验收。

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
