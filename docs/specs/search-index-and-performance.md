# 功能规格：搜索索引与查询性能改造 / 搜索建议与自动补全（FR-149 / FR-153）

> 状态：草拟 · 2026-10-19 · 新 Schema + 跨模块 + 并发/事务类（迁移 `0049`、后台回填、搜索主链路三语句改写、ACL 判定批量化）· 关联 PRD：FR-149、FR-153 · 索引取舍见 [ADR-0030](../adr/0030-fts5-search-index.md)
>
> 覆盖两个 FR：**FR-149** 见 §1–§2.1、§3.1–§3.7、§4 第一组、§5 第一批；**FR-153** 见 §2.2、§3.8–§3.9、§4 第二组、§5 第二批。两者共用同一张 FTS 索引与同一套权限收窄实现。

## 1. 背景与目标

**问题**：全局搜索（FR-30）在百万级 `asset` 规模下响应缓慢，且没有上界。根因是一条**可解释的放大链**，每一环单独都不致命，叠加起来才是「慢到不可用」：

| # | 根因                              | 代码位置                                                                                                                                                                                                                         | 为什么慢                                                                                                                                                                                             |
| - | --------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1 | 前导通配无法命中任何索引          | `buildSearchWhere` 为每个包含词生成 `path LIKE ?` 且参数为 `"%"+escapeLike(t)+"%"`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L244-L278)）                                                                    | 前导 `%` 使 B-Tree 无从定位，只能整表逐行做字符串匹配；`asset.path` 上的 `UNIQUE(repository_id, path)`（[0002_asset.sql](apps/server/internal/persistence/migrations/0002_asset.sql#L13-L23)）对它完全无效 |
| 2 | 一次搜索要跑**三条**全扫 SQL     | `COUNT(*)` + 分页 `SELECT`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L307-L320)）+ facet 的 `GROUP BY`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L330-L339)）                            | 三条语句共用同一个不可索引的 `WHERE`，即单次请求把全表扫三遍；库越大线性恶化                                                                                                                          |
| 3 | 排序无法走索引                    | `searchOrderBy` 的 `name` 键用 `replace(path, rtrim(path,…), '')` 表达式、`repo` 键用 `(SELECT name FROM repository WHERE id = asset.repository_id)` 相关子查询（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L282-L303)） | 排序键是计算列，只能建临时 B-Tree 对**全集**排序，`LIMIT` 无法提前收敛；`repo` 键还会为每行跑一次子查询                                                                                                |
| 4 | ACL 判定逐仓库 N+1                | 非管理员路径先 `ListPublic()`、再 `List(1000,0)` 取全量仓库，然后逐仓库 `HasPermission`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1044-L1071)）                                                  | `HasPermission` 内部最多两条 SQL（用户分支 + 组分支，[acl_repo.go](apps/server/internal/repository/acl_repo.go#L111-L125)），N 个私有仓库即最多 `2N` 条查询，且与搜索本身的开销相乘                 |
| 5 | 第二次全量仓库列举                | 带 `repoScope` / `repo:` / `format:` 时再 `List(1000,0)` 一次，并在 Go 内存里 `narrow`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1081-L1113)）                                                  | 同一请求内向仓库表发起两次「取全量」，随仓库数增长                                                                                                                                                    |
| 6 | 仓库名映射再 N+1                  | `repoName()` 闭包按 `RepositoryID` 逐个 `GetByID`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1138-L1147)）                                                                                       | 结果条目与 facet 里出现的每个不同仓库各一条查询                                                                                                                                                       |
| 7 | 全链路无超时保护                  | `SearchByFilter` / `SearchFacetsByFilter` 用 `db.Get` / `db.Select`（非 Context 版本），域层 `SearchAssets` 不接受 `ctx`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1021)）                     | 病态查询（宽泛词、深翻页）可无限期占用连接与 SQLite 读事务；客户端断开也不会中止，没有上界                                                                                                            |
| 8 | 协议搜索把上述成本乘以成员数      | npm `registrySearch` 对 group 的每个成员循环调用完整 `SearchAssets`，`limit = from+size`（[npm_registry.go](apps/server/internal/protocol/npm_registry.go#L330-L333)）                                                              | 每个成员各跑三条全扫；`npm search` 因此比网页搜索更慢                                                                                                                                                 |

**目标（对用户）**：

- **搜索重新变成可交互的操作**：百万级真实数据下常见关键词与版本号检索 P95 < 500ms（PRD FR-149 的验收口径），而不是等几秒到超时；
- **快而不漏**：改造是**纯性能工程**——既有搜索语法（包含词、`-排除`、`repo:`、`format:`、`ext:`、引号短语）与结果集必须逐字等价，不借机改语义、不改默认排序、不改响应结构；
- **慢查询有上界**：病态查询返回明确错误而不是挂死，上界可配置；
- **升级不换停机**：新索引在后台回填，启动不等它；回填未完成期间搜索**照常可用**（正确但尚未提速）；
- **输入时能看到候选**（FR-153）：搜索框给出包名与仓库名建议，且**绝不泄漏无权限仓库**。

**为什么需要 spec**：本批动到**新 Schema**（新增 FTS5 虚拟表、回填状态表，迁移 `0049`）、**跨模块**（持久层 / 领域层 / API 层 / npm 协议层 / 前端）、以及**并发与事务**（索引写入必须与资产写入同事务；后台回填与在线写入并发交叠；回填可中断续跑）。三者任一都足以要求一份可评审的契约，何况同时发生。

## 2. 需求（要什么）

### 2.1 FR-149 范围内

1. **新索引**：为 `asset.path` 建 FTS5 虚拟表，分词器必须是 `trigram`（唯一能支撑「任意子串」匹配的分词器），随迁移 `0049` 建结构；索引与 `asset` 的写入**在同一事务内**同步（取舍见 ADR-0030）。
2. **后台异步回填**：升级后由后台分批回填历史行，**不阻塞启动**、任意时刻可中断且重启可续、可观测（日志 + 只读状态端点）、可重建（管理员端点）。
3. **查询改写**：搜索的 `COUNT` / 分页 `SELECT` / facet 三条语句全部改为「FTS 候选集驱动 + 既有后置谓词」，语义逐字等价；保留一条**降级路径**应对索引无法覆盖的查询（含 1–2 字符词）。
4. **超时保护**：搜索链路接受 `context` 并在域层施加可配置上限；超时映射为稳定错误码。
5. **ACL 判定批量化**：逐仓库 `HasPermission` 改为固定两条批量查询；顺带消除仓库名映射的 N+1；`repo:` / `format:` 收窄在 Go 内存里做这一既有形态保持不变（它不产生查询）。
6. **协议侧收口**：npm `registrySearch` 的语义、响应结构、去重与 `total` 口径全部不变，但享受索引加速与超时保护。
7. **零回退**：既有搜索语法行为、`total` 精确性、facets 语义、排序键白名单、分页参数夹取口径、`/api/v1/search` 响应结构与错误码全部不回退。

### 2.2 FR-153 范围内

1. **suggest 端点**：新增 `GET /api/v1/search/suggest`，复用 FR-149 的 FTS 索引给出**包名**与**仓库名**两类候选。
2. **权限硬约束**：无读权限的仓库不得出现在候选中——复用 FR-149 的批量 ACL 判定，不新增第三套实现。
3. **前端**：搜索输入框下拉候选、防抖 ≤ 300ms、键盘 ↑/↓ 选择与回车确认、Esc/失焦收起、**无候选时不弹空面板**。

### 2.3 不做（范围外，附理由）

| 不做的事                                                     | 理由                                                                                                                         |
| ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| 相关性排序 / `order=relevance` / `version:` 严格匹配 / 包级聚合 | 属 FR-150（[search-package-aggregation.md](search-package-aggregation.md)）；本批改索引不改排序契约，默认排序仍为 `path asc`，避免中途换默认值 |
| 搜索结果页主从双栏与折叠树                                   | 属 FR-151                                                                                                                    |
| 搜索进度指示与请求取消（结果区）                             | 属 FR-152。suggest 自身的乱序保护是本 FR 的一部分，两者不重叠                                                                  |
| 输入即搜（结果区随键入刷新）                                 | PRD 明示不做。suggest 只弹候选，**不触发搜索**；提交仍由回车/按钮显式发起                                                       |
| 跨节点搜索聚合                                               | PRD 明示不做                                                                                                                 |
| `asset` 表结构变更（版本列、包坐标列）                       | 属 FR-150 的数据模型议题；本批只新增虚拟表与状态表，`asset` 一列不改                                                            |
| 拼音 / 同义词 / 拼写纠错 / 搜索历史                          | 无 PRD 条目，且都需要新的词表或语料，YAGNI                                                                                    |
| 把成员循环合并成单条 SQL（npm）                              | 会改变去重口径与 `total` 语义，本批只做加速不做语义变更                                                                        |
| suggest 的服务端缓存 / 速率限制                              | 与「组内成员变动即时生效」的既有授权承诺冲突；限流属网关层议题，本批不做                                                       |

## 3. 设计（怎么做）

### 3.1 迁移 `0049`：FTS5 trigram 虚拟表与回填状态

**建议文件名：`0049_asset_path_fts.sql`**（对齐既有命名风格：`0045_storage_cleanup_indexes.sql`、`0046_user_groups.sql`——编号 + 下划线小写短语，只描述本迁移建了什么）。文件头照抄既有范式给出「引入版本 / 影响 / 数据处理 / 回滚 / 相关」五行，并说明为什么**不在迁移里回填**。

**由 [ADR-0030](../adr/0030-fts5-search-index.md) 决策、本规格只承接不重述的部分**（该 ADR 状态为「已接受」）：采用 external content 形态（`content='asset'`、`content_rowid='id'`），以 `asset.id` 作 `rowid`、索引不复制路径文本；索引由**应用层显式维护、不使用触发器**；一致性异常时按水位续灌或全量 `rebuild`。

因此写入点被穷举为三处，全部必须在**既有事务边界内**成对维护索引：

| 写入点                                                                                                     | 形态                                         |
| ---------------------------------------------------------------------------------------------------------- | -------------------------------------------- |
| `UpsertWithTime`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L32-L47)，`INSERT … ON CONFLICT DO UPDATE`） | 新增或覆盖写（同一 `repository_id + path`）  |
| `DeleteByPath`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L67-L70)）                        | 单条删除                                     |
| `asset_mutation_repo.go` 的 apply / 回滚（[asset_mutation_repo.go](apps/server/internal/repository/asset_mutation_repo.go#L274-L330)） | 统一资产操作的批量提交与回滚                  |

外加一处**整仓**路径：仓库删除经 `repository` 的 `ON DELETE CASCADE`（[0002_asset.sql](apps/server/internal/persistence/migrations/0002_asset.sql#L15)）级联清掉该仓库全部 `asset`，此时按仓库一次性清理索引比逐行维护更直接。

**实现注意（由 ADR 决策直接推出的必改点）**：`UpsertWithTime` 与 `DeleteByPath` 目前都是**裸单语句** `Exec`（各自自动提交）。加入索引维护后必须提升为显式事务（或由调用方已有的 `tx` 承载），**不能**写成「先 `Exec` asset、再 `Exec` 索引」两条自动提交语句——那正是 §3.1 不变量 1 要排除的窗口。

本规格固定以下不变量：

1. **同事务**：一次资产写入与它对应的索引更新必须落在**同一个事务**里提交——既不允许「元数据已提交、索引未写」（会漏索引，且无法靠一次性的回填补救），也不允许反序；
2. **一一对应**：索引行的 `rowid` 严格等于 `asset.id`。**孤儿索引行不会伪造结果**——查询按 rowid 回 `asset` 取真值，被删行在回表时自然消失（ADR-0030 已论证）；但仍须在删除路径与仓库级联时清理，否则索引只增不减，体积与「索引行数 ≈ `asset` 行数」的一致性校验都会失效；
3. **只镜 `path`**：索引只需镜像 `path` 一列；`size` / `updated_at` / `repository_id` 的变更**不得**要求重建索引；
4. **幂等写**：对同一 `rowid` 重复写同一条 `(id, path)` 不得产生第二条索引行（回填与在线写入交叠时会真的发生，见 §3.2）。ADR-0030 已指出 external content 的正确维护必须**成对**执行 `'delete'` 与重新插入，这两步必须封在同一个可测试的入口里。

迁移与实现细节：

- 分词器参数固定为 `tokenize='trigram'`，**不得**开启 `case_sensitive`（原因见 §3.3 的大小写口径）。
- 同一迁移里建回填状态表（**单行语义**，用 `CHECK (id = 1)` 钉死）：

  `search_index_state(id INTEGER PRIMARY KEY CHECK (id = 1), last_id INTEGER NOT NULL DEFAULT 0 CHECK (last_id >= 0), total_rows INTEGER NOT NULL DEFAULT 0 CHECK (total_rows >= 0), completed_at TEXT, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`

  初始行 `(1, 0, 0, NULL)`。`completed_at IS NULL` 即表示回填未完成——**不另设 `done` 列**，避免两个字段表达同一状态而漂移。`total_rows` 是回填开始时取的 `asset` 行数快照，仅供进度展示与一致性校验，不参与正确性判定。**不复用 `schema_migrations`**：迁移版本表达「结构已建」，回填完成表达「数据已灌」，两者生命周期不同，混用会让「升级完成」与「搜索已提速」不可区分。单行形态而非按 name 多行：当前只有一个索引，不预留多行；将来若新增索引另起表或扩展主键。
- **迁移内不回填**：迁移器把每个文件放在单个事务里执行（[migrate.go](apps/server/internal/persistence/migrate.go#L11-L42)），且只前向、无 down。百万级回填塞进迁移会把升级变成一次长时间持写锁的不可用事务，且中途失败无法断点恢复。回填交给 §3.2。
- **启动自检**：迁移建虚拟表若因构建缺少 FTS5 而失败，必须给出可读的中文错误（说明「当前 SQLite 构建不支持 fts5」），而不是让升级挂在一句泛化的 `SQL logic error`。已实测 `modernc.org/sqlite v1.60.1` 支持 `fts5` 与 `tokenize='trigram'`，但自检不依赖该结论。

### 3.2 后台异步回填

**触发点**：进程启动、迁移成功后启动一个**一次性后台 goroutine**。不用 FR-44 调度器——调度器语义是「启动不立即执行，首次在第一个间隔到期后」（见 [0.11.0-storage-governance.md](0.11.0-storage-governance.md) §3.2），而回填需要尽快开始、只跑一次、跑完即退出；塞进周期作业还会污染 `jianartifact_scheduler_job_*` 的运行中/失败计数语义。

**算法（可任意时刻中断、重启续跑）**：

1. 读 `search_index_state`（`id = 1`）。`completed_at IS NOT NULL` → 立即退出，零开销。
2. 取上界快照 `upper = SELECT COALESCE(MAX(id), 0) FROM asset`，同时把 `total_rows` 写为 `upper` 对应的行数快照。上界**只取一次**：此后新写入的行由在线同步负责，回填不追（追会导致回填永不收敛）。
3. 循环批：`SELECT id, path FROM asset WHERE id > ? AND id <= ? ORDER BY id LIMIT 2000` → 写索引 → **同一事务内**把 `last_id` 推进到本批最后一条 `id`。
4. 取空批即置 `completed_at = datetime('now')` 并记完成日志；否则回第 3 步。每批之间让出调度（短 `sleep` 1ms 或等价让出），并检查进程退出信号用于优雅停机。

**每批必须让出唯一连接（硬约束）**：`persistence/db.go` 里 `SetMaxOpenConns(1)`，全库只有一个连接。回填事务必须短、批与批之间提交并让出，否则会阻塞全部在线读写。

**为什么必须幂等写**：`upper` 快照内的某行可能在回填到它之前被**覆盖写**（同一 `repository_id + path` 的 UPDATE）。在线同步会用新 path 更新索引，回填随后又读到同一个 `id`（此时已是新 path，值相同但是第二次写）。不幂等就会在索引里留下两条同 rowid 记录，表现为搜索结果重复。回填的写入原语必须按 §3.1 不变量 4 幂等。

**回填期间搜索的正确性（关键决策）**：`completed_at IS NULL` 时**整体走既有 LIKE 路径**（§3.3 的降级分支），因此「搜索可用」= 结果正确、有响应，但尚未提速。刻意取「宁可慢而全，不可快而漏」：另一条候选方案是「FTS 结果 ∪ LIKE 结果」，但它的成本等同一次全扫，等于不提速，故不取。回填耗时（百万级、每批 2000）预计分钟级，属一次性窗口。

**观测**：

- **日志**（中文，不含任何路径内容）：进度按「本次进度较上次记录跨过 ≥10%」节流，避免每批一行刷屏；完成与失败各一行，含总行数、已回填行数、耗时。
- **只读状态端点**：`GET /api/v1/search/index`（仅管理员）→ `{ready, indexed, total, lastId, updatedAt}`。回填中 `ready=false`，`indexed/total` 供判断剩余量。按 `/api/v1/search` 的既有先例作为**非契约端点**注册（[main.go](apps/server/cmd/jianartifact/main.go#L319) 处同一分组）。

**失败与重建**：

- **单批失败**：不推进 `last_id`，指数退避重试；仍失败则记错误日志、`ready` 保持 false、goroutine 退出（**不阻塞进程**）。搜索继续走降级路径（可用但慢），下次启动自动从 `last_id` 续跑。
- **索引损坏或与 `asset` 漂移**（例如有人手工改库、或同步写点曾有 bug）：`POST /api/v1/search/index/rebuild`（仅管理员）→ **202 只表示已受理**，后台执行「清空索引 → `completed_at=NULL, last_id=0` 重置 → 按本节重跑」；正在运行时再触发返回 **409**（对齐既有运维接口 `job_running` 口径）。重建全程在线：清空索引后 `completed_at=NULL`，搜索自动走降级路径，正确性不受影响。
- **不提供**「局部修补」（按 id 区间补索引）：漂移范围不可知，全量重建更简单也更可信。

### 3.3 查询改写：语义等价是硬约束

**语句形态**：FTS 只负责产出候选 `rowid`，命中的行再回 `asset` 取真值（ADR-0030 的「以 `rowid` 回表取真实 `path`」）。落到 SQL 上采用 **`id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)` 子查询**追加到既有 `WHERE`，而不是字面写成 `JOIN asset_path_fts`。

两者语义相同（都是按 rowid 回表、孤儿索引行自然被丢弃），选子查询有两个具体理由：

1. **避开列名歧义**：external content 表同样暴露 `path` 列，`FROM asset JOIN asset_path_fts` 会让既有裸列名 `path`（`buildSearchWhere` 与 `searchOrderBy` 里都是裸写的）变成 `ambiguous column name: path`，进而必须机械地把每一处列名限定成 `asset.`——而 `searchOrderBy` 的 `repo` 键里还有 `asset.repository_id` 这样的既有显式限定，两种风格混用易错。子查询形态下 `buildSearchWhere` / `searchOrderBy` **一行都不用改**，改动面收敛到「追加一个 AND 条件」。若评审偏好 JOIN 形态，则必须把两处函数的裸列名全部限定为 `asset.` 并有等价性测试兜底，二者择一但要一致。
2. **候选集算一次、按整数主键回表**：FTS 产出的 rowid 列表被物化一次，`asset` 侧仍走主键定位，既有谓词原样生效。

三条语句（`COUNT`、分页 `SELECT`、facet `GROUP BY`）**共用同一段 `WHERE` 构造**——保持 `buildSearchWhere` 作为单一构造点，新增的候选集条件由它统一追加，避免三条语句各拼一遍而漂移。

**逐条语法映射**：

| 既有语法         | 既有谓词                                                               | 新路径                                                                                                                  |
| ---------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| 包含词（多词）   | 每词一条 `path LIKE '%t%'`，**AND**                                     | 全部提升为 FTS `MATCH` 的 **AND** 项；每词作为 FTS5 **字符串字面量**（`"` 包裹、内部 `"` 双写）                          |
| 引号短语         | `path LIKE '%含 空格 短语%'`（整串作为**一个**连续子串）                | 同样是一个 FTS5 字符串字面量；trigram 的短语匹配即连续子串匹配，等价                                                     |
| 排除词 `-词`     | `path NOT LIKE '%t%'`                                                  | **保留为 SQL 后置谓词**，作用于候选集；不下推进 FTS 的 NOT                                                              |
| `ext:`           | `path LIKE '%.jar'`（多个为 OR 集合）                                  | **保留为 SQL 后置谓词**                                                                                                 |
| `-ext:`          | `path NOT LIKE '%.jar'`                                                | 保留为 SQL 后置谓词                                                                                                     |
| `repo:` / `-repo:` / `format:` / `-format:` | 域层先按仓库元数据收窄成 `repository_id IN (...)`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1089-L1113)） | **不变**，仍在域层收窄，SQL 谓词原样                                                     |
| 仓库范围 / ACL   | `repository_id IN (...)`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L267-L273)）                                  | **不变**                                                                                                                 |

**为什么 `ext:` 不进 FTS**：既有语义是 `LIKE '%.jar'` 的**任意位置包含**而非后缀（`foo.jar.txt` 也会命中），把它搬进 FTS 需要让 `.` 参与分词并重定义 `%` / `_` 的转义口径，语义风险高于收益；而它作为候选集上的后置谓词时成本可忽略（候选集通常 10²–10⁴ 行），且能保证与旧实现逐字一致。

**降级路径（必须有，而且是唯一可能让结果集悄悄变少的地方）**：以下任一成立时，整体走既有实现（`buildSearchWhere` 的原始形态），**不使用** FTS：

- `IncludeTerms` 为空（例如只有 `-词`、只有 `ext:jar`、只有 `repo:`）——没有可用的正向候选驱动条件；
- **任一**包含词的**字符数（rune）< 3**——trigram 能匹配的最短子串是 3 字符，1–2 字符词若强行走 FTS 会**静默漏结果**，这是本批最危险的错误模式，因此判定按 rune 数而非字节数（中文 2 字词如「标签」同样降级）。

降级条件必须收敛在**一个函数**里返回布尔值，并有表驱动测试逐一钉住（纯 `ext:`、纯 `-词`、纯 `repo:`、1 字符、2 字符、2 个汉字、混合词）。

**安全与语义防线（FTS5 查询串构造）**：用户输入绝不可直接拼进 `MATCH`。FTS5 的匹配串有独立语法（`*`、`(`、`)`、`-`、`OR`、`NEAR`、`^`、`:`），直接把 `q = "a OR b"` 交给 `MATCH` 会变成布尔查询而不是「包含子串 `a OR b`」。每个词必须整体包成 FTS5 字符串字面量并把内部 `"` 双写。这既是语义回退防线也是注入面防线，必须有专项测试。

**大小写口径**：既有 `LIKE` 在 SQLite 默认设置下**只对 ASCII** 不区分大小写；trigram 分词器默认同样只折叠 ASCII 大小写。因此二者天然对齐，但这是需要**用测试钉住**的等价性，不是可以假设的。测试固定两条：`Foo` 与 `foo` 互命中；非 ASCII 不做大小写折叠（与旧实现一致）。

**排序与分页**：

- `searchOrderBy` 的表达式的形状不变（`name` 的 `replace/rtrim`、`repo` 的相关子查询）。本批**不**为排序新增索引：候选集收窄后临时 B-Tree 的输入规模从 10⁶ 降到 10²–10⁴，`ORDER BY` 仍是 `USE TEMP B-TREE`，但已不是瓶颈。为排序建表达式索引属于另一个议题（且 `name` 键的表达式无法索引）。
- `LIMIT/OFFSET` 语义不变；深翻页仍在候选集内线性跳过，见 §6。
- `total` 仍是同条件 `COUNT(*)`，精确性不变（这正是「候选集 + 后置谓词」形态的价值：三条语句共享完全相同的行集，计数与分页不会互相矛盾）。

### 3.4 `context` 超时保护

**放置层级**：施加在**领域层 `SearchAssets` 入口**，签名接收 `ctx context.Context`，内部 `context.WithTimeout(ctx, s.searchTimeout)` 并向下传。

- 三个调用方（`/api/v1/search` handler、npm `registrySearch`、新 suggest 端点）因此**统一**受保护；
- 放在 handler 层会让 npm 与 suggest 各写一遍；放在仓储层则拿不到「一次搜索的总预算」（三条语句要共享同一个 deadline，而不是各给一份）；
- `ctx` 源头是 `c.Request.Context()`——客户端断开即取消，不继续白烧 CPU。handler 不再额外套超时，避免双层 deadline 语义混乱。

**仓储层**：`SearchByFilter` / `SearchFacetsByFilter` 与新增的 suggest 查询改用 `SelectContext` / `GetContext`（`persistence.DB` 内嵌 `*sqlx.DB`，见 [db.go](apps/server/internal/persistence/db.go#L19-L21)，ctx 版本可直接用）。**本批只给搜索链路加 ctx**，其余读写路径不动（缩小回归面）。

**超时后的行为与错误码**：

- 新增哨兵 `ErrSearchTimeout`，且 `errors.Is(err, domain.ErrSearchTimeout)` 可判定；`context.DeadlineExceeded` 在域层归一为它。
- 映射为 **HTTP 504 + 错误码 `search_timeout`**。既有 504 只有 `upstream_timeout`（[handlers.go](apps/server/internal/api/handlers.go#L345-L348)），语义是「上游不畅」，与「本地查询超预算」不同，故新增独立错误码而不复用——复用会让用户与运维分不清该查上游还是查索引。
- **npm 协议侧不改错误契约**：`registrySearch` 命中超时后按既有容错口径**跳过该成员继续**（现有 [npm_registry.go](apps/server/internal/protocol/npm_registry.go#L334-L336) 已有 `continue`），但同时记一条中文 warn 日志（含成员仓库名与超时值），避免「静默少结果」完全无痕。

**可配置上限**：环境变量 **`JIAN_SEARCH_TIMEOUT_SECONDS`**（单位秒，后缀风格对齐 `JIAN_AUTO_BLOCK_BASE_SECONDS` / `JIAN_UPSTREAM_TIMEOUT`），默认 **10 秒**。

- 缺省 / 非数字 / ≤ 0 一律回落默认值，**不提供「0 = 禁用」**：搜索是无界查询，关闭超时等于开放一个可以无限占用连接的入口；这与 `JIAN_STORAGE_CLEANUP_INTERVAL` 的「0 = 禁用」形态不同，理由必须写进配置注释。
- 默认 10s 的理由：P95 < 500ms 是**正常态指标**，超时上限是**病态兜底**，两者不是同一个数字；上限取太小会误伤首次 FTS 冷启动与宽泛词查询，取太大则失去保护意义。

### 3.5 ACL 批量判定与仓库名批量映射

**目标**：把「每次搜索 `O(N)` 条 ACL 查询 + `O(M)` 条仓库查询」压成常数条。

- 新增 `AclRepo.RepoIDsWithPermission(repoIDs []int64, subject Subject, action string) (map[int64]bool, error)`：**固定两条 SQL**——用户分支一条、组分支一条，均以 `repository_id IN (...)` 与 `sqlx.In` 展开，Go 侧取并集。组为空时整条跳过（与 `HasPermission` 同口径）；匿名主体（`UserID == 0`）不属于任何组，行为与现状一致。
- **必须复用 `satisfyingGrants(action)`**（[acl_repo.go](apps/server/internal/repository/acl_repo.go#L84-L101)）：蕴含关系（`read` ← `read`/`write`/`admin` 等）若有第二份实现，两条路径会在「只授 write 能否读」这类情形上悄悄分叉——这是权限语义分叉，比性能回归严重得多。因此批量方法复用同一个动作展开函数，并由「结果必须与逐仓库 `HasPermission` 完全一致」的测试钉住。
- 调用点改写（[repository_service.go](apps/server/internal/domain/repository_service.go#L1053-L1071)）：`List(1000,0)` 拿到候选私有仓库后**一次**批量判定，把命中的 id 追加进 `repoIDs`。查询数从最多 `2N` 降为 `2`。
- **顺带消除仓库名 N+1**：`repoName()`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1138-L1147)）改为先用 `ListPublic()` + `List(1000,0)` 已取到的结果**一次遍历**建 `id → name` 映射，`repoName` 先查映射、未命中才回落 `GetByID`（防御「两次查询之间仓库被删」的窗口）。返回值口径不变（查不到仍为空串）。
- **一个都不改的部分**：`facetIDs` 忽略 `repo:` 正向限定的语义（让钻取后仍能看到其他仓库的命中数）、匿名主体映射、`repoScope` 单仓库的读权限校验（handler 层 [repository_handlers.go](apps/server/internal/api/repository_handlers.go#L889-L911)）、管理员的「搜全部」（`repoIDs = nil`）。
- **已知边界，本批不动**：`List(1000, 0)` 的 1000 条硬上限意味着仓库数超过 1000 时搜索范围被静默截断。这是既有行为，改它会牵动分页语义与内存模型，登记为 §6 待定。

### 3.6 npm `registrySearch` 的收口

- 语义零变更：仍按成员循环、仍 `limit = from+size`、仍按 `asset.path` 去重、仍跳过非 packument、`total` 口径不变。
- 收益：每个成员调用内部的三条语句由全扫变候选集；`ctx` 贯通后单个成员超时不再拖住整个请求。
- 不做：不合并成员循环、不改 `from+size` 的上限口径（见 §2.3）。

### 3.7 与 FR-153 的共用衔接点（只列衔接，细节见 §3.8）

三处**必须**共用、不得各写一份：

1. **同一张 FTS 表**与同一套匹配串构造（含 FTS5 字面量转义）；
2. **同一个允许仓库集合判定**（§3.5 的批量 ACL），suggest 不新增第三套权限逻辑；
3. **同一个超时上限**（§3.4），suggest 也受 `JIAN_SEARCH_TIMEOUT_SECONDS` 约束。

### 3.8 FR-153：suggest 端点

**路径**：`GET /api/v1/search/suggest`。与 `/api/v1/search` 同组同先例，作为**非契约端点**注册（理由：它只服务前端搜索框，形状会随 FR-150 的包坐标解析演进；避免为高度易变的内部形状引入契约面）。若评审要求入契约，则连同 `api/openapi.yaml`、生成物、devmock 一并交付，不得只改一侧。

**参数**：

| 参数    | 约定                                                                                                                   |
| ------- | ---------------------------------------------------------------------------------------------------------------------- |
| `q`     | 必填语义，但**trim 后为空时返回空 `items` 而不是 400**——suggest 是高频被动调用，空输入是常态而非错误                     |
| `limit` | 缺省 10，夹取到 `[1, 20]`；超限**夹取而不报错**，对齐 `parseSearchPageSize` 的既有修正口径（[repository_handlers.go](apps/server/internal/api/repository_handlers.go#L951-L968)） |

**响应**：`{"items": [{"type": "package"|"repository", "value": "..."}]}`。

- **不带命中计数**：为每条候选再算一次 `COUNT` 会把「一次轻量查询」变回「多次聚合」，与 suggest 的目的相悖。
- **仓库名候选优先占位**：它们更短、且用户常用来做 `repo:` 限定；剩余名额给包名候选。两类合计不超过 `limit`。

**两类候选的来源**：

- **仓库名**：仓库数在百级（远小于制品数），直接对 `repository.name` 做**子串**匹配（`strings.Contains`，ASCII 大小写不敏感）后套 ACL 过滤即可，不需要 FTS。取子串而非前缀，是因为仓库名多为 `maven-releases` / `npm-hosted` 这类「前缀是类型、后缀是用途」的命名——只做前缀会让用户输入 `rele` 找不到 `maven-releases`。仓库数在百级，子串遍历与前缀匹配的成本等价。注意 `repository.name` 是 `UNIQUE` 但排序规则为 BINARY（[0001_init.sql](apps/server/internal/persistence/migrations/0001_init.sql#L40-L49)），因此这条仍是小表扫——在该量级可接受，不为它建索引。
- **包名**：对 FTS 命中行的 `asset.path` 抽取包坐标。**抽取规则只有一个来源**：与 FR-150 共用同一个解析函数（即 [search-package-aggregation.md](search-package-aggregation.md) 定义的包坐标解析），FR-153 不自建第二套规则。在 FR-150 落地前以「取路径的首段目录组合」作为**可替换的内部实现**——对外契约只承诺「字符串候选」，不承诺抽取算法，因此可被 FR-150 的解析器无痛替换。

**权限（硬约束，验收单列）**：`isAdmin` 搜全部；否则候选必须落在「public ∪ 主体被授 `read`」的仓库集合内，**复用 §3.5 的批量判定**。

- 允许集合为空时返回**空 `items`**，不返回 403：候选为空对前端等价于「没有建议」，返回 403 反而会把「这里存在一个你看不到的仓库」作为信号泄漏出去。
- **不做服务端缓存**：`Repository` 授权的既有承诺是「组内成员变动即时生效」（见 [API.md](../API.md) 用户/组段落），缓存会把这条承诺打折扣。

**稳定序**：按「候选值长度升序 → 字典序升序」，去重后截断。候选顺序不得依赖 FTS 内部回表顺序（否则同一输入在不同时刻会抖动，键盘选择的落点也随之漂移）。

### 3.9 FR-153：前端交互

落点在 `SearchPage` 的搜索输入框（[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L211-L218)）。`Header` 搜索栏是否同样接入留待后续（§6）。

- **触发条件**：`input` 变化且去空白后长度 ≥ 2 才发请求（1 字符会返回噪声候选）；**防抖 ≤ 300ms**；按修饰键、方向键、Tab 等不改变文本的按键不触发。
- **请求管理**：每次发起前中止上一次（`AbortController`），**旧响应不得覆盖新响应**。注意这保护的是 suggest 自身；结果区的进度与取消仍属 FR-152，本批不碰。
- **下拉交互**：默认收起；`↑` / `↓` 在候选间移动并高亮；`Enter` 确认；`Esc` 与点击外部收起；**无候选时不弹空面板**（不渲染「无结果」占位行）。
- **确认语义**：
  - 包名候选 → 写入输入框并**直接发起搜索**（对齐既有 `handleSubmit`，[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L174-L179)）；
  - 仓库名候选 → 复用既有表达式拼装 `buildSearchExpression({...expr, repos: [name]})` 写回输入框（[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L164-L166)），**不立即搜索**——用户可以继续追加关键词，这是既有「筛选面板 ↔ 输入框双向同步」的同一套机制，不新造状态。
- **不触发搜索**：仅键入不刷新结果区（PRD 明确不做输入即搜）。
- **无障碍**：`role="listbox"` / `role="option"`、`aria-expanded`、`aria-activedescendant` 随高亮项移动。
- **i18n**：中英双语键集一致（沿用编译期同键约束与 i18n 键集测试）。

## 4. 任务拆分

**FR-149**

- [ ] 迁移 `0049_asset_path_fts.sql`：FTS5 虚拟表（`tokenize='trigram'`）+ 索引写入同步机制 + `search_index_state` 建表与初始行；文件头五行说明与「不回填」理由
- [ ] 启动自检：FTS5 不可用时给出可读中文错误
- [ ] 后台回填：分批 / 上界快照 / 同事务推进 `last_id` / 让出与优雅停机 / 幂等写 / 节流日志
- [ ] 回填可观测与重建：`GET /api/v1/search/index`、`POST /api/v1/search/index/rebuild`（202 / 409 / 403）
- [ ] 查询改写：`buildSearchWhere` 追加候选集条件（`id IN (SELECT rowid FROM asset_path_fts WHERE … MATCH ?)`）+ FTS5 字面量转义 + 降级判定函数（收敛在一处）
- [ ] 超时：`SearchAssets` 接收 `ctx`、`ErrSearchTimeout`、`search_timeout` 映射、`JIAN_SEARCH_TIMEOUT_SECONDS` 解析与默认值、仓储层改 `GetContext` / `SelectContext`
- [ ] ACL 批量化：`RepoIDsWithPermission` + 调用点改写 + `repoName` 映射改造
- [ ] npm `registrySearch`：`ctx` 贯通 + 超时跳过并记 warn 日志（语义与响应结构不变）
- [ ] 等价性测试基座：把旧 `buildSearchWhere` 保留为 oracle，表驱动对比「旧实现 vs 新实现」的结果集与 `total`

**FR-153**

- [ ] suggest 端点：包名 + 仓库名两类候选、`limit` 夹取、空 `q` 空返回、稳定序、复用批量 ACL
- [ ] 前端下拉：防抖 ≤ 300ms、`AbortController` 乱序保护、键盘 ↑↓/Enter/Esc、无候选不弹、无障碍属性
- [ ] 仓库名候选回填走 `buildSearchExpression`（复用既有表达式机制）
- [ ] 中英文案键集

**文档同步**

- [ ] PRD 状态行（FR-149 / FR-153）、`docs/specs/README.md` 索引、`docs/OPERATIONS.md`（新环境变量、回填与重建运维步骤）、`docs/API.md`（suggest 与两个索引端点、`search_timeout`）、`CHANGELOG`
- [ ] ADR-0030（[0030-fts5-search-index.md](../adr/0030-fts5-search-index.md)）与本规格交叉引用一致

## 5. 验收标准

**语义等价与索引正确性（自动化）**

1. **双实现对照**：同一批查询串在「旧实现（oracle）」与「新实现」下，命中的 `(repository_id, path)` 集合与 `total` **完全一致**；语料至少覆盖：单词、多词 AND、`-排除词`、`ext:`、`-ext:`、`repo:`、`-repo:`、`format:`、`-format:`、引号短语（含内部空格）、大小写混合、纯 `ext:`、纯 `-词`、纯 `repo:`、1 字符词、2 字符词、2 个汉字词、以及含 `%`、`_`、`\`、`"`、`*`、`(`、`)`、`OR`、`NEAR`、`^` 的输入。
2. **FTS 语法隔离**：`q = "a OR b"` 被当作「包含子串 `a OR b`」，不是布尔查询；`a*`、`(a)`、`"a` 等均按字面量处理，无 500 错误。
3. **降级不漏**：1–2 字符词与「无包含词」的查询走降级路径且结果正确；降级判定函数的表驱动用例逐条通过。
4. **索引一致性**：asset **新增 / 覆盖写（同 `repository_id + path` 的 UPDATE）/ 删除**后，FTS 命中集合与 `path LIKE` 结果一致；删除后无孤儿 rowid；重复写同一 rowid 不产生重复结果。
5. **回填可续**：跑到中途终止进程 → 重启后从 `last_id` 继续 → 最终 `ready=true` 且全量抽样一致；回填与在线写入交叠的用例（同一行先被覆盖写、随后被回填）结果不重复不遗漏。

**性能相关（自动化可断言的部分）**

6. `EXPLAIN QUERY PLAN` 断言：三条语句的计划中出现 FTS 虚拟表访问，且在无其他选择性谓词时**不出现** `SCAN asset`（作为回归断言，防未来改动退化）。
7. **快照测试**：固定语料下三条语句的扫描行数（`sqlite3_stmt_status` 或等价探针）随 `asset` 行数增长保持常数级，不随库变大线性增长。

**超时与权限（自动化）**

8. 注入极小超时 → `/api/v1/search` 返回 **504 + `search_timeout`**；npm `registrySearch` 跳过超时成员继续返回 200 并记 warn 日志；`JIAN_SEARCH_TIMEOUT_SECONDS` 缺省 / 非数字 / ≤ 0 均回落默认值。
9. 批量 ACL 判定与逐仓库 `HasPermission` **结果完全一致**（覆盖：用户直授、仅组授、多组、无组、`admin`、匿名主体、`write` 抵扣 `read`）；并断言「一次搜索的 ACL 查询数不随仓库数增长」。
10. 既有 `/api/v1/search` 的响应结构、字段、排序键白名单与 `page_size` 夹取口径无变化。

**suggest（自动化）**

11. **权限不泄漏**：无 `read` 权限仓库下的包名与仓库名**一律不出现**在候选中；允许集合为空时返回空 `items` 而非 403。
12. 空 `q` 返回空 `items`；`limit` 夹取到 `[1, 20]`；候选去重；同一输入多次调用顺序稳定。
13. 前端：连续键入只发一次请求（防抖生效）；慢响应后到不覆盖新结果；无候选不渲染面板；↑↓/Enter/Esc 行为符合约定；键入**不**触发结果区请求。

**实机验收（需用户确认通过，手动步骤）**

14. **百万级真实数据 P95 < 500ms（实机·须用户确认）**：在真实实例、真实百万级 `asset` 上，用固定查询集（版本号如 `1.17.12`、包名片段、路径片段、`ext:` 与 `repo:` 组合各若干条）各重复 20 次取 P95，报告冷启动后稳定态数值；单元测试、合成数据与快照测试**不替代**本项。
15. `EXPLAIN QUERY PLAN` 在真实库上显示三条语句命中 FTS 而非 `SCAN asset`（贴出计划原文）。
16. **回填期间搜索可用（实机·须用户确认）**：从 `0046` 库直接升级，启动到 `/readyz` 就绪的时间与回填无关（不被回填阻塞）；回填进行中发起搜索可正常返回结果（走降级路径），`GET /api/v1/search/index` 显示 `ready=false` 与递增的进度；回填完成后同一查询结果集不变。
17. **既有搜索语法行为不回退（实机·须用户确认）**：逐条人工抽验 `-排除` / `repo:` / `format:` / `ext:` / 引号短语 / `%` 等特殊字符，结果与升级前一致。
18. `POST /api/v1/search/index/rebuild` 实测：202 受理、运行中再触发 409、非管理员 403；重建期间搜索仍可用；重建完成后结果与重建前一致。

## 6. 风险 / 待定

- **索引体积**：`path` 的 trigram 索引行数约为字符数级别，百万级路径可能带来可观磁盘增量。需在百万级实例上实测「索引体积 / 原表体积」比值，并在 `docs/OPERATIONS.md` 给出磁盘预留建议；若比值不可接受，退路是缩短索引列（例如只索引 basename）——但那会破坏「任意子串」语义，因此只能作为最后手段。
- **写放大**：每次资产写入多一次 FTS 写。发布密集场景（大版本批量上传）的 TPS 影响需实测；若明显，考虑把索引更新合并到操作引擎的批提交边界内（前置条件是操作引擎的提交粒度足够粗）。
- **宽泛词仍慢**：`q = "jar"` 或单字符版本号这类候选集接近全表的查询，索引帮不上忙。ADR-0030 实测已记录该现象——`%artifact%` 命中全部 5 万行时 trigram 约 141ms、逐行扫描约 22ms，索引版**反而更慢**。这类查询的语义本就要求遍历近全表才能给出精确 `total`，靠 §3.4 的超时兜底，**不靠索引**；PRD 的 P95 口径限定在「常见关键词与版本号」，与之相符。命中规模无法在查询前预判（预估命中率的统计代价高于收益），故不做「命中率过高则自动退回扫描」的自适应分支。
- **`ORDER BY` 仍是临时 B-Tree**：`name` 键是表达式，无法索引；`repo` 键是相关子查询。本批只把输入规模降下来，不解决「排序走索引」。若未来要求窄候选集下也零排序开销，需要物化 basename / 仓库名到列（属新 Schema 议题，另立增量）。
- **深翻页**：`OFFSET` 在候选集内仍线性跳过；真出现深翻页需求应改键集分页（cursor），属独立议题。
- **回填期间不提速**是刻意取舍（§3.2）。风险是「升级完以为立刻变快」的预期落差：必须靠日志、状态端点与 `docs/OPERATIONS.md` 三处明示，并在此规格留痕。
- **`List(1000, 0)` 的 1000 仓库上限**：既有行为，本批不改（改动牵动分页语义与内存模型）。仓库数接近或超过 1000 的部署，搜索结果会被静默截断——这是**既有缺陷**，登记待定，需另立增量改为分页遍历。
- **包名抽取规则归属 FR-150**：FR-153 的包名候选依赖包坐标解析，而该解析的正式规则由 FR-150 定义。本规格用「可替换内部实现」隔离，但若 FR-150 的解析口径与本批过渡实现差异较大，suggest 的候选形态会随之变化（契约只承诺字符串候选，不承诺算法）。
- **suggest 的请求放大**：每次键入（去抖后）一次请求。是否需要网关层限流未定；本批不加缓存（与授权即时生效承诺冲突），若线上出现压力需在网关/中间件层解决。
- **suggest 是否入契约（`api/openapi.yaml`）待定**：本规格按 `/api/v1/search` 的非契约先例处理。若评审要求入契约，需连同生成物与 devmock 一并交付。
- **`Header` 搜索栏是否接入 suggest**：本批只落 `SearchPage`。Header 若已存在搜索入口，其接入需要单独的交互设计（全局可用性与权限态差异），登记待定。
- **FTS5 与 SQLite 构建耦合**：`modernc.org/sqlite v1.60.1` 实测支持 fts5 + trigram，但这是构建期能力。已要求启动自检给出可读错误；若未来更换 SQLite 驱动或启用 CGO 构建，需复验 `tokenize='trigram'` 行为一致（尤其 3 字符下限与大小写折叠）。
- **默认排序不变**：本批保持 `path asc` 默认值。FR-150 会把默认排序改为 `order=relevance`，两个 FR 合并时必须显式处理「默认值切换」的兼容窗口，不要让用户在两次升级之间感受到默认行为跳变。
