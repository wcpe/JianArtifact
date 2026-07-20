# 架构设计：JianArtifact

> 系统当前真貌（HOW）。始终原地更新到现状；结构 / 机制变了就改它。决策的"为什么"见 `docs/adr/`。

## 1. 定位与边界

JianArtifact 是一个**自托管、单二进制交付的多格式制品仓库**。它：

- **是**：托管（hosted）+ 代理（proxy）+ 聚合（group）制品的服务，含管理端、原生协议端点与 Nexus 迁移能力。
- **不是**：CI/CD 平台、代码托管、制品构建器、公共 SaaS。

外部边界：

- **北向**：原生包客户端（curl/mvn/npm…）走各格式协议端点；管理员 / CI 走管理 REST API 与内嵌 web 管理端。
- **南向**：proxy 仓库回源到远程上游（Maven Central、npmjs 等）；迁移域读取 Nexus OSS（REST / 原生目录 / 离线包）。
- **落地**：元数据落 SQLite，制品内容落文件系统 blob 存储。默认无外部依赖。

## 2. 模块与依赖

单 monorepo，前后端分置，共享契约。

```
JianArtifact/
├─ apps/
│  ├─ server/   Go 后端（Gin + sqlx + embed）
│  ├─ web/      React 管理端（Mantine 7 + i18next）
│  └─ wiki/     UI 组件 / 业务模式验收站
├─ packages/
│  ├─ ui/                共享 Mantine 组件库、主题、设计令牌
│  ├─ devmock/           浏览器 + Node 双端 Mock、场景包
│  ├─ eslint-config/     前端共享严格 ESLint
│  └─ typescript-config/ 前端共享严格 TS
├─ api/        OpenAPI 契约真源（openapi.yaml）+ 生成配置
├─ deploy/     Dockerfile / compose / .env.example / 部署脚本 / helm
├─ docs/       PRD / ARCHITECTURE / API / ROADMAP / ADR / specs / OPERATIONS
└─ scripts/    质量门与容器化开发入口
```

### 2.1 后端分层（`apps/server/internal/`，依赖单向向下）

```
api          HTTP 路由、中间件（鉴权 / 限流 / 日志）、管理端点
  ↓
protocol     Raw / Maven / npm 协议适配（请求解析、元数据、客户端语义）
  ↓
domain       仓库、制品、迁移等领域逻辑（编排能力域）
  ↓
repository · storage · migration · auth   能力域
  ↓
persistence（SQLite 访问）  +  blob 存储（文件系统，内容寻址）
config       横切，供各层读取，不反向依赖业务层
web          go:embed 前端 dist（由构建注入）
```

**依赖规则（不变量，见 `.claude/rules/architecture-invariants.md`）**：上层依赖下层，禁止反向穿透；`protocol` / `api` 不得直连 `persistence`；无循环依赖。

### 2.2 前端依赖

`packages/ui`（真源）→ 被 `apps/web`、`apps/wiki` 消费；`packages/ui` 不反向依赖 `apps/*`。`packages/devmock` 据 `api/openapi.yaml` 生成契约，供 web/wiki 离线开发与契约比对。共享 `eslint-config` / `typescript-config` 供各前端工程继承。

## 3. 数据模型

**元数据真源 = SQLite**（`modernc.org/sqlite`，纯 Go 无 CGO），经 sqlx 访问，开启 WAL、`foreign_keys=ON`、`busy_timeout`；schema 由 `internal/persistence/migrations/*.sql` 迁移脚本管理，内置极简迁移器按文件名字典序前向执行，`schema_migrations` 表记录已应用版本。0.2.0 已落地的表：

- **user**：账号（唯一）、argon2id 口令哈希、角色（admin/user）、状态（active/disabled）、创建 / 更新时间。明文口令不落库。
- **api_token**：仅存 sha256 摘要（唯一）、所属用户、名称、创建 / 吊销时间；明文令牌仅签发时返回一次，不入库。
- **revoked_token**：登出会话 JWT 的 `jti` 与过期时间，直至过期前拒绝复用（无状态 JWT 的短期黑名单）。
- **repository**：名称（唯一）、格式（raw/maven/npm）、类型（hosted/proxy/group）、可见性（public/private）、配置（JSON 文本，上游 URL / 成员列表等）、创建 / 更新时间。
- **acl**：主体（用户）× 仓库 × 动作（read/write/admin），唯一约束去重；admin 蕴含 read/write，write 蕴含 read。

后续版本引入：

- **artifact / component / asset**：制品坐标、路径、大小、内容哈希（指向 blob）、所属仓库、时间戳。
- **migration_task**：迁移任务状态机、来源、进度、断点、冲突策略、报告。

**blob 内容真源 = 文件系统**：按内容哈希（如 sha256）分片目录寻址；元数据 asset 记录哈希引用。一致性约束：元数据事务提交成功后 blob 才对外可见（见 §5）。

## 4. 接口

- **管理 REST API**：用户 / 令牌 / 仓库 / ACL / 迁移任务的 CRUD 与操作，契约见 `docs/API.md` 概览、`api/openapi.yaml` 为唯一真源。
- **协议端点**：Raw / Maven / npm 各按其原生协议暴露路径（GET 拉取、PUT/POST 发布、元数据端点）；不进 OpenAPI 契约（由各格式规范定义），但受同一鉴权 / ACL 中间件保护。
- **健康端点**：`/healthz`（存活）、`/readyz`（就绪，含 SQLite 与 blob 目录自检）。
- **前端 client**：据 `api/openapi.yaml` 生成，与 devmock 比对防漂移。

## 5. 关键机制

- **契约优先生成**：`api/openapi.yaml`（唯一真源）→ `oapi-codegen` 生成 Go server 接口与类型；前端生成 client；devmock 据同一契约比对。改契约 → 重生成 → 契约测试守护。
- **单飞合并（single-flight）**：同一制品并发回源合并为一次上游拉取，其余等待者复用结果；上游失败时等待者一致失败并可回退缓存。
- **流式传输**：上传 / 下载 / 回源全程 `io.Reader` 流式，边收边校验哈希边落盘，不整体入内存。
- **内容寻址与校验**：blob 按哈希存储，读取时可校验；写入 → 校验和匹配 → 落盘 → 元数据事务提交 → 对外可见。
- **代理回源与缓存**：proxy 按需回源、缓存 blob 与元数据；上游超时 / 5xx / 断连时重试与降级，不因单上游阻塞整体（M3 引入断路器 FR-43）。
- **迁移状态机**：迁移任务持久化于 SQLite，支持中断续传、幂等、冲突策略；异步执行、进度可查、产出报告。
- **鉴权**：JWT(HS256) 会话 + API Token；中间件统一校验，ACL 在后端判定（前端不替代授权）。
- **配置**：环境变量 / 配置文件注入；凭据引用名 → 环境变量注入，不入库不进日志。

## 6. 部署

- **默认形态**：单静态二进制（`CGO_ENABLED=0`，前端 dist 经 `go:embed` 内嵌），或单容器。数据 = SQLite 文件 + blob 目录，用命名卷 / 主机目录持久化。
- **容器**：多阶段 Dockerfile（node 构建前端 → golang 编译内嵌 → distroless/static 运行，非 root，`HEALTHCHECK` 打 `/readyz`）。
- **编排**：docker-compose（单服务 + 命名卷 + `.env`）；远程部署脚本（SSH：Docker/Compose 或 rootless systemd 二进制，原子切换 + 回滚）；Helm/K8s（M3 交付，单实例 RWO 卷 + `Recreate`）。
- **拓扑**：默认单进程单实例；无外部中间件。M5 才评估多节点 / 外部存储（需 ADR）。
- 运维细节见 `docs/OPERATIONS.md`，部署交付物见 `deploy/`。

## 7. 关键裁决与不做项

重大取舍见对应 ADR：

- **ADR-0001**：后端选 Go（替代旧 Rust 方案）。
- **ADR-0002**：元数据用纯 Go SQLite（`modernc.org/sqlite`）+ 文件系统 blob，非外部 DB / 对象存储；与 NORA「文件系统即真源、无数据库」路线不同（本项目取 SQLite 元数据真源，向前追加迁移）。
- **ADR-0003**：前端沿用旧项目 UI 栈 Mantine 7 + i18next。
- **ADR-0004**：API 设计优先，手写 OpenAPI + oapi-codegen。
- **ADR-0005**：单二进制交付，前端经 Go embed 内嵌。
- **ADR-0006**：monorepo 工作区——前端 pnpm + Turborepo + Makefile，后端 Go Task。
- **ADR-0007**：部署编排（Docker/Compose 主路径 + Helm/K8s + rootless systemd 可选）。

**当前不做**：外部数据库 / 对象存储默认后端、消息队列、分布式协调、多节点 HA、CGO 依赖——均属后续期可选方案，须先立 ADR 再引入。
