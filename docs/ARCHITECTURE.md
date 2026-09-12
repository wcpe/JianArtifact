# 架构设计：JianArtifact

> 本文是当前代码的架构真源（HOW）。它区分已落地实现和发布状态：当前发布版本由根目录 `VERSION` 标记；`0.8.0` 尚未发布，但本工作区已落地级联 relay 与邻接控制台实现，仍需整期真实环境验收后才能发布。

## 1. 定位与边界

JianArtifact 是自托管、单二进制交付的多格式制品仓库，提供：

- Raw、Maven、npm、Docker/OCI、Cargo、PyPI、Go modules、NuGet 等制品托管、代理和聚合；
- 管理端、原生协议端点、用户/仓库/ACL 和 API Token；
- Nexus OSS 在线 REST、离线目录和离线包迁移；
- SQLite 元数据、文件系统 blob、单机部署和开发中的主备复制。

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

当前工作区实现为静态级联树：root primary 唯一可写；每个 standby 只配置一个直接父节点，子节点主动 GET 拉取；开启 relay 的 standby 可服务多个直接子节点。节点不共享数据库、文件卷或全局拓扑。

## 3. 模块与依赖方向

后端位于 `apps/server/internal/`，依赖单向向下：

```text
api（HTTP handler / 中间件）
  → protocol（Raw/Maven/npm/OCI/Cargo/PyPI/Go/NuGet）
  → domain（仓库、制品、复制、迁移、观测）
  → repository / storage / migration / auth / upstream
  → persistence（SQLite） + blobstore（文件系统） + archive（备份包格式，纯叶子层）

config 为横切配置层；web 是 go:embed 的前端静态资源。
```

`api` 和 `protocol` 不直接访问 SQLite；鉴权和 ACL 在后端完成，前端只负责展示和交互。前端依赖方向为 `packages/ui` → `apps/web` / `apps/wiki`，不得反向依赖应用。

技术栈固定为 Go、Gin、sqlx、纯 Go SQLite、React、TypeScript、Vite、Mantine 和 i18next；API 设计真源为 `api/openapi.yaml`。

## 4. 数据真源与主要模型

- **元数据真源**：纯 Go `modernc.org/sqlite`，WAL、外键和 busy timeout；迁移位于 `apps/server/internal/persistence/migrations/`。
- **内容真源**：文件系统内容寻址 blob。`asset` 只保存路径、哈希、大小、类型和时间；blob 校验通过、元数据事务提交后才对外可见。
- **身份与权限**：`user`、`api_token`、`revoked_token`、`acl`；口令和令牌只保存哈希/摘要，不保存明文。
- **仓库与制品**：`repository`、`asset`、格式元数据和发布策略；仓库配置中的私有上游只保存受限逻辑引用，实际凭据运行时读取。
- **迁移**：`migration_task` 保存状态、计划、检查点、报告和加密来源凭据；任务重启不自动续跑，按运维流程显式恢复。
- **备份与搬迁**：`backup_package` 只登记包的元数据与规模，包体是 `${JIAN_DATA_DIR}/backups/` 下的 tar.gz（`manifest.json` + `VACUUM INTO` 快照 + `blobs.index` + 内容寻址 blob）。搬迁的传输单位是包而非实时复制流，包内不含任何密钥或节点本地配置（见 `docs/adr/0027`）。每个包生成时额外写**侧车索引** `${JIAN_DATA_DIR}/backups/<packageId>.index`（与包内 `blobs.index` 同格式，每行 `<sha256> <size>`）：全量包的侧车 = 该包完整 blob 集合，**差包的侧车 = 应用后的完整并集**（因此差包可再派生差包）。作用：算基线 blob 集合若靠流式扫描整个基线归档是 O(基线大小)，侧车把它降到 O(索引大小)。差包 manifest 含可选 `expected` 字段（`{count,totalBytes,indexSha256}`，全量包不写 → `omitempty` 向后兼容），表达"应用后应有的完整集合摘要"，导入端据其与本地 ∪ 差集比对判断基线是否就位。
- **导入记录**：`backup_import` 记录每次「从 URL 拉取 / CLI 直传 / 分片上传备份包并导入」的尝试，异步执行、跨重启可查；状态机 `queued → fetching → staging → pending_restart → done`，失败为 `failed` 并带 `error_code`。它只登记进度与结果，真正的数据库替换由启动期 `ApplyPendingRestore` 在 `persistence.Open` 之前完成（先做 `pre-restore-<ts>/` 回滚备份、再原子替换）。
- **分片上传会话**：`backup_upload` 记录一次 Web 分片上传会话（`upload_id`/`file_name`/`total_bytes`/`chunk_size`/`status`/`sha256`/`operator`/`created_at`/`updated_at`/`expires_at`），状态机 `initialized → receiving → completed`，随时可 `aborted`；`backup_upload_chunk` 记录每个已落盘分片（`upload_id`+`chunk_index` 主键、`size`、`sha256`），与 `backup_upload` 外键级联删除。`uploadedChunks` 以磁盘上真实存在的分片为准（续传不依赖库记录是否完好）；`abort` 只删磁盘目录、库记录保留为 `aborted` 供审计，由启动期 `ReconcileExpired` 按 `expires_at` 清理过期会话。
- **当前复制基础**：`repl_change` 保存 root primary 业务写入的根源序列；`replication_operation_outbox` 保存完整 operation envelope；relay standby 另以 `replication_relay_record` 保存已经成功应用的原始 v2 record 作为 inbox，并以 `replication_relay_frontier.forwardable_seq` 限制直接下级只能读取整轮成功后的 record/blob 连续前缀；`replication_operation_receipt` 固化原始 generation/source/seq/operationId/manifest，`repl_sync_log`、`replication_apply_log` 保存各节点本地同步和接收结果。
- **当前观测**：`replication_sync_event`、`replication_apply_event`、`audit_log` 和风险确认表只表达当前实例事实，不参与业务复制。
- **当前凭据**：primary 保存 `replication_pull_credential` 的不可逆摘要，standby 保存绑定本地数据目录密钥的密文；`replication-credential.key` 不复用 JWT 或迁移密钥。
- **级联边界**：父节点只按逐跳凭据服务直接子节点；观测以本节点父边、子边和有界上报快照为范围，不读取孙节点。relay blob 当前采用保守保留策略，待直接子边确认回收语义进一步完善后再做定向清理。

## 5. 已落地的通用机制

- **流式处理**：上传、下载、回源和 blob 复制使用流，不把大文件整体读入内存。
- **内容校验**：blob 按哈希校验后落盘；原子制品操作通过统一协调器维护引用、隔离回收和失败恢复。`asset_mutation.origin=received` 固化入站 intent 身份，standby 重启只恢复当前复制接收 intent；业务完成事务提交后，回滚快照仅作可重试清理，不再反向撤销已发布的 outbox/receipt/审计。
- **协议鉴权**：管理会话使用 JWT；机器/协议访问使用 API Token 或对应协议凭据；ACL 由后端统一判断。
- **代理回源**：proxy 按需回源并缓存；出站 URL、DNS、重定向和凭据头按安全策略校验，防止内网访问、DNS 重绑定和凭据外泄。
- **格式启停**：`JIAN_ENABLED_FORMATS` 在启动时决定协议路由和格式后台任务；未启用格式返回 404。
- **部署探活**：`/healthz` 表示存活，`/readyz` 检查 SQLite 和 blob 目录是否就绪。

## 6. 当前已落地的主备级联复制

当前代码支持 `disabled`、`primary`、`standby` 三种静态角色：

- `primary` 是唯一业务写节点，提供能力协商、v2 records 和 blob 三类复制 GET；不运行出站复制调度器。
- `standby` 可读、可登录、可健康检查，但业务、管理和协议写入由 HTTP 栅栏及领域写门拒绝；导入直接父凭据后，调度器只向该父节点拉取。
- **写入冻结窗口（FR-135）**：写栅栏已从「静态角色只读」扩展为运行时可冻结。所有写服务（仓库、设置、制品、格式元数据、发布策略、用户、令牌、迁移、认证）共用同一个 `FreezeController` 实例；冻结期间 HTTP 层以 503 + `write_frozen` 拒绝本地业务与管理写入，仅放行读方法、登录、维护命名空间与备份导入/上传出口。只替换其中一部分写路径会让"停写"语义漏网，故必须统一走同一控制器。
- 开启 `JIAN_REPLICATION_RELAY_ENABLED=true` 的 standby 可作为复制源，为任意多个直接 child 提供原样 v2 records/blob；relay 不写本地业务 `repl_change`，每条成功应用后先写 `replication_relay_record` inbox，整轮 records 与 blob 全部成功后才推进可转发前缀。
- 复制使用 v2 operation envelope；缺失 blob 流式补齐，完整应用成功后才推进源 stream watermark；复制接收、同步历史和观测事件写在各节点本地。
- 当前复制传输固定 GET/HTTP/1.1；`streamGeneration`、源节点和源序号在 relay 间保持不变，凭据按父子边绑定。
- `public_url`、角色、复制配置、复制状态、审计和观测均按节点本地处理，不作为业务变更复制。

## 7. 0.8.0 级联树实现与发布边界

目标拓扑为有向树：根 primary 唯一可写，每个非根节点只有一个直接父节点，每个节点可挂多个直接子节点。子节点主动向直接父节点 GET 拉取；具备 relay 能力的 standby 只向直接子节点提供已经完整接收、校验和原子应用的根源记录。

当前实现与发布前仍需保持的边界：

- 中继记录与本地业务 `repl_change` 分离，保留根源 stream generation、source node、seq 和 operationId；
- 每条父子边独立持有凭据、watermark 和观测状态；子边观测按 node ID 分开保存，不以单个 standby 快照覆盖其他 child；
- operation envelope 不拆批、不重编号、不生成本地 successor；relay blob 当前采用保守保留策略；
- 父节点只知道直接子节点，页面只展示本节点和直接邻接边，不声称掌握全局拓扑；
- 不做自动选主、自动换父、双主写入或跨分支合并；提升/重新挂接必须人工围栏并生成新的 stream generation。

实现与验收见 [`docs/specs/0.8.0-primary-standby-replication.md`](specs/0.8.0-primary-standby-replication.md)、[`docs/specs/0.8.0-atomic-asset-replication.md`](specs/0.8.0-atomic-asset-replication.md) 和 [`docs/specs/0.8.0-cluster-observability.md`](specs/0.8.0-cluster-observability.md)。自动化已覆盖多跳、多子边、失败轮次隔离和已回收历史压缩；本地真实进程已覆盖水位 0 的 fresh relay→child 重建、operation、单子边断网恢复、relay/child 重启、逐跳凭据轮换和单 relay 双子边。外部网络、Linux arm64、远程 CI 与发布门仍需独立证据。

## 8. 接口与部署

- 管理 REST 的字段和路径以 `api/openapi.yaml` 为唯一真源；生成 Go 接口、前端 client 和 devmock 后再更新概览文档。
- 原生协议端点不进入 OpenAPI，由各格式规格定义，但复用后端鉴权、ACL、写门和生命周期协调器。
- 当前部署路径是单二进制、Docker/Compose、rootless systemd 或单实例 RWO Helm/Kubernetes；多节点实例使用独立数据目录。
- `JIAN_REPLICATION_PRIMARY_URL` 在当前代码中表示 standby 的直接父节点基址（历史变量名保留）；`JIAN_REPLICATION_RELAY_ENABLED` 控制 standby 是否向多个直接 child 提供 relay。自动选主、自动换父和运行时拓扑编辑仍不支持。

## 9. 活跃决策与明确不做

活跃决策见 [`docs/adr/README.md`](adr/README.md)，重点包括 SQLite/blob（ADR-0002）、OpenAPI-first（ADR-0004）、单二进制（ADR-0005）、部署编排（ADR-0007）、v2 operation envelope（ADR-0020）以及 0.8.0 开发版已实现的主备/级联边界（ADR-0023、ADR-0026）。

当前不做：外部数据库作为默认后端、S3 作为默认 blob、消息队列、全局分布式协调、自动选主、自动换父、双主写入、跨节点主机监控和 CGO 依赖。改变这些边界必须先更新对应架构决策。
