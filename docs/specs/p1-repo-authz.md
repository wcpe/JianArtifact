# 功能规格：仓库模型与授权层

> 状态：开发中　·　关联 PRD：FR-06 / FR-07 / FR-08 / FR-09 / FR-10 / FR-13　·　分支：feature/p1-repo-authz

## 1. 背景与目标

在认证与身份层（Batch 1：解析"是谁"）之上，立起第一期的仓库模型与授权层：管理员能创建 / 配置 / 删除不同格式、类型（hosted/proxy）、可见性（public/private）的仓库并为每仓库授予读 / 写 ACL；调用方对仓库的读写按全局角色 × 可见性 × 每仓库 ACL 综合判定。这是后续制品上传 / 下载（Batch 3）与各格式端点写权限校验赖以判定"能否读写某仓库"的基础。属阶段 P1。

本规格覆盖**仓库领域模型 + 生命周期**与**授权判定（纯函数）+ 授权强制（api 层）**，**不含**制品 blob 的实际上传 / 下载、proxy 拉取缓存、任何格式处理器、Web UI——提前实现即越界（违反 scope-discipline）。制品浏览仅列 `meta` 的 `artifacts` 索引（当前无写入路径，索引为空亦正确）。

## 2. 需求（要什么）

- 范围内：
  - **FR-06**：仓库可见性 public / private。
  - **FR-07**：每仓库读 / 写 ACL（按用户授权名单，permission 取 read | write）。
  - **FR-08**：匿名仅能读 public；private 对匿名一律拒绝（映射 404 隐藏存在性）。
  - **FR-09**：写操作校验对应仓库写权限（命中写 ACL 或管理员）。
  - **FR-10**：创建 / 配置 / 删除仓库（格式、类型 hosted/proxy、可见性）。
  - **FR-13**：仓库列表（按身份过滤）/ 详情 / 制品浏览（列索引，read 受控）。
  - 授权判定纯函数 `authorize(identity, repo_view, action) -> Decision`：无副作用、可穷举测试。
  - 授权强制在 api 层：无权 private → 404；有读无写越权写 → 403；管理类端点限 Admin（非管理员 403、匿名 401）。
- 不做（范围外）：制品 blob 上传 / 下载（FR-11）、proxy 拉取缓存（FR-12）、格式处理器（FR-14~17）、Web 控制台（FR-18~22）、跨仓库搜索（FR-67）、制品详情与使用方式片段（FR-66/68）、列表分页 / 排序 / 搜索参数（统一 FR-62，后续批次落地，本批列表先返回数组形态）；用户组 / 细粒度权限动作（P2）。

## 3. 设计（怎么做）

### 模块结构（在既有 `meta` / `api` 上扩展，新增 `authz`）

- `meta/repo.rs`（新增，与 `meta/mod.rs` 同属元数据访问层）：`Visibility` / `RepoType` / `Permission` 枚举（含 `from_db_str` 未知值安全降级——可见性降 private、权限降 read），`RepositoryRecord` / `AclRecord` / `ArtifactRecord` 行结构，`NewRepository` 创建入参，以及仓库 CRUD（`create_repository` / `get_repository_by_id` / `list_repositories` / `update_repository` / `delete_repository`）、ACL CRUD（`create_acl` / `list_acl_by_repo` / `get_acl_by_id` / `delete_acl`）、授权辅助（`list_user_permissions` 取某用户在某仓库的权限集合、`list_repo_ids_with_read` 取某用户可读的仓库主键集合用于列表过滤、防 N+1）、`list_artifacts_by_repo`。上游凭据真值绝不入库，仅 `upstream_auth_ref` 存引用。
- `authz/mod.rs`（新增）：纯函数 `authorize(identity: &AuthIdentity, repo: &RepoView, action: Action) -> Decision`。`Action`（Read/Write）、`Decision`（Allow/Deny）、`RepoView`（可见性 + 调用方在该仓库的 ACL 命中 `caller_can_read` / `caller_can_write`，`from_permissions` 据 ACL 集合构造，write 蕴含 read）。判定规则见下。无副作用、不触 DB / IO。
- `api/repositories.rs`（新增）：仓库列表（按身份过滤）/ 创建（Admin）/ 详情（读受控）/ 更新（Admin）/ 删除（Admin）/ 制品浏览（读受控）。`load_readable_repo` 解析仓库并施加读授权，Deny → 404 隐藏存在性；`build_repo_view` 据身份查 ACL 构造视图（匿名不查库）。格式仅接受第一期四种（maven/npm/docker/raw），proxy 须带 upstream_url。
- `api/acl.rs`（新增）：列出 / 新增 / 移除某仓库 ACL，均限 Admin；重复授权 409，仓库 / 用户不存在 404，删除时校验 ACL 归属该仓库。
- `api/mod.rs`：挂载 `/repositories` 与 `/repositories/{id}/acl` 路由族。
- `meta/mod.rs`：新增 `pub(crate) pool()` 供同模块 `repo.rs` 复用连接池（不对外暴露原始连接，守"meta 唯一访问入口"）。

### 授权判定规则（ADR-0004）

- 管理员：对任意仓库读写一律放行。
- public 仓库：匿名与任意登录用户可读；写需命中写 ACL（或 Admin）。
- private 仓库：仅命中读 / 写 ACL 的用户（或 Admin）可读；其余（含匿名、无 ACL 用户）一律拒绝。
- 写操作：须已认证且命中写 ACL（或 Admin）；只读不得越权写。`authorize_write` 显式要求 `is_authenticated` 作纵深防御，杜绝匿名通道绕过。

### 错误语义（docs/API.md §2 定式）

- 无读权限的 private（含匿名、无 ACL 登录用户）详情 / 浏览 → **404**（隐藏存在性，不用 401/403）。
- 已可读但越权写（有读无写）→ 403（写路径属后续制品批次，本批仅 pure 函数判定到位）。
- 管理类端点（仓库 CRUD、ACL 管理）：非管理员 403、匿名 401。
- 创建重名仓库 / 重复 ACL → 409；非法格式 / 类型 / 可见性 / 权限 / proxy 缺上游 → 400。

### 数据库迁移

- 新增 `migrations/0002_repo_acl_unique.sql`：对 `repo_acl(repo_id, user_id, permission)` 建唯一索引，支撑 POST ACL 的 409 语义（同一用户对同一仓库的同类授权不可重复；read 与 write 两条不冲突）。既有 `0001_init.sql` 不改（迁移不可变，向前追加）。

### 对齐的 ADR

- ADR-0004（授权模型）：三层综合判定（全局角色 + 可见性 + 每仓库读写 ACL），私有对匿名 / 未授权一律拒绝，写校验写权限。本批为其落地，未引入新决策，故不新增 ADR。

### 本批新增依赖

无。仅复用既有 `sqlx` / `uuid` / `serde` 等；列表过滤用标准库 `HashSet`。

## 4. 任务拆分

- [x] meta/repo：枚举 + 行结构 + 仓库 / ACL / 制品 CRUD 与授权辅助查询，配套单测。
- [x] migrations/0002：repo_acl 唯一索引。
- [x] authz/mod：`authorize` 纯函数 + `RepoView`，鉴权矩阵全组合穷举单测。
- [x] api/repositories：列表过滤 / CRUD / 详情 / 浏览 + 读授权强制（私有 404）。
- [x] api/acl：ACL 列增删（Admin-only）。
- [x] api/mod：路由挂载。
- [x] HTTP 集成测试（tests/repo_authz_api.rs）：CRUD admin-only、私有 404、列表过滤、ACL CRUD、读权限矩阵、三身份通道一致。
- [x] 文档同步：本规格、PRD 状态（FR-06/07/08/09/10/13 改开发中）、CHANGELOG。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（83 lib + 22 auth 集成 + 16 repo_authz 集成 = 121 通过），覆盖：鉴权判定矩阵全组合（visibility × role × ACL × action，pure 函数穷举）；私有对匿名 / 无权一律 404（详情 + 浏览，不泄露存在性）；写权限边界（仅 read 不得写、写需 write 或 Admin）；三身份通道（Bearer-JWT / Bearer-Token / Basic）对私有读判定一致；仓库 CRUD admin-only（非管理员 403、匿名 401、重名 409、非法入参 400、proxy 缺上游 400）；ACL CRUD admin-only（重复 409、用户 / 仓库不存在 404）；列表按身份过滤（匿名仅 public，登录用户见 public + 自己有读权限的 private，含仅 write 蕴含读）。
- `cargo clippy --all-targets -- -D warnings` 无警告。
- 实跑：起二进制，建一个 private 仓库后，匿名 GET 该仓库 → 404；Admin GET → 200——已验证通过（HTTP 404 / 200）。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；上游凭据仅存引用、不回显 `upstream_auth_ref`、不入库明文 / 不进日志。

## 6. 风险 / 待定

- 列表与浏览端点本批返回数组形态；统一分页响应结构（`{items,total,offset,limit,has_more}`，FR-62）留后续批次落地，届时同步 API.md。
- 制品浏览当前列空索引（无制品写入路径）；待 Batch 3 制品上传落地后自然填充，端点与读鉴权已到位。
- 写授权（命中写 ACL → Allow，有读无写 → 403）pure 函数已就绪；其在制品上传 / 删除端点的实际强制属 Batch 3，本批不实现写端点。
