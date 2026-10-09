# 接口契约：JianArtifact

> 本文只提供接口分组和行为概览。管理 REST 的唯一契约是 [`../api/openapi.yaml`](../api/openapi.yaml)；生成 Go 接口、前端 client 和 devmock 时必须以该文件为输入。当前发布版本为 `0.10.1`（当前开发窗口为 `0.11.0`，进行中）；实时复制通道已随 FR-138 整体退役，`/api/v1/cluster/*` 与 `/api/v1/replication-apply-logs` 等端点已从契约与代码中移除。

## 1. 通用约定

- 管理 API 使用 `/api/v1`、JSON 和 REST 语义。
- 网页会话使用 `Authorization: Bearer <jwt>`；机器和原生协议凭据按各格式约定处理。
- 列表接口统一使用分页参数和总数/游标语义，具体字段以 OpenAPI 为准。
- 错误响应使用稳定错误码和可读消息；不得回显口令、令牌、Authorization、内部地址、原始上游错误或文件系统路径。
- 读取请求不因为访问本身产生业务审计；鉴权、授权失败与写入冻结拒绝按安全审计规则记录。

## 2. 当前管理 API 分组

### 认证与状态

- `POST /api/v1/auth/bootstrap`：空库创建首个管理员（仅未初始化时开放）。
- `POST /api/v1/auth/login`、`POST /api/v1/auth/logout`：登录和会话退出。登录为**本地口令优先、启用 LDAP 时回退目录**（两侧皆失败对外表现一致）。
- `GET /api/v1/auth/oidc/start`、`GET /api/v1/auth/oidc/callback`：OIDC 授权码登录（未配置 `JIAN_OIDC_ISSUER` 时返回 404）；成功后以 URL 片段携会话令牌回前端登录页。
- `GET /api/v1/status`：版本、就绪、迁移版本、用户数和非敏感初始化状态。
- `GET /healthz`、`GET /readyz`：存活与就绪探测。

### 用户、令牌、仓库和制品

- 用户与令牌：用户列表/创建/更新/禁用/改密，API Token 创建和吊销。
- 仓库与 ACL：仓库 CRUD、成员授权、仓库格式/可见性/上游配置和使用统计。授权主体分 `user` / `group` 两态（`AclEntry.subjectType`，缺省 `user` 以兼容既有请求体），`subjectId` 与 `subjectGroupId` 按 `subjectType` 二选一必填、另一列被忽略；动作 `action` 为六档 `read` / `write` / `publish` / `delete` / `acl_manage` / `admin`。蕴含关系（判定动作 → 能使之通过的已授权动作集合）：`read` ← `read`/`write`/`admin`；`publish` ← `publish`/`write`/`admin`；`write` ← `write`/`admin`；`delete` ← `delete`/`admin`；`acl_manage` ← `acl_manage`/`admin`；`admin` ← `admin`。即 **`write` 蕴含 `read` 与 `publish` 但不蕴含 `delete`**（`delete` 需单独授予），`publish` / `delete` / `acl_manage` 各自只蕴含自身且彼此互不满足、均**不蕴含** `read`。判定取「用户自身条目」∪「其所属各组条目」的并集，组内成员变动即时生效。字段与枚举以 [`../api/openapi.yaml`](../api/openapi.yaml) 为准，语义取舍见 [`specs/0.12.0-user-groups-and-fine-grained-actions.md`](specs/0.12.0-user-groups-and-fine-grained-actions.md)。
- 公开仓库列表：`GET /api/v1/public/repositories`（无需认证）在 `items` / `total` 之外附带 `pinnedNames`——**全局置顶**仓库的当前主名（有序），公开页据此置顶前置并显示图钉。
- 仓库使用说明：`GET /api/v1/repositories/{name}/usage` 返回 `UsageInfo.snippets`，每个 `UsageSnippet` 含 `group`（`auth` / `resolve` / `publish` / `other`）——它是**结构化分组标记，不参与本地化**（与随 `Accept-Language` 变化的 `title` / `description` 不同），供界面按「认证 / 解析依赖 / 发布制品 / 其他」折叠分区。
- 仓库别名与重命名：`Repository.aliases` 与 `CreateRepositoryRequest` / `UpdateRepositoryRequest` 的 `aliases` 表达别名集合（别名与主名**共享命名空间、全局唯一**，不得等于主名或与他仓主名/别名冲突）；`POST /api/v1/repositories/{name}/rename`（仅管理员，`{newName}`）重命名后**旧名自动转别名**，旧链接仍可解析。字段、请求体与错误码以 [`../api/openapi.yaml`](../api/openapi.yaml) 为准。
- 置顶仓库：`GET/PUT /api/v1/me/pinned-repositories` 读写**当前用户**的置顶（登录用户；匿名 `GET` 回退**全局置顶**，匿名 `PUT` 返回 401）；`GET/PUT /api/v1/settings/pinned-repositories` 读写**全局置顶**（仅管理员）。读写为**覆盖式**（按 repositoryId 整体替换，空数组即取消全部置顶），非法仓库 ID 返回 404 且不改动既有置顶；按 ID 持久化使仓库重命名不影响置顶。
- 制品：仓库文件树、详情、搜索、下载、统一资产操作和格式相关管理操作。
- 发布策略：`GET/PUT /api/v1/users/{id}/publish-policies/{repo}` 读写发布账号在**单个** Hosted 仓库的仓库级策略（路径前缀、每小时制品数、每日/单文件字节上限）；`PUT /api/v1/users/{id}/publish-policies`（body `{repositories[], ...}`）把同一份策略**批量应用到多个** Hosted 仓库，保存前对全部仓库统一预校验（仓库须存在且为 hosted，任一不合法即整体拒绝），响应按仓库逐条返回 `results[]`（`{repository, ok, error?}`）使部分失败可见。`immutableRelease` 已废弃、只读兼容，写入返回 400 `immutable_release_moved`（改由仓库 `immutableRelease` 配置维护）。
- 迁移：Nexus 来源发现、计划、显式启动/取消、进度、报告和恢复。
- 备份与搬迁：节点备份包的生成、列表、详情、删除、完整性校验与下载导出。

- 仓库上游连接状态（FR-114 起，FR-43 扩展）：`Repository.connectionStatus` 是 `ConnectionStatus{status, blockedUntil?, description?}`，**仅 `proxy` / `group` 返回**（`hosted` 无上游概念、字段缺省，向后兼容）。`status` 取值 `READY`（尚未探测）/ `AVAILABLE`（可用）/ `AUTO_BLOCKED`（自动阻止窗口内）/ `HALF_OPEN`（阻止窗口已到期、正在试探上游）/ `UNAVAILABLE`（不可用）/ `OFFLINE`（手动离线）。**`AUTO_BLOCKED` 与 `HALF_OPEN` 同属阻止态**：`HALF_OPEN` 表示后台已发起一轮探测，但业务流量仍**等效封锁**——每轮半开只放行一个探测请求，其余请求快速失败，故展示上不得把它当作「已恢复」；探测成功即回 `AVAILABLE` 并重置退避，失败则回 `AUTO_BLOCKED` 并推进一档窗口（退避为**每档翻倍**，起始 40s 可由 `JIAN_AUTO_BLOCK_BASE_SECONDS` 配置）。`blockedUntil` 只在 `AUTO_BLOCKED` 与 `HALF_OPEN` 两态有值。`online=false` 的仓库一律返回 `OFFLINE`（覆盖内存态）；`POST /api/v1/repositories/{name}/recheck-connection`（仅管理员、仅 online proxy）同步发起一次 HEAD 探测并返回最新状态，不等窗口。字段与枚举真源见 [`../api/openapi.yaml`](../api/openapi.yaml)，语义与闸门设计见 [`specs/0.12.0-upstream-circuit-breaker.md`](specs/0.12.0-upstream-circuit-breaker.md)。
- 仓库存储治理字段（FR-41）：`Repository.quotaBytes` / `quotaAssets` 是仓库级存储配额上限（计量口径为**逻辑字节** `SUM(asset.size)` 与**制品计数** `COUNT(*)`，0 / 缺省 = 不限），`Repository.cacheRetentionDays` 是代理缓存资产的保留天数（**仅 `type=proxy` 可设**，0 / 缺省 = 关闭）。超出配额后该仓库的写入返回 **429 `quota_exceeded`**，消息含当前占用与上限、不含文件系统路径；配额只对 `hosted` 仓库强制，且计量口径**与去重后的物理占用不等价**。创建与更新请求的对应字段为指针语义（缺省 = 不修改；显式 0 = 改为不限 / 关闭代理缓存保留），负数为 400；**非 `hosted` 仓库（`group` 与 `proxy`）携带非 0 配额一律 400**——`group` 不承载写入，`proxy` 的缓存写入发生在读取回源路径上、没有准入预检与流式早拒，配额强制尚未覆盖该路径（管理端因此只在 `hosted` 仓库渲染这两个输入）。字段真源见 [`../api/openapi.yaml`](../api/openapi.yaml)。

### 用户组与授权主体（FR-36）

用户组把「给 N 个用户逐条写 ACL」收敛成一条：组本身不产生权限，真正生效的是把组作为主体写进仓库 ACL（见「用户、令牌、仓库和制品」的仓库 ACL 条目）。**八个端点全部仅管理员可用**——管理面不做仓库级细粒度授权，`acl_manage` 未接到管理端点，沿用既有 `IsAdmin` 守卫；字段真源见 [`../api/openapi.yaml`](../api/openapi.yaml)。

- `GET /api/v1/user-groups?page=&page_size=`：用户组分页列表（`UserGroupList{items, total}`；`total` 是**全部组数、不受分页窗口限制**）。组规模按「管理面一次列全」设计，分页窗口在服务端切片。
- `POST /api/v1/user-groups`：`CreateUserGroupRequest{name（必填，不可为空白）, description?}` 创建组，**201** 返回 `UserGroup`。组名全局唯一，重名 **409**；组名为空 **400**。
- `GET /api/v1/user-groups/{id}`：读取单个组 `UserGroup{id, name, description, createdAt}`；不存在 **404**。
- `PATCH /api/v1/user-groups/{id}`：`UpdateUserGroupRequest{name?, description?}` 改组名 / 说明，**200** 返回更新后的 `UserGroup`。两字段均为**指针语义**——缺省（不传）表示不改；**显式空串也表示不改**（服务端按 `COALESCE(NULLIF(?, ''), 列)` 处理），故该端点**无法把组说明清空成空串**，只能改成非空文本。改名撞既有组名 **409**；**改名不影响既有授权**（ACL 引用的是组 ID）。
- `DELETE /api/v1/user-groups/{id}`：删组，**204**。连带清理该组的成员关系与**以该组为主体的全部 ACL 条目**——否则会留下指向已消失组的悬空授权，它在 ACL 列表里既显示不出组名、也无法回收；清理失败即整体失败。审计的 `entityKey` 落在**组名**上。
- `GET /api/v1/user-groups/{id}/members`：列出成员 `UserGroupMemberList{items: UserGroupMember[]{userId, username, createdAt}}`。**组不存在返回 404**（否则调用方无法区分「组不存在」与「组存在但没成员」）；成员列表不分页。
- `POST /api/v1/user-groups/{id}/members`：`AddUserGroupMemberRequest{userId（必填）}` 把用户加入组，**201** 返回 `UserGroupMember`。用户不存在 **404**（组不存在亦 404）；**重复加入幂等**（不报错，可安全重试）。
- `DELETE /api/v1/user-groups/{id}/members/{userId}`：把用户移出组，**204**；该成员关系不存在返回 **404**。移出后该用户**立即失去经由该组获得的授权**（判定无缓存、无快照），其自身被单独授予的授权不受影响。

错误码：全部端点未登录 **401**、非管理员 **403**；`GET`/`PATCH`/`DELETE /{id}` 与成员两个端点在目标不存在时 **404**；`POST /{id}` 与 `PATCH /{id}` 在组名非法 / 重名时分别为 **400** / **409**。

**组操作的审计动作**（`entityType=user_group`）：`group.create` / `group.update` / `group.delete` / `group.member.add` / `group.member.remove`。前三者 `entityKey` 为组名、detail 带 `id=<组 ID>`（`repo` 为空）；成员增删 `entityKey` 为组 ID，detail 带 `userId=<用户 ID>`（`member.add` 另带 username）。

### 设置与审计

- `GET/PUT /api/v1/settings`：实例基础设置（匿名访问开关、对外基础 URL、上游超时、域名白名单、回源 Token 与遗留的同步间隔项）；全部为节点本地配置，不参与任何跨实例传播。
- `/api/v1/observability/audit/*`：当前节点审计概览、记录、风险批次、通知和确认；只读或按确认接口定义的最小写入。
- `GET /api/v1/observability/host?from=&to=&interface=`：当前节点主机监控（CPU / 内存 / 磁盘 / 网络 / 进程）。`interface` 为可选网卡名（取自响应 `networkInterfaces[].name`），省略时按全部非回环网卡聚合，指定时速率图与累计总量都按该网卡；趋势点 `HostMetricPoint` 另暴露 `networkReceiveBytesTotal` / `networkTransmitBytesTotal`（自网卡启动以来的累计字节，可空），响应含 `networkInterfaces: HostNetworkInterface[]`（各网卡名与该网卡累计总量，无数据时省略）。字段以 [`../api/openapi.yaml`](../api/openapi.yaml) 为准。
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

### 维护作业（FR-41）

周期作业的只读清单与手动触发。**仅管理员**；**本批只交付 API，没有配套管理页面**。

- `GET /api/v1/maintenance/jobs`：返回本进程**已注册**的周期作业清单与状态快照（`MaintenanceJobList`：名称、间隔秒数、是否运行中、累计执行 / 失败次数、最近开始 / 结束时间、最近错误；未执行过时时间与错误字段为 `null`，空清单为 `[]`）。只读，不触发任何作业。**间隔 ≤ 0 的作业不注册、不出现**，"某作业不在清单里"等价于"该作业在本实例被禁用"；计数自进程启动累计、重启归零。
- `POST /api/v1/maintenance/jobs/{name}/run`：手动触发指定作业一次。返回 **202** 与 `MaintenanceJobRunResult{name, started:true}`——**只表示已受理**，不等待执行完成，是否跑完 / 失败回查清单接口的 `running` / `failures` / `lastError`。作业正在运行（含周期触发的那一轮）返回 **409 `job_running`**，不排队、不并发重入；作业名未注册或已禁用返回 **404 `not_found`**；触发成功写审计 `maintenance.job_run`，失败的响应不回显内部细节。调度器未接线时返回 503 `unavailable`（兼容分支，生产装配恒为已接线）。

两个接口均要求管理员（未登录 401 / 非管理员 403）；`/api/v1/maintenance/` 在写入冻结窗口的放行清单内。清理作业自身的行为（隔离区空目录、终态元数据裁剪、过期上传临时文件、代理缓存保留）与本批的配额语义见 [`specs/0.11.0-storage-governance.md`](specs/0.11.0-storage-governance.md)。

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

## 3. 已退役：复制与集群接口（FR-138）

0.7.0 的对等复制与 0.8.0 的主备级联树接口已整体退役，**当前契约与代码中不再存在**：

- 已移除的数据面端点：`/api/v1/cluster/sync/capabilities`、`/cluster/sync/pull`、`/cluster/sync/blob/{hash}`。
- 已移除的管理端点：`/api/v1/cluster`、`/api/v1/cluster/sync-now`、`/api/v1/replication-apply-logs`、`/api/v1/observability/cluster*`。
- 已移除的行为：`standby` 只读栅栏与 `503 standby_read_only`、节点角色与来源配置、逐跳复制凭据及其 CLI、水位 / 代次续拉。
- 旧的 `standby` 写入拒绝语义现由**写入冻结窗口**的统一写栅栏表达（见下节）。

决策与退役范围见 [`adr/0027`](adr/0027-package-based-node-backup-and-relocation.md)；历史接口形状见 [`specs/0.8.0-primary-standby-replication.md`](specs/0.8.0-primary-standby-replication.md) 与
[`specs/0.8.0-cluster-observability.md`](specs/0.8.0-cluster-observability.md)。

## 4. 节点搬迁相关接口

搬迁不使用任何专用数据面端点：包体通过 `POST /api/v1/backups` 生成、经签名链接或分片上传搬运、由导入端点落盘为待生效恢复。接口清单见上节「节点备份与搬迁」「写入冻结窗口」「备份包导入」「分片上传」。

## 5. 原生协议端点

协议端点不进入 OpenAPI，由格式规格定义，但复用后端鉴权、ACL、写入冻结写门和资产生命周期协调器：

- Raw：`/repository/{repo}/{path}`，支持托管读写和受保护删除。
- Maven/npm：保留各自原生 registry 语义和格式感知读取/删除。
- Docker/OCI：`/v2/` Distribution v2 路由。
- Cargo：sparse `config.json`、索引、crate 下载和发布。
- PyPI：Simple index、包下载和发布。
- Go modules：GOPROXY 只读代理路径。
- NuGet：V3 service index、flat container、registration 和 hosted push。

未启用的格式不注册路由，访问返回 404。新增或变更管理接口必须先改 `api/openapi.yaml`，再同步生成物、devmock、本文和 CHANGELOG。
