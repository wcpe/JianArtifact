# 接口契约：JianArtifact

> 本文只提供接口分组和行为概览。管理 REST 的唯一契约是 [`../api/openapi.yaml`](../api/openapi.yaml)；生成 Go 接口、前端 client 和 devmock 时必须以该文件为输入。0.8.0 尚未发布，但当前工作区的级联接口已经按直接邻接模型实现。

## 1. 通用约定

- 管理 API 使用 `/api/v1`、JSON 和 REST 语义。
- 网页会话使用 `Authorization: Bearer <jwt>`；机器和原生协议凭据按各格式约定处理。
- 列表接口统一使用分页参数和总数/游标语义，具体字段以 OpenAPI 为准。
- 错误响应使用稳定错误码和可读消息；不得回显口令、令牌、Authorization、内部地址、原始上游错误或文件系统路径。
- 读取请求不因为访问本身产生业务审计；鉴权、授权和 standby 只读拒绝按安全审计规则记录。

## 2. 当前管理 API 分组

### 认证与状态

- `POST /api/v1/auth/bootstrap`：空库创建首个管理员；standby 不允许自举。
- `POST /api/v1/auth/login`、`POST /api/v1/auth/logout`：登录和会话退出。
- `GET /api/v1/status`：版本、就绪、迁移版本、用户数和非敏感初始化状态。
- `GET /healthz`、`GET /readyz`：存活与就绪探测。

### 用户、令牌、仓库和制品

- 用户与令牌：用户列表/创建/更新/禁用/改密，API Token 创建和吊销。
- 仓库与 ACL：仓库 CRUD、成员授权、仓库格式/可见性/上游配置和使用统计。
- 制品：仓库文件树、详情、搜索、下载、统一资产操作和格式相关管理操作。
- 迁移：Nexus 来源发现、计划、显式启动/取消、进度、报告和恢复。
- 备份与搬迁：节点备份包的生成、列表、详情、删除、完整性校验与下载导出。

### 设置与审计

- `GET/PUT /api/v1/settings`：实例基础设置；角色、父边、复制凭据和拓扑不通过此接口编辑。
- `/api/v1/observability/audit/*`：当前节点审计概览、记录、风险批次、通知和确认；只读或按确认接口定义的最小写入。
- `GET /api/v1/licenses`：管理员读取内嵌依赖协议清单。

### 节点备份与搬迁（FR-132 起）

备份包是搬迁的传输单位：生成包 → 传包 → 导入包 → 校验，不涉及节点角色、水位或拓扑。包内只含 SQLite 一致性快照与内容寻址 blob，**不含任何密钥或节点本地配置**。

- `GET /api/v1/backups?page=&page_size=`：管理员分页读取备份包登记，按最近优先。
- `POST /api/v1/backups`：`{mode: "hot" | "frozen", label?}` 登记并启动生成。生成耗时与包体积同阶，接口立即返回 `queued` 记录，由调用方轮询详情看进度；已有生成任务在跑时返回 `409 backup_in_progress`。
- `GET /api/v1/backups/{id}`：单个备份包详情（状态 `queued`/`snapshotting`/`packing`/`done`/`failed`、大小、内嵌规模、失败摘要）。
- `DELETE /api/v1/backups/{id}`：删除包体与登记；被增量差包引用的基线返回 `409 backup_has_derived`。
- `POST /api/v1/backups/{id}/verify?deep=`：完整性校验。浅校验比对 manifest、db 摘要与 `blobs.index` 摘要与规模；`deep=true` 逐 blob 比对内容摘要（耗时与包体积同阶）。
- `POST /api/v1/backups/{id}/link`：`{ttlSeconds?}` 签发带时效的下载链接，返回 `{url, expiresAt}`。
- `GET /api/v1/backups/{id}/download?exp=&token=`：流式返回 `application/gzip`。**该端点是唯一不要求会话的备份接口**：`token` 是由启动密钥派生的 HMAC（绑定 packageId 与 exp），使普通链接与新机器可直接拉取；不带令牌时按管理员会话鉴权。支持 Range 断点续传。

### 写入冻结窗口（FR-135）

搬迁切换用「停写 → 生成差包/导入 → 起服 → 解冻」；写栅栏从静态角色扩展为运行时可冻结。仅管理员。

- `GET /api/v1/maintenance/freeze`：查询当前冻结状态，返回 `WriteFreezeState{frozen, until?, frozenAt?, reason?}`；未冻结时省略 `until`/`frozenAt`。
- `POST /api/v1/maintenance/freeze`：`{until?, ttlSeconds?, reason?}` 冻结写入。两者都省略时用 `ttlSeconds` 缺省 7200；窗口必须有界——`until` 须晚于当前且不超过 `now+24h`（否则 400），`ttlSeconds` 钳到 [60, 86400]。**故意不提供无限期冻结**（忘记解冻 = 服务假死）。返回生效后的 `WriteFreezeState`。
- `DELETE /api/v1/maintenance/freeze`：解冻，幂等（未冻结也返 200 与当前状态）。

冻结期间节点拒绝本地业务与管理写入（503 + `write_frozen`），但放行：读方法、`POST /api/v1/auth/login`、维护命名空间 `/api/v1/maintenance/`、以及备份导入/上传路径（`/api/v1/backups/import{s}`、`/api/v1/backups/uploads`）——它们只写 `restore-staging/` 与 `restore.pending`、重启才生效，不破坏冻结语义。

### 备份包导入（FR-137，三通道）

导入三通道均已交付：CLI 直传、Web URL 拉取、Web 分片上传；导入记录异步跨重启可查。仅管理员。

- `POST /api/v1/backups/import`：`{sourceUrl（必填，http/https）, overwrite?, deep?, expectedSha256?}` 由服务端拉取并异步导入，立即返回 `202` 与 `BackupImport`（状态 `queued`）。**SSRF 防护**：拒绝回环/私网/链路本地/云元数据地址，每次拨号重新解析（防 DNS 重绑定），重定向重新校验。
- `GET /api/v1/backups/imports?page=&page_size=`：导入记录分页列表（按 `createdAt` 倒序，最近优先），返回 `BackupImportList{items, total}`。
- `GET /api/v1/backups/imports/{id}`：单条导入记录详情（状态机、进度、错误码）。状态机 `queued → fetching → staging → pending_restart → done`，失败转 `failed`；`pending_restart` 表示包已校验、blob 已按内容寻址合并、db 已暂存、`restore.pending` 已写入，**需重启服务才替换数据库**。

### 分片上传（FR-137 第三通道）

Web 把 GB 级备份包按服务端约定的 **8 MiB** 分片顺序上传，落盘后组装并交本地导入状态机（仍需重启生效）。状态机 `initialized → receiving → completed`，随时可 `aborted`；会话有效期 24 小时，单包上限 50 GiB（与 URL 拉取同一护栏）。

- `POST /api/v1/backups/uploads`：`{fileName, totalBytes, sha256?}` 发起会话，201 返回 `BackupUploadSession{uploadId, fileName, totalBytes, chunkSize, uploadedChunks, status, expiresAt}`（`chunkSize` 由服务端决定，调用方据此切分，不要硬编码）。
- `PUT /api/v1/backups/uploads/{id}/chunks/{index}`：上传单个分片，请求体为原始字节（`application/octet-stream`），200 返回当前会话（含已落盘分片）。分片须 `> 0` 且 `<= chunkSize`；序号越界、单片超额、空体统一 400。
- `GET /api/v1/backups/uploads/{id}`：查询会话（供**续传**）。`uploadedChunks` 由**磁盘上真实存在的分片**推导（以磁盘为准，缺哪片补哪片、重复片幂等覆盖）。
- `POST /api/v1/backups/uploads/{id}/complete`：`{sha256?, overwrite?, deep?}` 拼装并触发导入，202 返回 `BackupImport`。缺片会带上缺失序号；边拼装边算 sha256，与声明不符删除半成品，再交导入（仍需重启生效）；`overwrite`/`deep` 已支持。
- `POST /api/v1/backups/uploads/{id}/abort`：204 取消并清理磁盘；**数据库记录保留**（状态 `aborted`，供审计），只删磁盘目录，故 `GET` 返回 200 + `aborted` 而非 404。

导入错误码：`fetch_failed`（连接/传输/上游非 200/私网拦截）、`package_oversize`（超过 50 GiB）、`sha256_mismatch`（归档整体摘要不符）、`manifest_invalid`（包非法）、`target_not_empty`（409，目标非空且未 `overwrite`）、`incompatible`（包所需 db 版本高于本程序）、`restore_pending`（409，已存在待生效恢复）、`internal`。增量差包导入：`basePackageId` 非空但 `expected` 为空（畸形手工包）报"增量包导入尚未支持"；本地集合 ∪ 包内差集与 `expected` 不一致报"基线包未就位，请先导入基线包 `<id>`"，且绝不落文件/标记。**导入规模护栏**（防解压炸弹，写盘前校验）：快照 8 GiB、blob 总量 50 GiB、blob 条目 500 万；目标「非空」判据排除内置 `anonymous` 主体（迁移 0007 无条件植入）。

包内 `manifest.json` 字段：`schemaVersion`、`kind`（固定 `jianartifact-node-backup`）、`packageId`、`mode`、`basePackageId`、`createdAt`、`nodeId`、`appVersion`、`dbSchemaVersion`、`counts{users,tokens,repositories,acls,assets,formatMetadata}`、`db{file,sizeBytes,sha256}`、`blobs{file,count,totalBytes,indexSha256}`。

## 3. 当前 0.8.0 开发代码中的主备级联复制

`0.8.0` 尚未发布；当前工作区已实现 root primary 与 relay standby 复制源：

- `GET /api/v1/cluster/sync/capabilities`：primary 或开启 relay 的 standby 提供 v2、operation envelope、`sourceNode`、`streamGeneration` 和 `relayEnabled` 能力声明。
- `GET /api/v1/cluster/sync/pull?protocol=v2&since=&limit=`：复制源提供 `{records,latestSeq,hasMore}`；relay 从本地已成功应用的原始日志读取，operation envelope 不可拆分且保留根源 seq。
- `GET /api/v1/cluster/sync/blob/{hash}`：primary 或 relay 按内容哈希流式提供 blob；所有复制传输均为 GET。
- `GET /api/v1/replication-apply-logs`：管理员查询当前节点复制接收审计。
- `GET /api/v1/cluster`：管理员查询当前静态角色、来源配置态、watermark、同步历史和错误；不回显令牌。
- `POST /api/v1/cluster/sync-now`：仅 standby 向配置的直接父节点触发一次受控同步。
- `GET /api/v1/observability/cluster`、`/sync-events`、`/{eventId}`、`/{eventId}/changes`：读取当前节点同步诊断；概览返回 `scope=local_neighbors`、可选 `parent` 和 `children[]`，每条边只表示直接邻接。

standby 的业务、管理和协议写入口统一返回 `503 standby_read_only`；健康、登录、GET/HEAD 和内部复制应用保持可用。

节点专属复制凭据通过本地 CLI 创建、标准输入导入、验证、撤销和轮换；复制中间件只授权 capabilities、pull、blob 三类 GET。

## 4. 0.8.0 级联树接口边界

FR-115/105/119/121/126 已把当前主备扩展为级联树，当前 OpenAPI 与运行时读模型遵循以下边界：

- 每个非根节点只有一个直接父节点，每个节点可有多个直接子节点；子节点主动拉取直接父节点。
- relay standby 只提供已经完整接收、校验和原子应用的连续根源流，不产生本地业务 change/outbox。
- 父子边使用逐跳凭据；水位以根源 `streamGeneration/sourceNode/sourceSeq` 续拉，relay 日志与本地业务变更分离。
- 集群接口返回 `parent`、`children[]` 和 `scope=local_neighbors`；`children[]` 同时包括已上报和已配对但暂未上报的直接 child。
- `standbyReport` 仅保留为旧客户端兼容字段；新页面以 `parent`/`children[]` 邻接边为主，不显示全局拓扑。

详细目标见 [`specs/0.8.0-primary-standby-replication.md`](specs/0.8.0-primary-standby-replication.md)、[`specs/0.8.0-cluster-observability.md`](specs/0.8.0-cluster-observability.md) 和 [`specs/primary-sync-view.md`](specs/primary-sync-view.md)。

## 5. 原生协议端点

协议端点不进入 OpenAPI，由格式规格定义，但复用后端鉴权、ACL、standby 写门和资产生命周期协调器：

- Raw：`/repository/{repo}/{path}`，支持托管读写和受保护删除。
- Maven/npm：保留各自原生 registry 语义和格式感知读取/删除。
- Docker/OCI：`/v2/` Distribution v2 路由。
- Cargo：sparse `config.json`、索引、crate 下载和发布。
- PyPI：Simple index、包下载和发布。
- Go modules：GOPROXY 只读代理路径。
- NuGet：V3 service index、flat container、registration 和 hosted push。

未启用的格式不注册路由，访问返回 404。新增或变更管理接口必须先改 `api/openapi.yaml`，再同步生成物、devmock、本文和 CHANGELOG。
