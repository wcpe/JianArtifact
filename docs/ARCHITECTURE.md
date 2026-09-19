# 架构设计：JianArtifact

> 本文是当前代码的架构真源（HOW）。它区分已落地实现和发布状态：当前发布版本由根目录 `VERSION` 标记（`0.8.0`），当前开发窗口是 `0.8.1`（控制台布局口径收敛、观测页性能、移动端响应式、时区与 i18n 落地）。**实时复制通道已随 FR-138 整体退役**，当前形态是单实例独立部署 + 一致性备份包搬迁。

## 1. 定位与边界

JianArtifact 是自托管、单二进制交付的多格式制品仓库，提供：

- Raw、Maven、npm、Docker/OCI、Cargo、PyPI、Go modules、NuGet 等制品托管、代理和聚合；
- 管理端、原生协议端点、用户/仓库/ACL 和 API Token；
- Nexus OSS 在线 REST、离线目录和离线包迁移；
- SQLite 元数据、文件系统 blob、单实例部署，以及以一致性备份包为单位的备份与搬迁。

它不是 CI/CD 平台、代码托管平台、制品构建器或公共 SaaS。

## 2. 当前运行形态

每个实例都是一个独立的 Go 进程或容器，前端静态资源由 Go embed 内嵌。实例本地持有自己的 SQLite 和 blob 目录，不共享数据库、文件卷或进程内状态。

```text
原生客户端 / CI / 管理员
          │
          ▼
HTTP(S) 入口（可选 CDN / 反向代理）
          │
          ▼
JianArtifact 单进程
   ├── 管理 REST + 内嵌 Web
   ├── Raw/Maven/npm/OCI/Cargo/PyPI/Go/NuGet 协议
   ├── SQLite 元数据
   └── 文件系统内容寻址 blob
```

一个实例就是一个完整的数据边界：所有业务写入都落在本地 SQLite 与 blob 目录，不存在节点角色、复制通道或跨实例共享状态。实例之间的数据流动只有一种形式——**一致性备份包的导出与导入**（见 §6），不做实时复制。

## 3. 模块与依赖方向

后端位于 `apps/server/internal/`，依赖单向向下：

```text
api（HTTP handler / 中间件）
  → protocol（Raw/Maven/npm/OCI/Cargo/PyPI/Go/NuGet）
  → domain（仓库、制品、原子操作与 blob 回收、备份与搬迁、迁移、观测）
  → repository / storage / migration / auth / upstream
  → persistence（SQLite） + blobstore（文件系统） + archive（备份包格式，纯叶子层）

config 为横切配置层；web 是 go:embed 的前端静态资源。
```

`api` 和 `protocol` 不直接访问 SQLite；鉴权和 ACL 在后端完成，前端只负责展示和交互。前端依赖方向为 `packages/ui` → `apps/web` / `apps/wiki`，不得反向依赖应用。

技术栈固定为 Go、Gin、sqlx、纯 Go SQLite、React、TypeScript、Vite、Mantine 和 i18next；API 设计真源为 `api/openapi.yaml`。

### 3.1 前端范式（0.8.1 收敛）

- **页面骨架**：固定视口高度由 `app/PageShell` 唯一提供（`100dvh − 页眉偏移 − 2×padding`），页面不再自造 `vh` / `dvh` / `max-width`；内容区宽度取 `theme/density.contentMaxWidth`。
- **KPI 口径**：`components/ops/OpsKit` 的 `OpsKpiBand` 是唯一 KPI 组件（含 `variant="strip"` 紧凑横带与 `actions` 插槽），不在页面内自造指标卡。
- **加载与错误**：`AsyncBoundary` 承担首载骨架与失败重试；`RouteErrorBoundary` 按路由路径重建，避免一次 chunk 加载失败污染后续页面；导航项在 hover / 聚焦时预取目标页 chunk。
- **刷新**：刷新入口唯一（页眉），页面通过全局刷新事件订阅，不各自放刷新按钮。
- **时间口径**：后端一律存 UTC（SQLite `datetime('now')` 为 naive `YYYY-MM-DD HH:MM:SS`，Go `time.Time` 为 RFC3339）。前端展示统一走 `lib/timeFormat`（`parseUtc` / `formatUtcToLocal[Date]`）按**浏览器本地时区**渲染；唯一例外是服务端渲染的 HTML 目录索引页（见 §3.2）。
- **语言与格式**：语言策略收敛在 `i18n/language.ts`（解析优先级：用户显式选择 > 浏览器语言 > 路由默认；公开页跟浏览器语言，管理页与 `/setup` 默认中文），`i18n/useLanguage` 负责同步 i18next 与 `<html lang>`；`i18n/current.ts` 的 `currentLocaleTag()` / `localizedFormatter()` 是 `toLocaleString`、`Intl.*` 与 Mantine 日期组件 locale 的**唯一取值来源**，禁止再写死 `zh-CN`。文案资源 `zh.ts` / `en.ts` 必须同命名空间同键集，由 `en.ts` 的 `satisfies Resources` 与 `I18nKeys.test.ts` 双层守卫。
- **开发态**：`packages/devmock` 提供全部控制台路由的正常 / 空 / 加载 / 失败状态契约，生产构建不含 Mock；开发态控制台（档位矩阵、观测抖动、拖拽与重置）只在开发态挂载。

### 3.2 HTML 目录索引页的时区例外

`apps/server/internal/protocol/browse.go` 渲染的目录索引页是**零依赖静态页面**（无外链资源、无框架）。但浏览器时区只有前端拿得到，因此该页是唯一一处刻意的例外：时间以 `<time datetime="…Z">` 承载**恒为 UTC** 的机器可读值，页内一段内联原生脚本按 `datetime` 改写展示文案与表头（`data-local` 提供本地时区文案），同时同步目录行的「最近更新」。脚本禁用或失效时页面回退为服务端渲染的 UTC 文本并把表头标回 `(UTC)`，信息不丢失；机器可读值与 API 返回、ETag 校验始终同口径。除这段脚本外不引入任何依赖。

## 4. 数据真源与主要模型

- **元数据真源**：纯 Go `modernc.org/sqlite`，WAL、外键和 busy timeout；迁移位于 `apps/server/internal/persistence/migrations/`。
- **内容真源**：文件系统内容寻址 blob。`asset` 只保存路径、哈希、大小、类型和时间；blob 校验通过、元数据事务提交后才对外可见。
- **身份与权限**：`user`、`api_token`、`revoked_token`、`acl`；口令和令牌只保存哈希/摘要，不保存明文。
- **仓库与制品**：`repository`、`asset`、格式元数据和发布策略；仓库配置中的私有上游只保存受限逻辑引用，实际凭据运行时读取。
- **迁移**：`migration_task` 保存状态、计划、检查点、报告和加密来源凭据；任务重启不自动续跑，按运维流程显式恢复。
- **备份与搬迁**：`backup_package` 只登记包的元数据与规模，包体是 `${JIAN_DATA_DIR}/backups/` 下的 tar.gz（`manifest.json` + `VACUUM INTO` 快照 + `blobs.index` + 内容寻址 blob）。搬迁的传输单位是包而非实时复制流，包内不含任何密钥或节点本地配置（见 `docs/adr/0027`）。每个包生成时额外写**侧车索引** `${JIAN_DATA_DIR}/backups/<packageId>.index`（与包内 `blobs.index` 同格式，每行 `<sha256> <size>`）：全量包的侧车 = 该包完整 blob 集合，**差包的侧车 = 应用后的完整并集**（因此差包可再派生差包）。作用：算基线 blob 集合若靠流式扫描整个基线归档是 O(基线大小)，侧车把它降到 O(索引大小)。差包 manifest 含可选 `expected` 字段（`{count,totalBytes,indexSha256}`，全量包不写 → `omitempty` 向后兼容），表达"应用后应有的完整集合摘要"，导入端据其与本地 ∪ 差集比对判断基线是否就位。
- **导入记录**：`backup_import` 记录每次「从 URL 拉取 / CLI 直传 / 分片上传备份包并导入」的尝试，异步执行、跨重启可查；状态机 `queued → fetching → staging → pending_restart → done`，失败为 `failed` 并带 `error_code`。它只登记进度与结果，真正的数据库替换由启动期 `ApplyPendingRestore` 在 `persistence.Open` 之前完成（先做 `pre-restore-<ts>/` 回滚备份、再原子替换）。
- **分片上传会话**：`backup_upload` 记录一次 Web 分片上传会话（`upload_id`/`file_name`/`total_bytes`/`chunk_size`/`status`/`sha256`/`operator`/`created_at`/`updated_at`/`expires_at`），状态机 `initialized → receiving → completed`，随时可 `aborted`；`backup_upload_chunk` 记录每个已落盘分片（`upload_id`+`chunk_index` 主键、`size`、`sha256`），与 `backup_upload` 外键级联删除。`uploadedChunks` 以磁盘上真实存在的分片为准（续传不依赖库记录是否完好）；`abort` 只删磁盘目录、库记录保留为 `aborted` 供审计，由启动期 `ReconcileExpired` 按 `expires_at` 清理过期会话。
- **原子操作与 operation outbox（退役时保留复用）**：`asset_mutation` 与 `replication_operation_outbox` 仍是原子制品操作的唯一真源——业务写入在同一事务内追加完整 operation envelope，供引用计数、可恢复隔离回收与审计使用。FR-138 退役复制时逐项确认了共享代码归属，该机制依 ADR-0014 / ADR-0019 / ADR-0024 继续保留。
- **复制期表的历史保留**：`repl_change`、`replication_relay_record`、`replication_relay_frontier`、`replication_operation_receipt`、`repl_sync_log`、`replication_apply_log`、`replication_pull_credential`、`replication_sync_event` 等表默认**保留不 DROP**（迁移 0010–0033 原样保留），当前代码不再写入其中的接收 / 中继 / 凭据 / 同步历史语义；`asset_mutation.origin='received'` 等入站身份字段仅为历史数据兼容而保留。
- **当前观测**：`audit_log`、运维告警与风险确认表只表达当前实例自身事实（审计中心、主机监控、业务仪表盘），不参与任何跨实例传播。

## 5. 已落地的通用机制

- **流式处理**：上传、下载、回源和 blob 复制使用流，不把大文件整体读入内存。
- **内容校验**：blob 按哈希校验后落盘；原子制品操作通过统一协调器维护引用、隔离回收和失败恢复。业务完成事务提交后，回滚快照仅作可重试清理，不再反向撤销已发布的 outbox / 审计；启动期恢复只处理本实例自身未完成的业务变更。
- **协议鉴权**：管理会话使用 JWT；机器/协议访问使用 API Token 或对应协议凭据；ACL 由后端统一判断。
- **代理回源**：proxy 按需回源并缓存；出站 URL、DNS、重定向和凭据头按安全策略校验，防止内网访问、DNS 重绑定和凭据外泄。
- **格式启停**：`JIAN_ENABLED_FORMATS` 在启动时决定协议路由和格式后台任务；未启用格式返回 404。
- **部署探活**：`/healthz` 表示存活，`/readyz` 检查 SQLite 和 blob 目录是否就绪。

## 6. 备份、搬迁与写入冻结

实例间的数据搬运只有**节点备份包**一条通道，没有节点角色、水位、凭据或拓扑：

- **备份包**：自包含归档 `${JIAN_DATA_DIR}/backups/jianartifact-backup-<packageId>.tar.gz`，内含 `manifest.json`、`VACUUM INTO` 一致性快照、`blobs.index` 与内容寻址 blob；另有侧车索引 `${JIAN_DATA_DIR}/backups/<packageId>.index` 用于 O(索引) 求差。包内只含数据不含密钥（见 §4 与 [`adr/0027`](adr/0027-package-based-node-backup-and-relocation.md)）。
- **双模式生成**：热备份不停服（`VACUUM INTO` 走独立连接，不占用业务主连接池）；冻结窗口模式先停写再取快照。
- **无登录下载**：`GET /api/v1/backups/{id}/download` 接受由启动密钥派生的 HMAC 签名令牌，新机器无需登录即可按 Range 断点续传拉包。
- **导入**：CLI 直传 / URL 拉取（含 SSRF 防护）/ Web 分片上传三通道，统一走「校验 → 合并 blob → 暂存 db → 写 `restore.pending`」，数据库替换只能由启动期 `ApplyPendingRestore` 在 `persistence.Open` 之前完成（先留 `pre-restore-<ts>/` 回滚点）。
- **写入冻结窗口（FR-135）**：写栅栏是运行时可冻结的有界窗口。所有写服务（仓库、设置、制品、格式元数据、发布策略、用户、令牌、迁移、认证）共用同一个 `FreezeController` 实例；冻结期间 HTTP 层以 503 + `write_frozen` 拒绝业务与管理写入，仅放行读方法、登录、维护命名空间与备份导入/上传出口。只替换其中一部分写路径会让"停写"语义漏网，故必须统一走同一控制器。窗口**必须有界**（`until` 不超过 `now+24h`，`ttlSeconds` 缺省 7200），不提供无限期冻结。
- **节点本地配置不随包迁移**：`public_url`、域名白名单、回源 Token、TLS 与各类密钥都按实例本地处理，导入后必须自行配置。

## 7. 0.8.0 发布边界与复制通道退役

0.8.0 已发布（`VERSION=0.8.0`）。本版的搬迁能力以备份包为单位，并已通过真机演练（热备份与深度校验、签名链接无登录下载与 Range、冻结窗口、三通道导入、重启原子替换）。

- **退役**：实时复制通道（0.7.0 对等复制、0.8.0 主备级联树、逐跳凭据）整体退役，功能面与页面/CLI/诊断读模型一并移除，见 FR-138 与 [`adr/0027`](adr/0027-package-based-node-backup-and-relocation.md)。
- **退役时保留复用**：原子 operation envelope 与 `asset_mutation` 事务边界、`BusinessWriteGate`（现由 `FreezeController` 承载冻结语义）、内容寻址 blob 合并逻辑继续保留——退役不得一刀切删除。
- **数据表**：复制期表默认保留不 DROP（见 §4）。
- **不做**：不做自动选主、自动换父、双主写入、跨分支合并或跨节点主机监控；不再提供任何复制端点、节点角色环境变量与配对流程。

历史实现与验收证据见 [`docs/specs/0.8.0-primary-standby-replication.md`](specs/0.8.0-primary-standby-replication.md)、[`docs/specs/0.8.0-cluster-observability.md`](specs/0.8.0-cluster-observability.md) 与 [`docs/adr/0023-*`](adr/0023-primary-standby-replication.md)（已随 ADR-0027 退役，仅供追溯）。

## 8. 接口与部署

- 管理 REST 的字段和路径以 `api/openapi.yaml` 为唯一真源；生成 Go 接口、前端 client 和 devmock 后再更新概览文档。
- 原生协议端点不进入 OpenAPI，由各格式规格定义，但复用后端鉴权、ACL、写门和生命周期协调器。
- 当前部署路径是单二进制、Docker/Compose、rootless systemd 或单实例 RWO Helm/Kubernetes；每个实例使用独立数据目录，实例间只通过备份包搬运数据。

## 9. 活跃决策与明确不做

活跃决策见 [`docs/adr/README.md`](adr/README.md)，重点包括 SQLite/blob（ADR-0002）、OpenAPI-first（ADR-0004）、单二进制（ADR-0005）、部署编排（ADR-0007）、v2 operation envelope（ADR-0020）、原子制品操作与回收（ADR-0014/0019/0024）以及**以一致性备份包取代实时复制的搬迁模型（ADR-0027，取代 ADR-0023 / ADR-0026 在搬迁用途上的定位）**。

当前不做：外部数据库作为默认后端、S3 作为默认 blob、消息队列、全局分布式协调、实时复制与节点角色、自动选主、自动换父、双主写入、跨节点主机监控和 CGO 依赖。改变这些边界必须先更新对应架构决策。
