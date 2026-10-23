# 接口契约：JianArtifact

> 本文只提供接口分组和行为概览。管理 REST 的唯一契约是 [`../api/openapi.yaml`](../api/openapi.yaml)；生成 Go 接口、前端 client 和 devmock 时必须以该文件为输入。当前发布版本为 `0.12.0`；实时复制通道已随 FR-138 整体退役，`/api/v1/cluster/*` 与 `/api/v1/replication-apply-logs` 等端点已从契约与代码中移除。

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
- 制品：仓库文件树、详情、搜索、下载、统一资产操作和格式相关管理操作。搜索链路的端点、参数与响应字段见下一节「搜索与搜索索引」。
- 发布策略：`GET/PUT /api/v1/users/{id}/publish-policies/{repo}` 读写发布账号在**单个** Hosted 仓库的仓库级策略（路径前缀、每小时制品数、每日/单文件字节上限）；`PUT /api/v1/users/{id}/publish-policies`（body `{repositories[], ...}`）把同一份策略**批量应用到多个** Hosted 仓库，保存前对全部仓库统一预校验（仓库须存在且为 hosted，任一不合法即整体拒绝），响应按仓库逐条返回 `results[]`（`{repository, ok, error?}`）使部分失败可见。`immutableRelease` 已废弃、只读兼容，写入返回 400 `immutable_release_moved`（改由仓库 `immutableRelease` 配置维护）。
- 迁移：Nexus 来源发现、计划、显式启动/取消、进度、报告和恢复。
- 备份与搬迁：节点备份包的生成、列表、详情、删除、完整性校验与下载导出。

- 仓库上游连接状态（FR-114 起，FR-43 扩展）：`Repository.connectionStatus` 是 `ConnectionStatus{status, blockedUntil?, description?}`，**仅 `proxy` / `group` 返回**（`hosted` 无上游概念、字段缺省，向后兼容）。`status` 取值 `READY`（尚未探测）/ `AVAILABLE`（可用）/ `AUTO_BLOCKED`（自动阻止窗口内）/ `HALF_OPEN`（阻止窗口已到期、正在试探上游）/ `UNAVAILABLE`（不可用）/ `OFFLINE`（手动离线）。**`AUTO_BLOCKED` 与 `HALF_OPEN` 同属阻止态**：`HALF_OPEN` 表示后台已发起一轮探测，但业务流量仍**等效封锁**——每轮半开只放行一个探测请求，其余请求快速失败，故展示上不得把它当作「已恢复」；探测成功即回 `AVAILABLE` 并重置退避，失败则回 `AUTO_BLOCKED` 并推进一档窗口（退避为**每档翻倍**，起始 40s 可由 `JIAN_AUTO_BLOCK_BASE_SECONDS` 配置）。`blockedUntil` 只在 `AUTO_BLOCKED` 与 `HALF_OPEN` 两态有值。`online=false` 的仓库一律返回 `OFFLINE`（覆盖内存态）；`POST /api/v1/repositories/{name}/recheck-connection`（仅管理员、仅 online proxy）同步发起一次 HEAD 探测并返回最新状态，不等窗口。字段与枚举真源见 [`../api/openapi.yaml`](../api/openapi.yaml)，语义与闸门设计见 [`specs/0.12.0-upstream-circuit-breaker.md`](specs/0.12.0-upstream-circuit-breaker.md)。
- 仓库存储治理字段（FR-41）：`Repository.quotaBytes` / `quotaAssets` 是仓库级存储配额上限（计量口径为**逻辑字节** `SUM(asset.size)` 与**制品计数** `COUNT(*)`，0 / 缺省 = 不限），`Repository.cacheRetentionDays` 是代理缓存资产的保留天数（**仅 `type=proxy` 可设**，0 / 缺省 = 关闭）。超出配额后该仓库的写入返回 **429 `quota_exceeded`**，消息含当前占用与上限、不含文件系统路径；配额只对 `hosted` 仓库强制，且计量口径**与去重后的物理占用不等价**。创建与更新请求的对应字段为指针语义（缺省 = 不修改；显式 0 = 改为不限 / 关闭代理缓存保留），负数为 400；**非 `hosted` 仓库（`group` 与 `proxy`）携带非 0 配额一律 400**——`group` 不承载写入，`proxy` 的缓存写入发生在读取回源路径上、没有准入预检与流式早拒，配额强制尚未覆盖该路径（管理端因此只在 `hosted` 仓库渲染这两个输入）。字段真源见 [`../api/openapi.yaml`](../api/openapi.yaml)。

### 搜索与搜索索引（FR-149 / FR-150 / FR-153）

全局搜索与它的两个运维端点、一个建议端点。**这四个端点全部不进入 [`../api/openapi.yaml`](../api/openapi.yaml)**——`/api/v1/search` 一直是非契约端点（前端搜索页与仓库内搜索直接用，协议客户端只走各格式原生接口），其余三个按同一先例注册。因此**本文是它们唯一的登记处**：形状会随后续批次演进，读到字段缺失时以代码为准，不要按契约面理解稳定性。设计见 [`specs/search-index-and-performance.md`](specs/search-index-and-performance.md) 与 [`specs/search-package-aggregation.md`](specs/search-package-aggregation.md)。

#### `GET /api/v1/search`

**参数**：

| 参数         | 约定                                                                                                                                                                                                                                             |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `q`          | **必填**，空串返回 400 `bad_request`。支持 `-词` 排除、`"引号短语"`、`repo:` / `-repo:`、`format:` / `-format:`、`ext:` / `-ext:`、`version:` / `-version:`（语法见下）                                                                          |
| `repository` | 可选，限定单仓库（浏览页内搜索即走它，条件下推保证 `total` 精确）。**非管理员 / 匿名**对「仓库不存在」与「存在但无 `read` 权限」返回**同一个 404 `not_found`**，两者不可区分（理由见下）。管理员不受限：他本就搜得到全部仓库，不存在可泄漏的差异 |
| `sort`       | 白名单 `name` / `repo` / `path` / `size` / `updated`，**非法值静默回落**缺省排序（`path asc`），不报错；`group_by=package&order=relevance` 下**被忽略**（见「排序」）                                                                            |
| `order`      | `asc`（缺省）/ `desc` / `relevance`。`relevance` 只在 `group_by=package` 下生效，见「排序」                                                                                                                                                      |
| `group_by`   | `file`（缺省）/ `package`。缺省值**不能改**：仓库内搜索依赖 `path asc` 的文件级结果建目录树                                                                                                                                                      |
| `page`       | 非正数 / 非法回落 1                                                                                                                                                                                                                              |
| `page_size`  | 缺省 20、上限 100；**超上限夹取到 100 而不是回落默认**（否则请求 200 条会被静默当成 20 条，调用方无从察觉）                                                                                                                                      |

**响应**（`file` 模式，字段与改造前一致，只多一个纯追加键 `items[].sha1`）：

- `items[]`：`{repository, path, size, hash, sha1, updatedAt}`。`hash` 是内容寻址的 `blob_hash`，`sha1` 是**新增的纯追加键**（详情面板做校验和展示与比对，老客户端忽略即可）。
- `total`：同条件 `COUNT(*)` 的**精确**匹配文件数，不受分页影响。
- `facets[]`：`{repository, count}`，按仓库聚合的命中数（钻取导航用）。facet 的口径**忽略 `repo:` 正向限定**——用户点选某仓库钻取后，聚合条上仍要能看到其他仓库的命中数，否则无法切换。

`group_by=package` 时**追加**四个字段（`items` / `total` / `facets` 的语义不变）：

- `packageTotal`：不同**包节点**数（含「无坐标」伪包），与 `total` 不同量纲，不要混用；`aggregation.truncated` 为真时它是**下界**。
- `packages[]`：`{coordinate, kind, repositories[], versionCount, fileCount, bestPrecision, latestUpdatedAt, topVersion, versions[], files?}`。`kind` 取 `maven` / `npm` / `file`（`file` 即无法解析坐标的降级伪包，按仓库归组）；`versions[]` 为 `{version, precision, fileCount, size, latestUpdatedAt, files[]}`，`files[]` 为 `{name, path, repository, size, sha1, updatedAt, fileKind}`。注意**包层还有一个可选的 `files` 数组**：只出现在 `kind=file` 的伪包上（这类条目跳过版本层、文件直接挂在包下），有坐标的包不返回该键。
- `aggregation`：`{scanned, truncated}`。单次聚合的扫描预算是 **5000 行**（按 `path ASC` 确定性截断），超出时 `truncated=true`、`packageTotal` 与各 `fileCount` 变成下界；**`total` 不受截断影响**，仍来自 `COUNT(*)`。
- `appliedOrder`：**实际生效**的排序值，用于消除「前端以为在按相关度排」的静默错位。`group_by=package` 时恒为 `"relevance"`（包序只由相关度比较器决定；`order=relevance` 时 `sort` 被忽略、文件级 `items` 固定 `path asc`，其余 `sort` 键只影响文件级 `items`）；`group_by=file` 且请求了 `order=relevance` 时为回落后的 `"asc"`；**其余常规请求不返回该键**。

`precision` / `bestPrecision` 的取值 `exact` → `prerelease` → `substring` → `none`（精度递减）：`exact` 是版本段与关键词完全相等，`prerelease` 是「关键词是版本前缀、余下部分以 `-` 或 `+` 起首」（覆盖 `-rc1` / `-SNAPSHOT` / `-alpha.3` / Maven 时间戳快照 / `+build.7`），`substring` 是其余子串巧合（`1.17.120`），`none` 表示无版本信号或无坐标。`bestPrecision` 取组内最精确的那个。

**为什么 `repository=` 的两个失败情形必须同码**：若「仓库不存在」与「存在但无权」返回不同状态（如 404 与 401/403），调用方只要反复改 `repository=` 参数就能区分两者，从而枚举出实例上有哪些私有仓库存在——这是一条匿名可达的信息泄漏路径，也与 suggest 的口径（可读集合为空时返回空 `items`，绝不 403）冲突。因此非管理员 / 匿名统一 **404 `not_found`**：404 是「你看不到任何东西」的中性回答，403 本身承认了资源存在。真正的内部故障（数据库不可用等）照实上抛，不伪装成 404——它与「仓库是否存在」无关，不构成枚举信号。

**排序**：`order=relevance` 的包级比较器为「精度升序 → 版本向量降序（仅精度非 `none` 时参与）→ 最近更新降序 → 坐标升序」。前三键的合力是「精确版本置顶、同精度下新版本优先、无版本信号的查询退化为原来的 `updated desc` 体验」，因此它是搜索结果页的默认排序；第 4 键保证分页稳定可复现。`relevance` **不写进 `sort`**：`sort` 白名单不变，相关度走 `order`。

**`group_by=package` 下 `sort` 与 `order` 的分工**（实现口径）：包序**恒**由相关度比较器决定，`sort` 只可能影响文件级 `items`；而 `order=relevance` 时**连 `sort` 都被忽略**——相关度需要全量候选集，分页却是无状态的，两者只能解耦，因此文件级 `items` 固定按 `path asc` 分页。请求 `group_by=package` 时响应里的 `appliedOrder` 恒为 `"relevance"`，调用方据此判断实际生效的排序，不要按自己传的 `sort` / `order` 猜。

**`version:` 语法**：`version:1.17.12` 只匹配**版本段严格相等**的制品（`1.17.120`、`1.17.12-rc1` 都不命中），多个 `version:` 之间为 **OR**；`-version:` 为排除（多个为 AND）；`version:`（空值）静默丢弃，与 `repo:` / `ext:` 一致。落地为 SQL 段边界条件（`/v/` 目录段，或 npm 的 `-v.tgz` 文件名尾），**不是等值比较**，因为版本只存在于路径文本里。已知取舍：某非版本目录恰好叫 `1.17.12` 时会误命中（path-as-schema 的固有代价）。

**超时**：搜索链路有统一总预算（环境变量 `JIAN_SEARCH_TIMEOUT_SECONDS`，默认 **10 秒**，`0` / 负数 / 非数字一律回落默认，**不提供「0 = 禁用」**——搜索是无界查询，关掉超时等于开放一个可无限占用连接的入口）。预算覆盖一次搜索的三条语句（计数 / 分页 / facet）与聚合扫描，超时返回 **504 `search_timeout`**，消息提示缩小检索范围。**不复用 `upstream_timeout`**：那个表示「上游不畅」，复用会让用户与运维分不清该查上游还是查索引。

#### `GET /api/v1/search/suggest`

搜索输入框的下拉候选，**与 `/api/v1/search` 同一条权限与超时链路**。

- 参数：`q`（必填语义，但 **trim 后为空返回空 `items` 而不是 400**——suggest 是高频被动调用，空输入是常态而非错误）、`limit`（缺省 10，夹取到 `[1, 20]`；非正数 / 非法回落缺省，超上限夹到上限，**一律不报错**）。
- 响应 `{"items": [{"type": "package" | "repository", "value": "..."}]}`。**不带命中计数**：为每条候选再算一次 `COUNT` 会把「一次轻量查询」变回多次聚合。
- **顺序**：仓库名候选优先占位（更短、且用户常拿它做 `repo:` 限定），剩余名额给包名候选；两类合计不超过 `limit`，跨类去重。**类内**按「候选值长度（rune 数）升序 → 字典序升序」稳定排序，不依赖索引回表顺序；**类间是仓库优先、不是全局短者优先**——两者不能合成一个全局序，否则较长的仓库名会被较短的包名挤掉。
- **权限**：候选只落在「public ∪ 主体被授 `read`」的仓库内（管理员搜全部），无 `read` 权限的仓库**连名字都不出现**。允许集合为空时返回 **200 + 空 `items`**，**绝不 403**——403 会把「这里存在一个你看不到的仓库」当作信号泄漏出去。
- `value` 的形态：npm 即包名（`@scope/pkg` / `pkg`）；Maven 取 artifact（`group:artifact` 里的 `:` 之后部分，因为带 `:` 的坐标当关键词搜索是 0 命中）；解析不出坐标的（`raw` 等）不产出包名候选——它们的「包」维度就是仓库名，已由第一类候选覆盖。**对外只承诺「字符串候选」，不承诺抽取算法**。
- 错误码与搜索一致：401（匿名访问关闭）/ 504 `search_timeout`。

#### `GET /api/v1/search/index`

**仅管理员**（未登录 401、非管理员 403），索引回填状态与一致性自检快照。只读、不触发任何后台动作、不写审计。

> **该端点不得被前端轮询。** 每次调用要在 `asset` 与 FTS 影子表上各做一次全表计数，成本与表大小线性相关（百万级 `asset` 上是秒级开销），且刻意**不加缓存**——缓存会把刚发生的漂移在窗口内报成正常。调用场景限定为运维手动查询。

响应九个字段：

- `ready`：索引**可用于加速搜索**——回填已完成（`completed_at` 非空）**且没有漏索引**（`missingRows == 0`）。这是给运维看的结论位，**不是查询路径的开关**，两者差异见下。注意**孤儿索引行不会让 `ready` 转 false**（它不丢结果），所以不能只看 `ready` 判断索引是否干净，要看 `missingRows` / `orphanRows`。
- `indexed`：已回填行数，按 `last_id` 水位回 `asset` 真源计数（`COUNT(*) WHERE id <= last_id`），与 `total` 同源同口径，两者的差就是剩余量。
- `total`：回填开始时的 `asset` 行数**快照**，只用于进度展示与一致性校验，不参与正确性判定。
- `lastId`：水位（已回填到的最大 `asset.id`），`0` 表示尚未开始。
- `updatedAt`：状态行最后一次变更时刻，用于判断「回填是否还在推进」（不再变化 = 卡住或已结束）。
- `indexedRows`：**索引里真实存在的文档数**（读 FTS5 的 `docsize` 影子表）。与 `indexed` 不是同一口径：后者是「水位以内的资产行数」。
- `assetRows`：`asset` 真源当前行数。
- `missingRows`：**漏索引**的行数——`asset` 里有、索引里没有。**这是危险方向**（这些制品搜不到），`ready` 只因数它转 false，监控要盯的就是这个字段。
- `orphanRows`：**孤儿索引行**的行数——索引里有、`asset` 已不存在。仓库删除经级联带走 `asset` 行时天然产生，只浪费索引空间、不丢结果，因此不影响 `ready`。

**为什么两个方向必须分开报**：合成一个差额（`indexedRows - assetRows`）时，「漏 k 行 + 孤儿 k 行」恰好抵消成 0；而且任何删过仓库的实例都会带着孤儿行让差额恒为正——无论按差额还是按「差额非 0」告警，第一次删仓库之后告警就永久在响，真正的漏索引信号被埋在噪音里。四个数字放在同一条 SQL 里取，保证属于同一时刻。

**`ready` 的语义与它覆盖不到的地方（重要）**：`ready=false` 有两种成因，处置完全不同——① **回填未完成**：搜索整体走既有 `LIKE` 路径，结果正确但没有提速，等它跑完即可；② **发现漏索引**（`missingRows > 0`）：此时索引**仍然被查询路径使用**（查询路径的就绪判定只看 `completed_at`，不看漂移），因此搜索**可能少返回行**，而状态端点不会自己变好——**必须重建（`POST /api/v1/search/index/rebuild`）才能恢复**。这正是要额外报漂移的原因：索引正确性依赖「所有 `asset` 写入必经仓储层」的约定，而回填只跑一次，一旦出现旁路写入（运维脚本、手工 SQL、未来的迁移），漂移就是**永久静默**的——端点若只报进度，用户唯一能感知到的就是「某些制品搜不到」。

**「自动降级」的触发点在哪（别把两种口径读混）**：服务会在**启动自检**与**回填完成**这两个时机检出漏索引并**自动降级**——清空 `completed_at`，搜索立即退回全表检索（慢但完整）。**本端点自身是只读的，不改任何状态**：它只是把漂移报出来，因此经本端点发现 `missingRows > 0` 时**不会**自动降级，也不会自愈，需要手动触发一次重建。这就是 [`OPERATIONS.md`](OPERATIONS.md) 那句「检出漏索引时会自动降级」与本文「索引仍然被查询路径使用」的差别来源——两者的「检出」发生在不同的位置，不是互相矛盾。

**已知盲区（如实登记）**：这四个数字一致**不等于**索引内容正确。若某条 `asset` 行的 `path` 被就地 `UPDATE`，external content 表不会重建该行索引，行数与 rowid 集合却完全一致，于是端点报「无漂移」，而**新名字搜不到、旧名字还能搜到**。现任写入路径不产生这种 `UPDATE`（冲突键就是 `path`，改名走「插新行 + 删旧行」、`rowid` 随之改变），但运维脚本或手工 SQL 可以——那种情况下唯一手段是全量重建。

索引服务未装配时返回 **503 `unavailable`**（兼容分支，生产装配恒为已接线）。

**本端点自身的预算**：状态查询有自己的超时预算（防一次诊断调用把连接占住），耗尽时返回 **504 `search_index_status_timeout`**，消息提示「稍后重试」。刻意**不复用** `search_timeout`：那个表示「检索范围太大 / 该查索引」，而这里是**运维诊断查询本身太重**，处置方向不同（稍后重试，或看日志里的回填进度）。底层计数不会被这个预算中断，它仍在后台跑完。

#### `POST /api/v1/search/index/rebuild`

**仅管理员**，受理一次全量重建。

- **202 + `{"accepted": true}` 只表示已受理**：清空索引 → 水位归零（`completed_at=NULL, last_id=0`）→ 后台分批重灌全部在服务内异步进行。进度看 `GET /api/v1/search/index` 的 `indexed` / `total` / `updatedAt`，但**请人工偶尔查一次，不要做成自动轮询**——那个端点每次都要全表计数（见上）。重建本身不需要盯：完成后 `ready` 转 true，未完成期间搜索走降级路径、结果仍然正确。
- 已有回填或重建在跑时返回 **409 `job_running`**（对齐维护作业手动触发的既有口径），不排队、不并发重入。
- **重建全程在线**：清空后 `completed_at=NULL`，搜索自动退回降级路径，正确性不受影响；代价只是重建期间搜索变慢。
- 不提供「按 id 区间局部修补」：漂移范围不可知，全量重建更简单也更可信。
- **受理成功后写审计**：动作 `search.index_rebuild`、资源类型 `search_index`（`entityKey` 为索引表名 `asset_path_fts`）。这是破坏性动作——重建会清空整张索引并把水位归零，与「维护作业手动触发写 `maintenance.job_run`」「空 Maven 制品清理必须留痕」同一口径；不记的话「索引为什么被清空过」事后无从追溯。409（已有任务在跑）与 503（服务未启用）**不写审计**：它们没有真正触发重建，记一条反而是假账。
- 未登录 401、非管理员 403；索引服务未装配 503 `unavailable`。

错误码小结：**504 `search_timeout`** 是**搜索链路**（`/api/v1/search` 与 suggest）的专属超时码（不是 500），见上；`GET /api/v1/search/index` 的诊断查询自身超预算是另一个码 **504 `search_index_status_timeout`**（表示「这个运维查询太重」，而非检索范围太大）；`GET /api/v1/search` 关键词为空 **400 `bad_request`**；搜索与 suggest 在匿名访问关闭时对匿名为 **401**；`/api/v1/search?repository=` 的两个失败情形对非管理员 / 匿名统一 **404 `not_found`**；重建在已有任务运行时 **409 `job_running`**。

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

- `GET/PUT /api/v1/settings`：实例基础设置（匿名访问开关、对外基础 URL、上游超时、域名白名单、回源 Token、遗留的同步间隔项，以及**自动备份与保留策略**三项）；全部为节点本地配置，不参与任何跨实例传播。
  - FR-40 追加的三个字段：`backupAutoInterval`（秒，`0` = 关闭）、`backupKeepLast`（份，`0` = 不按份数限制）、`backupRetentionDays`（天，`0` = 不限天数）。**三项的 `0` 都是合法取值**，不是"未设置"，因此既可读也会照写；写后由 `backup-auto` 作业在下一轮读取生效（**无需重启**）。
  - 校验：三项均拒绝负数；上限分别为 30 天 / 1000 份 / 3650 天，超限返回 400（防手滑录入无意义的大值，而不是静默接受）。
- `/api/v1/observability/audit/*`：当前节点审计概览、记录、风险批次、通知和确认；只读或按确认接口定义的最小写入。
- `GET /api/v1/observability/host?from=&to=&interface=`：当前节点主机监控（CPU / 内存 / 磁盘 / 网络 / 进程）。`interface` 为可选网卡名（取自响应 `networkInterfaces[].name`），省略时按全部非回环网卡聚合，指定时速率图与累计总量都按该网卡；趋势点 `HostMetricPoint` 另暴露 `networkReceiveBytesTotal` / `networkTransmitBytesTotal`（自网卡启动以来的累计字节，可空），响应含 `networkInterfaces: HostNetworkInterface[]`（各网卡名与该网卡累计总量，无数据时省略）。字段以 [`../api/openapi.yaml`](../api/openapi.yaml) 为准。
- `GET /api/v1/licenses`：管理员读取内嵌依赖协议清单。

### 节点备份与搬迁（FR-132 起；FR-40 增来源与演练）

备份包是搬迁的传输单位：生成包 → 传包 → 导入包 → 校验，不涉及节点角色、水位或拓扑。包内只含 SQLite 一致性快照与内容寻址 blob，**不含任何密钥或节点本地配置**。

- `GET /api/v1/backups?page=&page_size=`：管理员分页读取备份包登记，按最近优先。每条含 `origin`（`manual` = 人工触发，`auto` = `backup-auto` 周期作业生成；迁移 `0047` 之前的存量行按列默认值补为 `manual`）。
- `POST /api/v1/backups`：`{mode: "hot" | "frozen", label?}` 登记并启动生成。生成耗时与包体积同阶，接口立即返回 `queued` 记录，由调用方轮询详情看进度；已有生成任务在跑时返回 `409 backup_in_progress`。
- `GET /api/v1/backups/{id}`：单个备份包详情（状态 `queued`/`snapshotting`/`packing`/`done`/`failed`、大小、内嵌规模、失败摘要、`origin`）。
- `DELETE /api/v1/backups/{id}`：删除包体与登记；被增量差包引用的基线返回 `409 backup_has_derived`。
- `POST /api/v1/backups/{id}/verify?deep=`：完整性校验。浅校验比对 manifest、db 摘要与 `blobs.index` 摘要与规模；`deep=true` 逐 blob 比对内容摘要（耗时与包体积同阶）。结论一次性返回、不落库。
- `POST /api/v1/backups/{id}/drill`：`{level?: "shallow" | "deep"}` 发起**恢复演练**（省略请求体即深校验：浅校验只验归档结构，对「内容被静默篡改但结构完好」的包会判为通过，故默认取更严格的一档）。在**隔离临时目录**内真解包、真校验：归档完整性 → 解包 → 解出的 SQLite 跑 `PRAGMA quick_check` 并确认关键表可查询 → blob 规模比对。深校验耗时与包体积同阶，故返回 **202 + `BackupDrillAccepted{packageId, level, acceptedAt}`**（受理，不代表已完成），由调用方轮询报告列表。**演练未通过不算接口失败**：失败同样落库为一份 `ok: false` 的报告（含失败原因）；仅包不存在（`404`）、包尚未生成完成（`409 backup_not_done`）、级别非法或请求体坏（`400`）走错误响应。**演练是只读路径**：绝不写 `restore-staging/` 与 `restore.pending`，不会在下次重启触发真正的数据替换。发起动作写审计 `backup.drill`（`entityType=backup_package`）。
- `GET /api/v1/backups/{id}/verifications?page=&page_size=`：分页读取该包的历史演练报告（最近优先），返回 `BackupVerificationList{items: BackupVerificationReport[], total}`。报告含 `id` / `level` / `ok` / 起止时间 / `durationMs` / `checks: BackupDrillCheck[]`（逐项 `{name, ok, detail, durationMs}`，`name` 为 `integrity`/`extract`/`sqlite`/`blobs`）/ 失败摘要 `errorSummary`。**检查项失败后仍继续后续项**（能跑的都跑，便于一次看清"哪里坏了"），故一份报告里可能多项为 false。报告随包级联删除。包不存在返回 `404`（不返回空列表）。
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

### 存储体检（只读）

`GET /api/v1/maintenance/storage-health`（**仅管理员**）返回一次磁盘现状快照，用于判断"有没有可回收的东西"。

- **非契约端点**：与 `/api/v1/search/index` 同一先例，不进 `api/openapi.yaml`。它服务运维手动排障，形状随清理能力演进，不属于协议客户端依赖的稳定面。
- **只读，绝不清理**：仓库删除与 blob 回收都是不可逆的运维决策，把一次误判变成自动删除就是数据丢失。真正的清理仍由 `blob-gc` / `storage-cleanup` / `backup-orphan-cleanup` 按各自间隔执行（三者**默认每日一次**，显式设 0 才禁用）。
- **不得被前端轮询**：一次体检要遍历 blob 目录与若干暂存目录，成本随数据规模线性增长。单次调用有 15 秒服务端预算，超时返回 **504 `storage_health_timeout`**；客户端断开不写响应体。
- 响应字段：`orphanBlobs` / `orphanBlobBytes`（磁盘有、asset 无引用的 blob；只由 `blob-gc` 回收）、`quarantinedBlobs` / `quarantinedBytes`（隔离区待回收）、`emptyDirs`（活动 blob 目录下被清空的分片目录）、`preRestoreDirs` / `preRestoreBytes`（恢复前回滚备份，按"保留最近 N 个"回收）、`backupUploadsBytes` / `backupImportsBytes` / `backupDrillBytes`（暂存占用）、`totalBytes`（数据目录总占用）。
- **单项统计失败不整份失败**：仍返回已统计到的项，并附 `partialError` 说明缺项原因——看到一个有缺项的全貌比什么都看不到更接近排障需要。
- **判定口径刻意保守**：只报确定可回收的类别，不猜"看起来没用"的文件（把业务数据误报成垃圾，比漏报危险得多）。

### 404 负缓存诊断与清理（FR-157）

proxy/group 读路径对「确认不存在」的路径做短窗负缓存（默认 TTL 60s，见 `JIAN_NEGATIVE_CACHE_TTL`）。这两个接口用于回答「某个路径现在是不是被钉成 404 了」并在必要时提前放它回源。**仅管理员**；**本批只交付 API，没有配套管理页面**（日志已能回答「为什么 404」的日常问题，为此再建一个有界缓冲与一套 UI 不划算，见 [`specs/0.13.0-proxy-resolve-observability.md`](specs/0.13.0-proxy-resolve-observability.md) §3.3）。

- `GET /api/v1/maintenance/negative-cache`：返回进程内负缓存的有效条目快照（`NegativeCacheSummary`：`total` 为**过滤前**的有效条目总数，`entries` 按写入时间升序、最多 `limit` 条，每项含仓库、路径、写入时间与剩余 TTL）。支持 `repo` / `path_prefix` / `limit`（默认 100、上限 500）过滤。路径按**原样**返回（控制字符已转义）——管理员本就能通过仓库制品列表看到全量路径，此处不构成新增暴露面，而脱敏后的路径对排障没有价值。**只读，不清理、不改状态、不写审计**。
- `DELETE /api/v1/maintenance/negative-cache`：语义按参数递进——**无参数 = 全清；仅 `repo` = 按仓库清；`repo` + `path` = 精确清这一条**。只传 `path` 不传 `repo` 返回 **400**；未知仓库返回 **404**。清理写审计，登记清理的条数与范围。典型场景：上游已修好但同路径仍是 404（负缓存 TTL 内不再回源），清掉对应条目即可立即回源。

负缓存是**纯内存**的，重启即清空；条目数有上限（默认 10000，满时先清过期、仍满则淘汰最早写入的一条），因此不会随运行时长无界增长。相关指标见 [`OPERATIONS.md`](OPERATIONS.md) 的「代理解析结果指标」。两个接口均要求管理员（未登录 401 / 非管理员 403）；未接线时返回 503。

### 一键重启服务（FR-40 追加七）

让本节点重新起服（备份恢复后生效、或按运维要求重载进程）。**仅管理员**。

**执行路径是进程自替换（`syscall.Exec`），不执行任何外部命令。** `jianartifact` 本就有权执行自己的二进制（systemd 的 `ExecStart` 就是它），自替换只是"同一进程换新映像"，**不引入任何新权限**——因此不需要 sudo / polkit / sudoers，也不受 `NoNewPrivileges=true` 影响，在用户级单元 / 系统级单元 / 容器等任何部署形态下都可用。两个必须知道的语义差异（运维视角）：**① systemd 视角上单元从未停止**——PID 不变、`ActiveEnterTimestamp` 与 `NRestart` 都不动，`systemctl status` 看不出重启过；**② 存在一个短暂的中断窗口**（先优雅关停再接替映像）。详见 [`OPERATIONS.md`](OPERATIONS.md) §1.7。

- `GET /api/v1/maintenance/restart`：**只读探测**本机的重启能力，返回与 `POST` 相同的 `MaintenanceRestartResult{supported, command, reason?, scope?}`，但**不含 `delayMs`**（没有受理就没有延迟可言）。
  - **绝不排定任何重启**：它只调 `Probe()`，不调 `RestartAfter`。**必须存在这个只读端点**，因为 `command` 是**手工重启的等效命令**、随部署形态与运行身份而变（rootless 用户级 / 系统级 root / 系统级非 root / 容器），只有后端知道本机该敲哪一条；界面若硬编码一条，在仓库自带的 rootless 用户级单元下就会给出**照做必失败**的 `sudo systemctl restart <unit>`（自带形态该用 `systemctl --user`，且它本就不需要 sudo）——同一台机器上于是同时出现两条命令，其中一条是陷阱。
  - **不能用 `POST` 代替它做探测**：`POST` 在 `supported:true` 时会当场排定重启（见 `RestartAfter`），"打开弹窗即调用它取 command"等于**打开弹窗就重启服务**。因此本端点存在的意义正是把"读能力"与"触发重启"在接口层彻底分开：读操作无论被轮询多少次都不会让进程重启。
  - 探测口径与 `POST` 完全一致（同一次 `Probe`、同一套 `RestartPlan` → `MaintenanceRestartResult` 映射），因此两个端点回显的 `supported` / `command` / `scope` **逐字一致**。
  - **不写审计**：只读探测不改变任何状态，与 `GET /api/v1/maintenance/jobs` 一致（读操作不落审计）；否则界面每次打开弹窗都会在审计库里留一条记录，把真正的重启受理淹没。
  - 未装配重启能力时返回 503 `unavailable`；未登录 401、非管理员 403。
- `POST /api/v1/maintenance/restart`：返回 `MaintenanceRestartResult{supported, command, reason?, scope?, delayMs?}`。
  - **顺序契约：先回响应，再重启。** 返回 **202** 时重启**尚未开始**，只是**已受理**，真正开始（优雅关停 → 自替换）在 `delayMs` 毫秒后（约 1.8 秒），留出响应送达与前端展示的余量。调用方看到 202 后应立刻停止轮询本节点——连接随后断开是**预期行为**，不是失败。若"先重启再回响应"，202 根本发不出去，调用方只会看到连接被重置，既分不清"重启成功"还是"网络抖动"，也拿不到 `command` / `supported` 这些降级信息。
  - **能力预检与降级：`supported: false` 是正常业务结果，用 HTTP 200 返回，不是错误。** 受理前会**同步**预检四条判据——① 本平台是否支持自替换（`syscall.Exec` 仅 Unix）；② `os.Args[0]`（即自替换要加载的路径）能否解析为可执行文件；③ 重启触发点是否已接线；④ 单元名与作用域是否合法。判据不满足时返回 **200** + `supported:false` + `reason` + `command`（可复制到终端手动执行的**手工等效命令**），供前端渲染降级 UI。预检不通过**不**返回 4xx/5xx，也**绝不**假装成功。预检必须在响应之前：后台失败对调用方不可观测（进程随即换映像、响应早已发出），无法区分"重启成功"与"根本没重启成"。
  - **`command` 不是本进程将要执行的命令**（本进程走自替换，压根不执行命令），它是"若你想手工重启，敲这条"。**任何情况下都返回**（含受理成功时），便于审计与人工复现。`scope` 回显配置的作用域（`system` / `systemd-user` / `docker`）。
  - **各作用域的 `command` 形态**：`systemd-user` → `systemctl --user restart <unit>`（**与仓库自带的 rootless 用户级单元配套，推荐**）；`system` 非 root → `sudo -n systemctl restart <unit>`；`system` 且进程是 root → `systemctl restart <unit>`；`docker` → `docker restart <unit>`（**不支持自动重启**，`supported:false`；实例由 compose / k8s 编排时，实际生效的命令应换成 `docker compose restart` / `kubectl rollout restart`，见 [`OPERATIONS.md`](OPERATIONS.md) §1.7）。
  - **配置**（环境变量，见 [`OPERATIONS.md`](OPERATIONS.md) §1）：`JIAN_RESTART_UNIT`（默认 `jianartifact`）指定单元名，`JIAN_RESTART_SCOPE`（默认 `system`，非法值回落默认）决定 `command` 的形态。两者都只影响**手工命令的展示**，不影响自替换本身（自替换加载的是本进程自己的 `os.Args[0]`，与单元名无关）。
  - **安全**：单元名**只**来自环境变量，**绝不**从请求体读取——那是一条直通命令注入的路径。此外单元名须匹配 `[A-Za-z0-9._@-]`，含 `;` / `$()` / 空格 / 换行等元字符时一律降级为 `supported:false`，且 `command` 只给说明性注释、**不回显配置原文**（`command` 会被运维复制到终端粘贴执行，回显等于把一处配置错误变成可粘贴的注入载荷）。
  - 审计动作 `maintenance.service_restart`（`entityType=maintenance`）：受理记 `ok`、降级记 `failed`，均带命令或原因。重启能力未装配时返回 503 `unavailable`（兼容分支，生产装配恒为已接线）。

### 备份包导入（FR-137，三通道）

导入三通道均已交付：CLI 直传、Web URL 拉取、Web 分片上传；导入记录异步跨重启可查。仅管理员。

- `POST /api/v1/backups/{id}/restore`：`{overwrite?, deep?}` **用本机已有的备份包恢复本机数据**，立即返回 `202` 与 `BackupImport`（`origin=local`）。存在的意义是补上「页面上明明列着可用的包，却没有任何按钮能用它恢复」这个缺口——另外三条通道（URL / 上传 / CLI）都要求包从外部来，用户因此会得出「只能备份、不能恢复」的结论。语义与它们**完全一致**（复用同一条编排：校验 → 合并 blob → 暂存 db → 写 `restore.pending`，**需重启生效**），前置校验也相同：包必须 `done`（否则 409 `backup_not_done`）、目标非空需 `overwrite`、已有待生效恢复时 409 `restore_pending`；包不存在 404。
- `POST /api/v1/backups/import`：`{sourceUrl（必填，http/https）, overwrite?, deep?, expectedSha256?}` 由服务端拉取并异步导入，立即返回 `202` 与 `BackupImport`（状态 `queued`）。**SSRF 防护**：拒绝回环/私网/链路本地/云元数据地址，每次拨号重新解析（防 DNS 重绑定），重定向重新校验。
- `GET /api/v1/backups/imports?page=&page_size=`：导入记录分页列表（按 `createdAt` 倒序，最近优先），返回 `BackupImportList{items, total}`。
- `GET /api/v1/backups/imports/{id}`：单条导入记录详情（状态机、进度、错误码）。状态机 `queued → fetching → staging → pending_restart → done`，失败转 `failed`；`pending_restart` 表示包已校验、blob 已按内容寻址合并、db 已暂存、`restore.pending` 已写入，**需重启服务才替换数据库**。
- `GET /api/v1/backups/restore-status`：查询是否存在待重启生效的恢复，返回 `BackupRestoreStatus{pending, packageId?, origin?, stagedAt?}`。`pending=true` 时后续导入/上传一律 `409 restore_pending`（不允许叠加第二次恢复），界面据此解释"为什么现在导入不了"——该状态此前只体现在导入记录行的 `pending_restart` 徽章上，徽章滚出视野后原因就不可见了。未暂存时只返回 `pending: false`，其余字段**省略**（不返回零值占位，避免前端把 `0001-01-01` 当真实时间渲染）。

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
