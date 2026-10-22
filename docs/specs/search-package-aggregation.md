# 功能规格：搜索结果包级聚合与主从双栏视图（FR-150 / FR-151）

> 状态：**已交付@v0.12.0** · 2026-10-19 · 关联 PRD §4 FR-150 / FR-151 · FR-150 **需 spec**（新增响应结构与排序语义）· FR-151 **免 spec**（纯前端视图重排，不新增 Schema / 端点 / 迁移）

## 1. 背景与目标

**现状**：全局搜索（FR-30）是一条**文件级**链路。后端 `SearchByFilter`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L307)）按 `path LIKE '%词%'` 返回扁平的 `asset` 行，`SearchFacetsByFilter`（[asset_repo.go](apps/server/internal/repository/asset_repo.go#L330)）只按 `repository_id` 聚合；服务层 `SearchAssets`（[repository_service.go](apps/server/internal/domain/repository_service.go#L1021)）只补仓库名映射，不做任何坐标或版本维度的归并。前端结果页（[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L366)）把结果铺成一张扁平 Table，默认按 `updated desc` 排序（[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L71)）。

**问题**：用户搜一个版本号 `1.17.12`，命中集里混进两类噪声——

- 语义相邻但不同的版本：`1.17.12-rc1`、`1.17.12-SNAPSHOT`、`1.17.12.1`；
- **纯子串巧合**：`1.17.120` 同样包含 `1.17.12`。

`asset` 表没有 version 列（[0002_asset.sql](apps/server/internal/persistence/migrations/0002_asset.sql#L13-L23)），包坐标与版本**只存在于 path 文本**里，因此排序只能按 updated/size/path，无法表达"哪个才是用户真正要的那个版本"。结果是精确版本会被同页的 `1.17.120`、`-sources.jar`、`.sha1` 一起淹没，用户需要肉眼二次过滤。同时，同一个包的 8 个文件（jar/pom/sources/javadoc/校验和）在平铺视图里各占一行，一页 50 条（[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L54)）经常只够放下六七个包。

**目标**：

1. **FR-150** 让结果按包坐标聚合、按命中精度分层排序，把"用户要的那个版本"顶到最前；新增 `version:` 严格语法与 `order=relevance` 默认排序。
2. **FR-151** 把结果页改成**主从双栏 + 包级折叠树**：左栏"包 → 版本 → 文件类型"，右栏选中项详情，让"同包多版本"在一屏内可比、可展开。

**归属阶段**：P2（与 FR-149 搜索索引改造同属搜索域的可读性收口，互不阻塞）。

## 2. 需求（要什么）

### 2.1 FR-150（需 spec）

- 搜索结果按**包坐标**聚合：同包多版本折叠为一组，组内按版本拆分，版本下挂文件。
- 命中精度**三层分层**：精确版本 > 预发布 > 子串；`1.17.12` 必须排在 `1.17.12-rc1` 之前，且二者都排在 `1.17.120` 之前。
- 新增 `order=relevance`，并作为**搜索结果页的默认排序**；`name` / `repo` / `path` / `size` / `updated` 全部保留可用。
- 新增 `version:` 严格匹配语法：`version:1.17.12` 只返回**版本严格相等**的制品，不返回 `1.17.120` / `1.17.12-rc1`。
- 无法解析包坐标的**裸路径不丢失**：降级为文件级条目（归入「无坐标」伪包），仍可搜索、可下载。
- npm 协议搜索 `GET /-/v1/search`（[npm_registry.go](apps/server/internal/protocol/npm_registry.go#L299)）的对外响应**零变化**。

### 2.2 FR-151（免 spec）

- 结果视图改为**主从双栏**：左栏包级折叠树，右栏选中项详情。
- 左栏行信息：包行（包名 / 协议徽标 / 版本数 / 文件数）、版本行（版本号 / 精度徽标 / 文件类型 chip / 大小）。
- 右栏详情：文件名、包坐标:版本、仓库、大小、修改时间、SHA1、下载、打开仓库、同版本文件列表。
- 默认展开首个命中包、默认选中首个命中版本。
- 命中词高亮（包坐标 / 版本号 / 文件名）。
- 窄屏（宽度 < 820px）降维单栏，可进入详情并返回。
- 四态齐备：空结果 / 无权限 / 加载中 / 失败。

> **为什么 FR-151 免 spec**：它只重排既有数据的呈现——不新增表、不新增端点、不新增响应字段（字段由 FR-150 提供），不改变任何对外契约。因此本 FR 的设计按"可直接照做的界面规格"写（§3.9–§3.11），验收全部可自动化或按步骤手动复验，不再单独立 spec 文件。

### 2.3 范围外（明确不做）

- **不做虚拟滚动**：单次聚合最多渲染 5000 条文件（§3.7 预算），左栏包/版本节点数在常规查询下是个位到百级；一次性渲染 + 折叠即可。引入虚拟滚动会与折叠展开、键盘焦点管理冲突，收益不成比例。若实测到明显卡顿，再单独立项。
- **不做输入即搜**（debounce 自动查询）：沿用现有"回车 / 点搜索按钮提交"，避免每次击键触发聚合扫描。
- **不改仓库内搜索的呈现**：浏览页树内搜索（`RepoBrowser`，[RepoBrowser.tsx](apps/web/src/components/repo/RepoBrowser.tsx#L338)）继续用 `buildAssetTree` 拼目录树，不接包聚合。
- **不做坐标推断启发式**：不猜测 raw 仓库里的 Maven 布局（§3.2 格式门控的取舍见 §6）。
- **不做版本号排序的严格 semver 语义**：只做"足够好"的数字向量比较（§3.5）。
- **不做聚合结果导出 / 批量下载 / 包级删除**：属资产操作域，另立 FR。

## 3. 设计（怎么做）

### 3.1 改动面总览

| 层         | 文件                                                                                                                          | 改动                                                                                                                                                                                                                   |
| ---------- | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 表达式解析 | `apps/server/internal/domain/search_query.go`                                                                                 | `parsedSearch` 新增 `versions` / `notVersions`（[search_query.go](apps/server/internal/domain/search_query.go#L21)、[parseSearchQuery](apps/server/internal/domain/search_query.go#L59)）                              |
| 条件构造   | `apps/server/internal/repository/asset_repo.go`                                                                               | `SearchFilter` 新增 `IncludeVersions` / `ExcludeVersions`（[L236](apps/server/internal/repository/asset_repo.go#L236)），`buildSearchWhere` 拼段边界条件（[L244](apps/server/internal/repository/asset_repo.go#L244)） |
| 候选集     | `apps/server/internal/repository/asset_repo.go`                                                                               | 新增 `SearchAllByFilter`（无分页、带上限，复用 `buildSearchWhere`）                                                                                                                                                    |
| 聚合       | **新增** `apps/server/internal/domain/search_package.go`                                                                      | 纯函数：坐标解析 `ParseCoordinates`、精度判定 `MatchPrecision`、分组 `BuildPackageGroups`、相关度比较器                                                                                                                |
| 服务       | `apps/server/internal/domain/repository_service.go`                                                                           | 新增 `SearchAggregate`（[SearchOutput](apps/server/internal/domain/repository_service.go#L1007) 扩展 `Packages`）                                                                                                      |
| HTTP       | `apps/server/internal/api/repository_handlers.go`                                                                             | `SearchAssets` 解析 `group_by` / `order=relevance`，输出 `packages`（[L849](apps/server/internal/api/repository_handlers.go#L849)）                                                                                    |
| 前端数据   | `apps/web/src/api/endpoints.ts`                                                                                               | `SearchResult` 类型扩展（[L1224](apps/web/src/api/endpoints.ts#L1224)）                                                                                                                                                |
| 前端解析   | `apps/web/src/lib/searchQuery.ts`                                                                                             | `SearchExpression` 新增 `versions` / `notVersions`，保持面板双向同步不丢 token                                                                                                                                         |
| 前端视图   | **新增** `apps/web/src/components/search/PackageSearchTree.tsx`、`SearchDetailPanel.tsx`、`apps/web/src/lib/searchPackage.ts` | 树、详情、树构建与高亮切分                                                                                                                                                                                             |
| 前端页     | `apps/web/src/pages/SearchPage.tsx`                                                                                           | 替换结果区，默认排序改 relevance                                                                                                                                                                                       |
| i18n       | `apps/web/src/i18n/zh.ts` / `en.ts`                                                                                           | `search.*` 新键（受 [I18nKeys.test.ts](apps/web/src/i18n/I18nKeys.test.ts) 守卫）                                                                                                                                      |

**无 migration**：坐标与版本是 path 的派生量，聚合在 Go 侧内存完成，不新增列/表/索引。FR-149 落地后若引入规范化列，`BuildPackageGroups` 的输入可平滑替换（§6）。

### 3.2 包坐标解析规则

**门控原则**：只有能确定布局的格式才解析。仓库的 `format` 由 `repository.Format`（[models.go](apps/server/internal/repository/models.go#L41)）提供，服务层在构建 `repoID → {name, format}` 映射时一并取到（复用既有 `s.repos.GetByID` 缓存，[repository_service.go](apps/server/internal/domain/repository_service.go#L1141)）。

| 仓库 format      | 解析            | 结果                           |
| ---------------- | --------------- | ------------------------------ |
| `maven`          | 启用 Maven 规则 | 有坐标或降级                   |
| `npm`            | 启用 npm 规则   | 有坐标或降级                   |
| 其它（`raw` 等） | **不解析**      | 一律降级为「无坐标」文件级条目 |

#### Maven：`group/artifact/version/file`

把 path 按 `/` 切段为 `S[0..n-1]`，`S[n-1]` 为文件名。

- 判定条件：`n >= 4` **且** `S[n-2]`（版本目录段）匹配 `^[0-9]`。
- 解析：`group = S[0..n-3]` 以 `.` 连接；`artifact = S[n-3]`；`version = S[n-2]`；`展示坐标 = group:artifact`。
- 不满足条件（如 `n == 3`，即 `group/artifact/file` 这种缺版本目录的扁平发布）→ **降级**为无坐标文件级条目。
- 仅看末三段，因此深层 group（`com/example/sub/module/...`）无需特判。

#### npm

把 path 按 `/` 切段为 `S`。

- **scoped**（`S[0]` 以 `@` 起首）：
  - 包名 = `S[0]/S[1]`。`n == 2` → 该路径就是 packument（包根），版本为空。
  - `S[2]` 匹配 `^[0-9]` → `version = S[2]`。
- **unscoped**：
  - 包名 = `S[0]`。`n == 1` → packument，版本为空。
  - `S[1] == "-"` → tarball 形态 `pkg/-/pkg-<version>.tgz`：从文件名剥离前缀 `<pkg>-` 与后缀 `.tgz` 得到版本；剥离失败则版本为空（归包根）。
  - 否则 `S[1]` 匹配 `^[0-9]` → `version = S[1]`。
- 版本为空时该文件挂在包的「未版本化」组（徽标位留空），不丢。

#### 裸路径（降级）

无坐标条目 `kind = "file"`：`coordinate` 显示为 `（无坐标）`，按**仓库**归为伪包节点（每个仓库一个，避免每个路径都成一个节点把树冲垮），版本层跳过，文件直接挂在伪包下。仓库名本身就是这些条目的唯一分组维度，因此徽标显示仓库名（§3.9）。

### 3.3 聚合口径

| 概念                     | 定义                                                                                                                                                               |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **包**                   | 身份键 = `coordinate`（**不含仓库**）。同一坐标出现在多个仓库时合并为一个包节点，`repositories` 记录涉及仓库；文件行保留各自 `repository` 以消歧（理由见 §6 待定） |
| **版本**                 | 包内身份键 = 解析出的 `version` 字符串（ASCII 大小写不敏感归并，展示取首次出现的原文）；空版本 = 「未版本化」组                                                    |
| **文件**                 | 一个 `asset` 行。`(repository, path)` 唯一，因此同一文件不会重复计入                                                                                               |
| **文件类型（fileKind）** | 由文件名后缀 + Maven classifier 归类，封闭集合，见附录 A                                                                                                           |

**计数**：`package.versionCount` = 版本组数（不含空版本组）；`package.fileCount` = 组内文件总数（含校验和文件）；`version.fileCount` = 该版本文件数。所有计数含被 UI 默认折叠的校验和文件——计数口径与「展开了能看到几个文件」一致。

### 3.4 命中精度分层（核心规则）

**判定输入**：用户表达式里的**普通关键词**（`IncludeTerms`，即 `-` 排除词 / `repo:` / `format:` / `ext:` / `version:` 之外的 token，含引号短语去引号后的内容）与文件的**版本段** `V`。命中词未落在 `V` 上时，该查询对这份文件不构成版本相关度信号。

**比较**：ASCII 大小写不敏感（与 SQLite `LIKE` 的默认行为一致，避免「SQL 命中但 Go 判定不命中」的错位）。

对每个关键词 `T` 与 `V`，按下表自上而下取**首个**命中项；多个关键词时取**编号最小**者（最有利精度）：

| 编号 | 标记                   | 判定条件                                                        | 例（`T = 1.17.12`）                                                                                  |
| ---- | ---------------------- | --------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| 0    | `exact`（精确）        | `V` 与 `T` 完全相等                                             | `V = 1.17.12`                                                                                        |
| 1    | `prerelease`（预发布） | `V` 以 `T` 起首，且余下部分 `R = V[len(T):]` 以 `-` 或 `+` 起首 | `1.17.12-rc1`、`1.17.12-SNAPSHOT`、`1.17.12-alpha.3`、`1.17.12+build.7`、`1.17.12-20240101.123456-3` |
| 2    | `substring`（子串）    | `V` 含 `T`，但不满足 0 / 1                                      | `1.17.120`（`R = 0`）、`1.17.12.1`（`R = .1`）、`11.17.12`                                           |
| 3    | `none`（无信号）       | `V` 不含 `T`，或文件无坐标                                      | 搜 `foo` 时的全部命中；裸路径条目                                                                    |

**为什么是结构判定而不是后缀白名单**：`-` / `+` 起首已经覆盖 Maven 与 semver 的全部预发布形态——`-rc1`、`-SNAPSHOT`、`-alpha`、`-beta`、`-M2`、`-milestone`、`-preview`、`-ea`、`-dev`、时间戳快照 `-20240101.123456-3`、以及 `+build` 构建元数据。封闭名单必然漏项（如新兴的 `-canary.N`），且每加一个都要改代码。结构判定的语义是"命中串是版本前缀，后面还有额外的标记段"，这正是"同一版本线的预发布/非正式发布"的定义。

**包的精度** = 组内所有版本精度的**最小值**（`bestPrecision`）。

**版本精度只由版本段决定**，不看文件名。理由：Maven 的文件名里同样带版本（`foo-1.17.12.jar`），若一并计算，`1.17.120` 的目录下的文件就不会被判为子串——而目录段才是包坐标的权威来源。

### 3.5 `order=relevance` 排序权重

**排序位置**：`relevance` 的排序在 **Go 侧**完成（聚合阶段已持有全部候选行，不额外查询），因此不需要在 SQL 里表达精度。其余 `sort` 键继续下推 SQL（[searchOrderBy](apps/server/internal/repository/asset_repo.go#L282) 不动）。

**前提**：`relevance` 需要全量候选集才能算准组内最佳精度，因此与 `group_by=package` 绑定使用（§3.7）。`group_by=file`（默认）下收到 `order=relevance` 会**静默回落** `path asc`——与既有"非法排序值静默回落"的口径一致，响应里用 `appliedOrder` 回报实际生效值，前端不猜。

**比较器（包级，字典序，逐键比较至分出胜负）**：

1. `bestPrecisionRank` **升序**（0 精确 → 1 预发布 → 2 子串 → 3 无信号）
2. `topVersionKey` **降序**（版本向量，见下）；**仅当 `bestPrecisionRank < 3` 时参与比较**，否则跳过
3. `latestUpdatedAt` **降序**（组内最新 `updated_at`，ISO 串直接比大小）
4. `coordinate` **升序**（稳定兜底）

第 2 键是"同精度下新版本优先"；第 3 键承接"没有任何版本信号时，新的改动在前"——这恰好等于今天的默认排序 `updated desc`，因此**没有版本信号的查询（绝大多数关键词搜索）在 relevance 下的体验与现状一致**，不会因为改默认值而让用户困惑。第 4 键保证分页稳定、可复现。

**版本向量 `topVersionKey`**：把版本串按 `.` / `-` / `+` 切分为若干段，遇到非数字段即停止；得到的数字数组逐位数值比较（缺失位视为 0）。非数字版本串（如 `latest`）向量为空，此时 `topVersionKey` 等价于全 0，退到键 3 比较。

**组内排序（版本行）**：同样先按 `precision` 升序，再按版本向量降序，最后版本串升序。

**组内文件排序**：`fileKindPriority` 升序（附录 A 的优先级列），再 `path` 升序——即"主构件在最前，校验和垫底"。

**示例（`q=1.17.12`，同包 `com.example:foo` 三个版本全部命中）**：
`1.17.12`（rank 0）→ `1.17.12-rc1`（rank 1）→ `1.17.120`（rank 2）。**验收断言以这个顺序为准。**

### 3.6 `version:` 严格匹配语法

**形式**：

| 写法               | 语义                                                                                 |
| ------------------ | ------------------------------------------------------------------------------------ |
| `version:1.17.12`  | 版本严格等于 `1.17.12`（ASCII 大小写不敏感，`EqualFold`）；多个 `version:` 为 **OR** |
| `-version:1.17.12` | 排除该版本；多个为 AND（都不等于）                                                   |
| `version:`（空值） | 静默丢弃，与 `repo:` / `ext:` 处理一致                                               |

**与 `search_query.go` 的关系**：新增的两个字段与 `ext:` **同级**，走同一套 `tokenizeSearch` + `HasPrefix(lower, "version:")` 分支，进 `parsedSearch`（[L21](apps/server/internal/domain/search_query.go#L21)）与 `SearchFilter`。关键点：`version:` 的值**绝不能**作为子串词落进 `IncludeTerms`——否则 `1.17.12` 会被拿去子串匹配包名与文件名，失去"严格"语义。

**严格性如何落地（不下推为等值比较）**：版本在 path 文本里没有列也没有分隔约束，因此在 `buildSearchWhere` 里表达为**段边界条件**（复用 `escapeLike`）：

```
version:v  →  ( path LIKE '%/'||v||'/%' ESCAPE '\'
             OR path LIKE '%-'||v||'.tgz' ESCAPE '\' )   -- npm tarball 形态
```

- 第一支覆盖 Maven `.../1.17.12/file` 与 raw 的 `.../1.17.12/file`（**整段相等**，因此 `1.17.120/` 不命中、`1.17.12-rc1/` 不命中）；
- 第二支覆盖 npm tarball `pkg/-/pkg-1.17.12.tgz`（版本不在独立路径段里）。

**为什么不做"先松散 LIKE 再 Go 侧后置过滤"**：后置过滤会让 `total` 变成过滤后的计数，分页 `offset/limit` 也得搬到 Go 侧，`total` 与 `page` 的既有语义（[repository_handlers.go](apps/server/internal/api/repository_handlers.go#L849) 计算 `offset`，`SearchByFilter` 内 `COUNT(*)` 出精确 `total`）就全破了。段边界条件在 SQL 里**已经是严格的**，`total` 与分页天然保持精确。

**已知取舍**：path 中恰好存在一个叫 `1.17.12` 的**非版本**目录（如某个包名就叫这个）会被命中。这是 path-as-schema 的固有代价，误报率极低，不做额外辨认（§6）。

**前端同步（实测更正）**：设计的本意是让 `SearchExpression` 新增 `versions` / `notVersions`（[searchQuery.ts](apps/web/src/lib/searchQuery.ts#L5) 的接口），否则手输 `version:1.17.12` 后一动筛选面板，`buildSearchExpression` 重拼表达式会把该 token 丢掉——表现为"改了个扩展名筛选，版本约束悄悄失效"。**实现阶段核对后确认这条风险并不成立**：解析器不认识 `version:`，该 token 落进 `terms`（`-version:` 落进 `excludeTerms`），而 `buildSearchExpression` 把这些数组原样回写，所以往返不丢（实测 `-version:1.17.12 ext:jar` → 面板加 `pom` → `-version:1.17.12 ext:jar ext:pom`）。因此本批**没有**新增这两个字段：它们在没有任何面板控件使用时只是多一份需要同步的状态。代价是筛选面板没有版本输入框（只能手输），且该 token 会进高亮关键词列表（它在 `path` 里不出现，视觉上无影响）。若将来给版本加面板输入框，仍需补这两个字段并加往返测试。

**与 FR-149（FTS5 trigram 索引）的衔接**：trigram 索引本身是**子串**语义，`1.17.12` 的索引候选里必然包含 `1.17.120`——这正是必须有 §3.4 分层的原因，索引只负责把候选集取小，**不负责判定精度**。两件事互不替代：FR-149 落地后，`buildSearchWhere` 仍原样生效并于末尾追加索引子查询条件（见 [search-index-and-performance.md](search-index-and-performance.md) §3.3 的「追加一个 AND 条件」形态），本 FR 新增的 `version:` 段边界条件作为其中一个谓词自然参与；精度比较仍在 Go 侧聚合阶段完成，对「走索引」与「走全表 LIKE」两种实现等价。

### 3.7 响应结构变化

`GET /api/v1/search` 在 `group_by=package` 时新增以下字段。`group_by` 缺省为 `file`，此时**只有 `items[]` 新增一个 `sha1` 键**，其余字段名、结构与语义完全不变（`sha1` 是纯追加键，老客户端忽略即可）。

```jsonc
{
  "items": [/* 既有文件级数组，字段不变；file 模式下的唯一数据源 */],
  "total": 128, // 匹配文件总数（全量，与分页无关）—— 未变
  "facets": [/* 未变 */],

  "packageTotal": 24, // 不同包节点数（含「无坐标」伪包）；truncated 时为下界
  "packages": [
    {
      "coordinate": "com.example:foo", // 展示坐标；无坐标条目为仓库名
      "kind": "maven", // maven | npm | file
      "repositories": ["maven-releases"],
      "versionCount": 3,
      "fileCount": 12,
      "bestPrecision": "exact", // exact | prerelease | substring | none
      "latestUpdatedAt": "2026-10-18 09:12:33",
      "topVersion": "1.17.12",
      "versions": [
        {
          "version": "1.17.12",
          "precision": "exact",
          "fileCount": 4,
          "size": 1834244, // 组内文件体积合计
          "latestUpdatedAt": "2026-10-18 09:12:33",
          "files": [
            {
              "name": "foo-1.17.12.jar",
              "path": "com/example/foo/1.17.12/foo-1.17.12.jar",
              "repository": "maven-releases",
              "size": 1523712,
              "sha1": "3f2a…",
              "updatedAt": "2026-10-18 09:12:33",
              "fileKind": "jar",
            },
          ],
        },
      ],
    },
  ],
  "aggregation": { "scanned": 128, "truncated": false },
}
```

- **`total` vs 文件数**：`total` 是**匹配文件**总数（既有语义，来自 `COUNT(*)`，不受分页影响）。"文件数"出现在三个更细的层级上：每包的 `fileCount`、每版本的 `fileCount`、`packages[].versions[].files` 的长度。三者自下而上求和 = `total`（未截断时）。`packageTotal` 表达**包**的维度，与 `total` 不同量纲，不要混用。
- **`bestPrecision` / `precision`** 的字符串取值即 §3.4 的四个标记，前端据此渲染徽标与排序提示。
- **`aggregation.truncated`**：单次聚合扫描预算 `maxAggregateRows = 5000`（按 `path ASC` 确定性截断）。超出时 `truncated = true`，`packageTotal` 与各 `fileCount` 变为**下界**（UI 显示 `≥ N` 并向用户提示收窄关键词）。截断不会让 `total` 失真——`total` 仍来自 `COUNT(*)`。
- **`appliedOrder`**（`group_by=file` 且请求 relevance 回落时回报实际排序）——避免前端"以为在按相关度排"的静默错位。
- **`sha1` 为新增字段**：现在响应只给 `hash`（= `blob_hash`，[asset_repo.go](apps/server/internal/repository/asset_repo.go#L315) 已选出 `sha1` 列但未被 handler 输出），详情面板需要它（§3.9）。新增一个键属向前兼容（老客户端忽略）。

**契约同步落点**：`/api/v1/search` 目前**不在** `api/openapi.yaml` 里——handler 注释明确写「非契约端点，经 WithProtocolRoutes 注册」（[repository_handlers.go](apps/server/internal/api/repository_handlers.go#L848-L849)），全文检索 `openapi.yaml` 无 `search` 字样。因此本 FR 的同步落点是：`docs/API.md` 的搜索端点说明 + 前端 `SearchResult` 类型（[endpoints.ts](apps/web/src/api/endpoints.ts#L1224)）+ devmock。把聚合结构纳入 oapi-codegen 契约需要为嵌套坐标系建一整套 schema，收益不抵成本，列为 §6 待定。

### 3.8 兼容性

| 关注点                 | 结论                                                                                                                                           | 依据                                                                                                                                                                                                 |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `page` / `page_size`   | 语义不变：仍然对**文件级** `items` 分页；`page_size` 默认 20 / 上限 100 不变（[L952](apps/server/internal/api/repository_handlers.go#L952)）   | `group_by=package` 只增 `packages`/`packageTotal`，不改 `items` 与 `total`                                                                                                                           |
| `sort` 白名单          | 不变（`name`/`repo`/`path`/`size`/`updated`）                                                                                                  | `relevance` 走 `order`，不塞进 `sort`                                                                                                                                                                |
| `order` 缺省           | **仍是 `asc`**（`path asc`）                                                                                                                   | 关键：仓库内搜索 `RepoBrowser` 不传 `sort`/`order`，依赖 `path asc` 才能稳定建树（[RepoBrowser.tsx](apps/web/src/components/repo/RepoBrowser.tsx#L351)）。若把后端缺省改成 relevance，树会按包序抖动 |
| 搜索结果页默认排序     | 变成 relevance                                                                                                                                 | 仅前端 `DEFAULT_SORT`（[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L71)）改，且显式传 `group_by=package&order=relevance`                                                                      |
| 排序偏好持久化         | `SEARCH_SORT_KEY`（[L68](apps/web/src/pages/SearchPage.tsx#L68)）里的旧值（如 `{by:"updated",order:"desc"}`）继续合法；只在缺键/脏值时用新默认 | `sanitizeSort` 扩展：`order:"relevance"` 仅在与 `by:"relevance"` 搭配时接受                                                                                                                          |
| npm `GET /-/v1/search` | **零变化**                                                                                                                                     | 见下                                                                                                                                                                                                 |

**npm 端点不受影响的论证**（三条独立理由，任一成立即够）：

1. **路径不同**：`registrySearch`（[npm_registry.go](apps/server/internal/protocol/npm_registry.go#L299)）直接调用领域层 `SearchAssets`，**不经过** HTTP handler `SearchAssets`。聚合与 `order=relevance` 的解析都在新增的 `SearchAggregate` 与 handler 侧，领域层 `SearchAssets` 的签名与行为**一行不改**——npm 调用点因此不需要任何改动。
2. **参数显式**：npm 调用点硬编码 `SearchAssets(keyword, member, 0, true, "path", "asc", from+size, 0)`——`sort=path`、`order=asc`，永远不进 relevance 分支。
3. **响应自建**：`objects` 由 `searchObject` 从 packument 提炼，`total`/`time` 自算；即便领域层内部新增了聚合字段，npm 响应体只挑它读的 `out.Items`，多出来的字段不会外泄。

**回归保护**：现有 `apps/server/internal/api/search_page_size_test.go`、`apps/server/internal/domain/search_query_test.go` 必须全绿且不改断言（若因 `version:` 新分支必须扩展，只允许新增用例）。

### 3.9 FR-151 界面设计

**布局**：`PageShell` 内一个 `display:flex` 容器（`gap:16px`，即 `tokens.spacing.md`）。

- 左栏：`flex: 0 0 40%`，`minWidth: 320`，`maxWidth: 460`，独立竖向滚动（`overflow:auto`），表头/工具条吸顶。
- 右栏：`flex: 1`，`minWidth: 0`，独立竖向滚动。
- 沿用既有主题：主色 `#228be6`（`packages/ui/src/tokens.ts` 的 `brandColor`）、`defaultRadius: "md"`、`radius.md = 8`（`packages/ui/src/theme.ts`）。不新造颜色或圆角令牌。

**桌面（≥820px）线框**：

```
┌ 制品搜索 ───────────────────────────────────────────────────────────────────────┐
│ [搜索制品，支持 -排除词 repo: format: ext: version: ] [语法帮助] [筛选] [搜索]  │
│ (全部 128) (maven-releases 96) (npm-hosted 32)          ← 既有仓库 facet 钻取条   │
├──────────────────────────────────┬──────────────────────────────────────────────┤
│ 共 128 个文件 · 24 个包  [相关度▾] │                                              │
│                                  │  foo-1.17.12.jar                             │
│ ▾ com.example:foo        [maven] │  ┌────────────────────────────────────────┐  │
│    3 个版本 · 12 文件            │  │ 包坐标   com.example:foo               │  │
│    ▾ 1.17.12   [精确]  4 文件    │  │ 版本     1.17.12   [精确]              │  │
│        [jar] [pom] [sources]     │  │ 仓库     maven-releases                │  │
│        [javadoc]                 │  │ 路径     com/example/foo/1.17.12/…     │  │
│      ▸ 1.17.12-rc1 [预发布] 3    │  │ 大小     1.5 MB                        │  │
│      ▸ 1.17.120    [子串]   3    │  │ 修改     2026-10-18 09:12:33           │  │
│ ▸ com.example:bar        [maven] │  │ SHA1     3f2a…                         │  │
│ ▸ @acme/ui               [npm]   │  └────────────────────────────────────────┘  │
│ ▸ （无坐标）             [raw]   │  [下载] [打开仓库]                            │
│    5 个文件                      │                                              │
│                                  │  同版本文件 (4)                              │
│                                  │  ┌────────────────────────────┬───────┬────┐ │
│                                  │  │ foo-1.17.12.jar            │ 1.5MB │下载│ │
│                                  │  │ foo-1.17.12.pom            │ 12KB  │下载│ │
│                                  │  │ foo-1.17.12-sources.jar    │ 120KB │下载│ │
│                                  │  └────────────────────────────┴───────┴────┘ │
└──────────────────────────────────┴──────────────────────────────────────────────┘
```

**左栏元素**：

| 元素       | 内容                                                                                                                                  | 交互                                                                                                    |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| 包行       | 包名（`coordinate`）、协议徽标（`maven` / `npm` / `raw`，`Badge variant="light" size="xs"`）、`N 个版本 · M 文件`                     | 点击展开/折叠；展开后再点包名选中该包的首个版本                                                         |
| 版本行     | 版本号、精度徽标（`精确`/`预发布`/`子串`，无信号不显示徽标）、`N 文件`、文件类型 chip 组（按附录 A 优先级取前 4 个，其余折叠为 `+N`） | 点击选中该版本（右栏切到该版本的首个文件），**不折叠其它节点、不回退列表、不重发请求**                  |
| 文件行     | 只在版本展开时出现，`fileKind` + 文件名                                                                                               | 点击选中该文件                                                                                          |
| 校验和文件 | 该版本内 `fileKind=checksum` 的文件默认折入一行 `校验和 (3)`，点击展开                                                                | 与既有「隐藏校验和」心智一致（`CHECKSUM_EXTS`，[searchQuery.ts](apps/web/src/lib/searchQuery.ts#L104)） |

**右栏详情**：文件名（标题）、包坐标:版本、精度徽标、仓库（`Badge`）、完整路径（等宽字体、可复制）、大小（`formatBytes`）、修改时间（`formatUtcToLocal`）、SHA1（截断显示 + 复制按钮）、`下载`（`assetDownloadUrl`，新窗口）、`打开仓库`（沿用 FR-145 的 `?highlight=<path>` 直达，[SearchPage.tsx](apps/web/src/pages/SearchPage.tsx#L194)）、`同版本文件` 列表（当前版本全部文件，每行含大小与下载）。同版本文件列表里当前选中的那行高亮（背景 `var(--mantine-color-blue-0)`）。

**默认行为**：首屏 `packages[0]` 自动展开、其 `versions[0]` 自动选中、右栏显示该版本首个文件。数据变化（换关键词/换排序）后重新套用同一规则——但用户手动展开/选中过的节点在同一次数据内保持不变。

**命中词高亮**：仅在三处对命中子串加高亮——包坐标、版本号、文件名。实现为把文本切成 `[前置, 命中, 后置]` 三段并渲染 `<mark>`（背景 `var(--mantine-color-yellow-2)`，圆角 2px，不加粗）。**路径全文不高亮**（目录段命中会让整行变黄，噪声大于价值）。多个关键词各自高亮；大小写不敏感匹配，但保留原文大小写。

**排序控件**：结果计数行右侧一个 `Select`（`相关度`(默认) / `名称` / `仓库` / `路径` / `大小` / `修改时间`），与 `SEARCH_SORT_KEY` 持久化联动。选非相关度时切到文件级排序语义——此时**左栏仍按包分组**，只是包与版本内部的相对顺序改用对应键（前端在 `lib/searchPackage.ts` 里重排；不为此再发请求）。

### 3.10 窄屏（<820px）降维

断点：`useMediaQuery("(max-width: 51.25em)")`（= 820px，按 16px 根字号换算）。820 是**本页唯一的布局断点**：旧扁平表格按 48em（768px）隐藏"大小/修改时间"列的做法随表格一起退场，实现时把 `isNarrow` 的残余用途（搜索框最小宽度等）一并对齐到 820px，避免同页出现两个"窄屏"定义（§6）。

**两级视图**：

```
┌ 制品搜索 ──────────────────────────┐        ┌ ← 返回搜索结果 ──────────────────┐
│ [搜索框            ] [筛选] [搜索] │        │ foo-1.17.12.jar                  │
│ (全部 128) (maven 96) (npm 32)     │  点击  │ com.example:foo · 1.17.12 [精确] │
├────────────────────────────────────┤  版本/ │ 仓库  maven-releases             │
│ 共 128 个文件 · 24 个包            │  文件  │ 大小  1.5 MB                     │
│ ▾ com.example:foo        [maven]   │ ────▶  │ SHA1  3f2a…                      │
│    3 个版本 · 12 文件              │        │ [下载] [打开仓库]                │
│    ▸ 1.17.12   [精确]  4 文件      │        │ 同版本文件 (4) …                 │
│    ▸ 1.17.12-rc1 [预发布] 3        │        └──────────────────────────────────┘
│ ▸ com.example:bar        [maven]   │
└────────────────────────────────────┘
```

- 单栏：只显示左栏树全宽（`flex-basis: 100%`），右栏不占位。
- **进入详情**：点击版本行或文件行 → 切换到详情视图（整屏替换，保留页面头部搜索框）。
- **返回**：详情视图顶部固定一行 `← 返回搜索结果`（`Button variant="subtle" size="compact-sm"`，带 `aria-label`）+ 浏览器返回键（`popstate`）均可回到列表，**且保留原展开状态与滚动位置**。
- 详情内的"同版本文件"点了另一个文件 → 就地切换详情，不返回列表。
- 820px 断点下若用户把窗口拉宽跨过断点：双栏恢复显示，右栏显示当前选中项（选中态是视图状态，不随断点重置）。

### 3.11 四态

| 状态   | 触发                      | 呈现                                                                                                                                                                            |
| ------ | ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 加载中 | 首次查询、`data === null` | `ContentSkeleton`（[AsyncBoundary.tsx](apps/web/src/components/AsyncBoundary.tsx#L33)，双栏各自骨架：左栏 6 行、右栏 3 块）。换排序/翻页沿用 `keepPreviousData`，**不**回到骨架 |
| 空结果 | `total === 0`             | 居中 `Text c="dimmed"`：`未找到匹配的制品` + 副行提示（可尝试的语法示例），并保留搜索框与筛选条可继续操作                                                                       |
| 无权限 | `state.forbidden`         | `ForbiddenState`（`@jianartifact/ui`），仅结果区替换，搜索框保留                                                                                                                |
| 失败   | `state.error`             | `ErrorState` + `重试`（`state.reload`），错误消息用 `state.error.message`                                                                                                       |

四态都只替换"结果区"，从不整体卸载搜索框与 facet 条——否则用户无法在失败后换个词重试。

## 4. 任务拆分

### FR-150（后端）

- [x] `search_query.go`：`parsedSearch` 增加 `versions` / `notVersions`，`parseSearchQuery` 加 `version:` 分支（与 `ext:` 同级），补 `TestParseSearchQuery` 用例（含空值丢弃、`-version:`、与子串词共存）
- [x] `asset_repo.go`：`SearchFilter` 增 `IncludeVersions` / `ExcludeVersions`；`buildSearchWhere` 拼段边界条件（含 `%-<v>.tgz` 支）；单测覆盖 `1.17.12` 不命中 `1.17.120/`、不命中 `1.17.12-rc1/`、命中 `pkg/-/pkg-1.17.12.tgz`
- [x] `asset_repo.go`：新增 `SearchAllByFilter`（无分页 + `maxAggregateRows` 上限 + `truncated` 回报），复用 `buildSearchWhere`
- [x] **新增** `domain/search_package.go`：`ParseCoordinates`（Maven / npm / 降级）、`MatchPrecision`、`BuildPackageGroups`、relevance 比较器；配 `search_package_test.go` 表驱动测试（正例 + 反例 + 边界：`n==3` 的 Maven 扁平路径、scoped packument、`pkg/-/pkg-x.tgz` 剥离失败）
- [x] `repository_service.go`：`SearchAggregate`（复用现有 ACL / facet 收窄逻辑，扩展 `repoID → {name, format}` 映射）；`SearchOutput` 增 `Packages` / `PackageTotal` / `Aggregation`
- [x] `repository_handlers.go`：解析 `group_by`（`file` 缺省 / `package`）与 `order=relevance`；输出 `packages` / `packageTotal` / `aggregation` / `appliedOrder` / 文件级 `sha1`
- [x] 回归：npm `registrySearch` 显式传 `path`/`asc`，断言 `GET /-/v1/search` 响应体与改造前逐字段一致

### FR-151（前端）

- [x] `lib/searchQuery.ts`：`SearchExpression` 增 `versions` / `notVersions`，`parseSearchExpression` / `buildSearchExpression` 双向同步；补测试（手输 `version:x` → 改筛选面板 → 表达式不丢）——**按「不丢」这一实际风险收口，未新增两个专属字段**：解析器不识别 `version:`，`version:1.17.12` 会落进 `terms`、`-version:1.17.12` 落进 `excludeTerms`，而 `buildSearchExpression` 原样回写这些数组，因此**改一次扩展名筛选后版本约束不会失效**（已实测：`-version:1.17.12 ext:jar` → 面板加 `pom` → `-version:1.17.12 ext:jar ext:pom`）。代价有两点，均不影响搜索正确性：① 筛选面板没有版本输入框（只能手输）；② 该 token 会进高亮关键词列表，但它在 `path` 里不出现，视觉上无影响。若将来要给版本做面板输入框，仍需补这两个字段。
- [x] `lib/searchPackage.ts`：树构建、版本向量比较、`fileKind` 归类、高亮切分（`splitHighlight`）；配 vitest
- [x] `api/endpoints.ts`：`SearchResult` 增 `packages` / `packageTotal` / `aggregation` / `appliedOrder`；`searchAssets` 增 `group_by` 参数
- [x] `components/search/PackageSearchTree.tsx`：包/版本/文件三级折叠树 + 精度徽标 + 类型 chip + 校验和折叠
- [x] `components/search/SearchDetailPanel.tsx`：详情 + 同版本文件列表 + 高亮
- [x] `SearchPage.tsx`：结果区替换为主从双栏；`DEFAULT_SORT` 改 relevance；`SortKey` 增 `relevance`（`sanitizeSort` 同步）；窄屏 820px 单栏降维 + 返回；四态接入
- [x] i18n：`search.*` 新键（`precisionExact` / `precisionPrerelease` / `precisionSubstring` / `packagesCount` / `versionsCount` / `filesCount` / `checksums` / `sortRelevance` / `detail*` / `backToResults` / `aggregationTruncated` 等），`zh` / `en` 同步（受 `I18nKeys.test.ts` 守卫）
- [x] devmock：`packages` 结构的 mock 响应，供前端独立开发
- [x] 文档同步：`docs/API.md` 搜索端点（新增参数与响应字段）、PRD 状态、`docs/CHANGELOG.md`；`api/openapi.yaml` **不动**（§3.7）

## 5. 验收标准

### FR-150

- [ ] **排序断言（核心）**：在含 `com.example:foo` 的 `1.17.12` / `1.17.12-rc1` / `1.17.120` 三份制品的数据集上，`q=1.17.12&order=relevance&group_by=package` 的返回顺序严格为 `1.17.12` → `1.17.12-rc1` → `1.17.120`；三者 `precision` 分别为 `exact` / `prerelease` / `substring`（后端单测 + 前端 e2e 双层断言）
- [ ] **`version:` 严格相等**：`q=version:1.17.12` 只返版本段完全等于 `1.17.12` 的文件；同数据集里的 `1.17.120`、`1.17.12-rc1` **一条不返**
- [ ] **`version:` 与其他条件组合**：`q=version:1.17.12 ext:jar` 只返该版本的 jar；`q=version:1.17.12 -ext:sha1` 生效；`-version:1.17.12` 排除该版本且其余结果不受影响
- [ ] **裸路径不丢失**：raw 仓库中 `docs/a/b.txt` 这类无坐标路径出现在 `packages` 的「无坐标」伪包下（`kind: "file"`），仍可点开下载；其 `precision = none`
- [ ] **`order=relevance` 为默认**：把浏览器 `localStorage` 里的 `jianartifact.search.sort` 清空后进入搜索页，第一次查询请求参数含 `order=relevance`，且结果不再按 `updated desc` 排列
- [ ] **无信号查询不退化**：`q=spring`（无版本信号）在 relevance 下的顺序与旧默认（`updated desc`）完全一致（同一数据集比对断言）
- [ ] **降级正确**：`q=spring`（不命中任何版本段）全部 `precision = none`；`q=foo` 命中包名但不命中版本段时同样为 `none`，不误报 `substring`
- [ ] **分页与 total 不变**：`group_by=package&page=2&page_size=20` 的 `items` 与 `total` 与改造前同一请求完全一致；`packageTotal` 不受分页影响
- [ ] **预算截断**：构造 > 5000 条命中的数据，响应 `aggregation.truncated = true`、`total` 仍为精确 `COUNT(*)`、`packageTotal` 标注下界
- [ ] **npm 契约不变**：`GET /-/v1/search?text=xxx` 的响应（`objects` / `total` / `time`）与改造前逐字段一致（golden 对比测试）
- [ ] **既有回归全绿**：`search_page_size_test.go`、`search_query_test.go`、`assetTree.test.ts` 不改断言通过

### FR-151

- [ ] **主从联动**：进入搜索页即自动展开首个包、选中首个版本、右栏显示该版本首个文件（默认态断言）
- [ ] **切换版本不回退列表**：点击 `1.17.120` 后再点击 `1.17.12`，左栏展开状态、滚动位置、其他包的折叠状态**均不变**；且这两个动作**不产生新的网络请求**（断言无 `/search` 请求）
- [ ] **键盘可操作**：`Tab` 可达包行与版本行；`Enter`/`Space` 切换展开与选中；`ArrowRight` 展开、`ArrowLeft` 折叠、`ArrowUp`/`ArrowDown` 在同层内移动焦点（roving tabindex，焦点环可见）；全程鼠标零操作可打开某版本详情并触发下载
- [ ] **高亮范围**：搜 `1.17.12` 时，包坐标/版本号/文件名中的命中子串被 `<mark>` 标记；路径全文无 `<mark>`；切到别的版本后高亮跟随新文本
- [ ] **窄屏 820px**：窗口宽 800px 时左栏单栏全宽、右栏不渲染；点击版本行进入详情；`← 返回搜索结果` 与浏览器返回都能回到列表且展开态/滚动位置保留；详情内点「同版本文件」另一项就地切换
- [ ] **窄屏与双栏切换**：拖宽窗口跨过 820px → 双栏出现且右栏显示当前选中项，选中态不被重置
- [ ] **四态**：① 无结果 → 空态文案 + 搜索框仍可用；② 无权仓库 → `ForbiddenState`；③ 首载 → 骨架、换排序 → 保留旧结果不闪骨架；④ 后端 500 → `ErrorState` + 重试可重新发起
- [ ] **校验和折叠**：版本内 `sha1`/`md5`/`asc` 默认折为 `校验和 (n)`，展开可见且可下载
- [ ] **i18n**：`zh` / `en` 键集与占位符一致（`I18nKeys.test.ts` 绿）；界面无硬编码中文
- [ ] **真机过**：在真实 Maven + npm 混合仓库上，搜 `1.17.12` 能看到精确版本置顶、`1.17.120` 排在最后一行组；点开详情 SHA1 与 `sha1sum` 一致；点「打开仓库」跳转到该文件并在树中选中（与 FR-145 一致）

## 6. 风险 / 待定

- **包身份键跨仓库合并**：本规格取 `coordinate`（不含仓库），让"同包多版本"跨 proxy/hosted 合并成一个节点。副作用是同一版本同时存在于两个仓库时文件会并列出现（靠文件行的仓库徽标消歧）。若用户反馈混淆，改为 `(coordinate, repository)` 复合键即可——只影响 `BuildPackageGroups` 的分组键，不动接口形状。**待实机验证后定稿。**
- **坐标解析的格式门控**：只认 `maven` / `npm` 两种 format，raw 仓库里的 Maven 布局一律降级。这是有意的保守取舍（避免把 `docs/a/b/c` 误判为 `a:b:c`），代价是"把 Maven 制品发到 raw 仓库"这种用法享受不到聚合。若真出现规模性诉求，再评估"按扩展名 + 版本段形态"的受限启发式。
- **`version:` 的路径段误报**：若某个**非版本**目录恰好叫 `1.17.12`（如包名与版本号同形），`version:` 会命中。误报率极低，暂不处理。
- **聚合扫描预算 5000** 是按"宽泛关键词（`a`、`test`）"取的保守值，未做百万级实测。若常规查询就频繁触发截断，需下调预算或改为"先按包分组降维再取数"的两段式查询（后者要 SQL 侧表达坐标，与 FR-149 的规范化列方向一致）。**与 FR-149 联动待定。**
- **响应体积**：`group_by=package` 一次吐出文件明细（5000 行 × 约 200 字节 ≈ 1 MB，gzip 后小得多）。若实测超预期，改为"树只带包与版本汇总，展开某版本时按需拉该版本文件列表"（复用既有仓库资产列表端点）。当前不做。
- **是否把 `/api/v1/search` 纳入 openapi 契约**：聚合结构是三层嵌套，纳入 oapi-codegen 会带来一整套新 schema 与前端生成的连锁改动。本 FR 只同步 `docs/API.md` + 前端类型。**待定（倾向长期纳入，但单独一件工作）。**
- **精度分层的产品直觉**：`1.17.12+build.7` 被判为"预发布"而非"精确"。语义上 build metadata 不是预发布，但它确实不是用户输入的那个版本串，放在第二层符合"次优但相邻"的直觉。若用户认为应等同精确，只需把 `+` 从 §3.4 rank 1 的条件里去掉。
- **窄屏断点归一**：结果区换成树+详情后，旧扁平表格里按 48em（768px）隐藏的"大小/修改时间"列随之消失，`isNarrow` 只剩"搜索框最小宽度"等零星用途。因此本 FR **只保留 820px 一个布局断点**管双栏降维，不新增第二套并存断点；实现时把 `isNarrow` 的残余用途一并对齐到 820px，避免一个页面两个"窄屏"定义。
- **数据模型决定：不新增版本列 / 包坐标列**（FR-149 规格把该议题派给本 FR）。理由是：坐标可由 path 在 Go 侧确定性推导，加列意味着迁移 + 回填 + 双写一致性，且解析规则一旦调整就要回灌历史数据；而当前聚合是**只读、无状态**的派生计算。代价是每次聚合要扫候选行（见上条预算）。若将来聚合成为高频路径或需要按包做服务端分页/过滤，再评估物化列并单独立项。

## 附录 A：fileKind 归类表

判定顺序自上而下，首个命中即停（扩展名比较大小写不敏感）。

| `fileKind`            | 判定                                                          | `fileKindPriority` |
| --------------------- | ------------------------------------------------------------- | ------------------ |
| `jar`                 | 扩展名 `jar`，且文件名不含 `-sources` / `-javadoc` / `-tests` | 0                  |
| `war` / `ear` / `aar` | 同名扩展名                                                    | 1                  |
| `tgz`                 | 扩展名 `tgz`（npm tarball）                                   | 1                  |
| `sources`             | 扩展名 `jar` 且文件名含 `-sources`                            | 2                  |
| `javadoc`             | 扩展名 `jar` 且文件名含 `-javadoc`                            | 3                  |
| `tests`               | 扩展名 `jar` 且文件名含 `-tests`                              | 4                  |
| `pom`                 | 扩展名 `pom`                                                  | 5                  |
| `packument`           | 扩展名 `json` 且路径为 npm 包根                               | 5                  |
| `metadata`            | 文件名为 `maven-metadata.xml`，或扩展名 `module`              | 6                  |
| `other`               | 以上都不匹配                                                  | 7                  |
| `checksum`            | 扩展名 ∈ `sha1` / `md5` / `sha256` / `sha512` / `asc`         | 9                  |

`checksum` 与既有 `CHECKSUM_EXTS`（[searchQuery.ts](apps/web/src/lib/searchQuery.ts#L104)）保持同一集合，避免"一键隐藏校验和"与"树内校验和折叠"两套口径打架。

## 附录 B：精度判定用例表

`T = 1.17.12`，判定见 §3.4。

| 版本段 `V`                  | 精度         | 说明                              |
| --------------------------- | ------------ | --------------------------------- |
| `1.17.12`                   | `exact`      | 完全相等                          |
| `1.17.12.1`                 | `substring`  | `R = .1`，不是预发布标记          |
| `1.17.12-rc1`               | `prerelease` | `R = -rc1`                        |
| `1.17.12-SNAPSHOT`          | `prerelease` | `R = -SNAPSHOT`                   |
| `1.17.12-alpha.3`           | `prerelease` | `R = -alpha.3`                    |
| `1.17.12-20240101.123456-3` | `prerelease` | Maven 时间戳快照，`R` 以 `-` 起首 |
| `1.17.12+build.7`           | `prerelease` | 构建元数据，`R = +build.7`        |
| `1.17.120`                  | `substring`  | `R = 0`（关键回归用例）           |
| `11.17.12`                  | `substring`  | 命中在尾部                        |
| `1.17.1200`                 | `substring`  | 同上                              |
| `1.18.0`                    | `none`       | 不含 `T`                          |
| （无坐标裸路径）            | `none`       | 降级条目                          |
