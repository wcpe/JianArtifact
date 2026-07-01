# 功能规格：group / 虚拟聚合仓库（新仓库类型）

> 状态：已实现（ADR-group-virtual-repository 已接受）　·　关联 PRD：FR-136（被 FR-137 依赖）　·　分支：fr-136-group-repo

## 1. 背景与目标

当前仓库只有 hosted（直传）与 proxy（代理缓存）两种类型。实际使用中，同一格式的多个仓库
（如 maven-releases、maven-snapshots、maven-central-proxy）需要被客户端用**单一 URL** 统一消费，
而不必在客户端逐个配置。这正是 Nexus / Artifactory 的 **group（虚拟聚合）仓库**：它本身不存储制品，
而是聚合一组**有序的成员仓库**，GET 制品时按成员顺序解析，返回第一个命中的成员的制品。

本能力推翻 `scope-discipline` 对 group 的排除（用户已授权范围扩张），属架构决策，另写 ADR（见 §3）。

属阶段：P2。

## 2. 需求（要什么）

范围内：
- 新增第三种仓库类型 **group**：聚合**有序成员仓库列表**，自身不存储 blob。
- 创建 group 时指定成员（有序，按 position）；成员须与 group **格式一致**（maven group 只聚合 maven 成员），否则拒绝创建。
- 成员可为 hosted 或 proxy（proxy 成员命中时复用既有回源缓存机理）。
- GET 制品 `/{group}/{*path}`：按成员 position 顺序遍历，对每个成员**施加调用方读权限判定**
  （复用既有 authz：public/private + ACL + 角色 + 组继承）。命中第一个「调用方有读权限且存在该制品」的成员→返回其制品；
  全部未命中→404。
- **权限过滤（命门）**：无读权限的成员视同**不存在**、不参与解析、不泄露其存在性；匿名跳过 private 成员；
  private 成员制品对无权调用方一律 404（不泄露存在性）。
- group 仓库 **只读**：PUT / POST / DELETE 制品到 group → 405 Method Not Allowed（与 Nexus 行为一致）。
- group 仓库的管理（创建 / 配置成员 / 删除）仅管理员；删除 group 仅删 group 自身与成员关联，**不影响成员仓库**。
- group 自身有可见性（public/private）与 ACL：访问 group 端点先过 group 自身读判定，再逐成员过成员读判定
  （双层判定：group 不可见 → 整体 404；group 可见但成员无权 → 该成员跳过）。

不做（范围外，主控已拍板）：
- **Docker（OCI registry v2）group 不在本 FR 范围、属后续 FR**：Docker 走独立的 `/v2/` 路由树（`docker_routes`），
  其 manifest/blob 解析与 group 成员聚合是另一套代码路径，本 FR 不覆盖；group 仅支持经 `format_routes` 的格式
  （maven / npm / raw / go / cargo / pypi / nuget）。Docker group 若需要，另开 FR。
- 嵌套 group（group 成员又是 group）：本期成员仅限 hosted / proxy，建仓时校验成员非 group（防环与递归解析复杂度）。
- 写入路由到成员（Nexus 的「group 写到指定 deployment 成员」）：本期 group 纯只读。
- 成员级 metadata 合并（如 maven group 把多成员 maven-metadata.xml 合并为聚合视图）：本期按「成员有序解析、首个命中即返回」，
  不做跨成员 metadata 合并（**真实功能限制见 §6**）。可作后续增强 FR。

## 3. 设计（怎么做）

涉及架构决策（新仓库类型 + 推翻 scope 排除）→ 另写 **ADR-XXXX（group/virtual 仓库）**（占位号，主控统一分配），
在此仅引用、不重复其决策正文。

### 3.1 数据模型（meta，唯一真源）

新增 migration `00NN_group_members.sql`（向前兼容，遵 ADR-0031：只增表，不改既有表）：

```sql
CREATE TABLE repository_group_members (
    group_repo_id  TEXT NOT NULL,   -- group 仓库 id（repositories.id）
    member_repo_id TEXT NOT NULL,   -- 成员仓库 id（repositories.id）
    position       INTEGER NOT NULL, -- 解析顺序（升序）
    PRIMARY KEY (group_repo_id, member_repo_id),
    FOREIGN KEY (group_repo_id) REFERENCES repositories (id) ON DELETE CASCADE,
    FOREIGN KEY (member_repo_id) REFERENCES repositories (id) ON DELETE CASCADE
);
CREATE INDEX idx_group_members_order ON repository_group_members (group_repo_id, position);
```

- 删除 group → 经外键级联清成员关联；删除某成员仓库 → 经外键级联从所有 group 中移除其关联。
- `repositories.type` 增加合法取值 `group`（不改表结构，仅 `RepoType` 枚举 + 解析新增分支）。

### 3.2 仓库类型与生命周期（meta + repo）

- `meta::RepoType` 增 `Group` 变体；`as_str`→`"group"`、`from_db_str` 增 `"group"` 分支（未知值仍回退 Hosted）。
- `repo::create`：type=group 时跳过 proxy 上游校验，改为校验成员列表——成员存在、格式与 group 一致、去重、非空（空 group 允许？取**允许**，解析恒 404）。
- 新增 meta 方法（不绕过 meta）：
  - `set_group_members(group_id, &[member_id])`：按入参顺序写 position（建 / 配置时调用，先清后插，事务内）。
  - `list_group_members(group_id) -> Vec<RepositoryRecord>`：按 position 升序连表取成员仓库记录。
- `repo::create` 入参 `CreateRepoInput` 增 `members: Option<Vec<String>>`（成员仓库名或 id，定为**名**对齐 API 友好）。

### 3.3 解析与鉴权（api，handler 保持薄，authz 复用既有）

在 `format_routes` 新增 group 分流：`get_artifact` / `get_repo_root` 解析仓库后，若 `type==group`：
1. 先过 **group 自身**读判定（`build_repo_view` + `authorize(Read)`）；group 不可见 → 404（隐藏存在性）。
2. `list_group_members` 取有序成员；逐成员：
   - 构造该成员的 `RepoView`（`build_repo_view`，复用既有 ACL / 组继承查询），过 `authorize(Read)`；
   - **Deny → 跳过该成员**（视同不存在，不泄露）；
   - Allow → 调既有 `get_artifact_inner` 等价逻辑（按成员格式分派 + proxy 回源），命中（非 NotFound）→ 返回；
     NotFound → 继续下一成员。
3. 全部成员遍历完未命中 → 404。

抽出 group 解析编排为薄函数（如 `resolve_group_get`），handler 只调用，不堆业务。
PUT / POST / DELETE：解析出 type=group → 直接返回 405（在 `resolve_writable_repo` 或 handler 入口拦截）。

### 3.4 创建 / 配置 API（api）

- `CreateRepositoryRequest` 增 `members: Option<Vec<String>>`（仅 group 用）。
- type=group 创建：校验 + 落 repositories 行 + `set_group_members`（事务）。
- 更新成员：复用 `update_repository` 或新增 `PUT /api/v1/repositories/{id}/members`（定为：**创建时设定，更新经 members 字段**，最简）。
  → 决定：`UpdateRepositoryRequest` 增 `members: Option<Vec<String>>`，仅当 type=group 时生效。
- `RepositoryDto` 对 group 增 `members: Vec<String>`（成员名有序），非 group 省略。

## 4. 任务拆分

- [ ] migration `00NN_group_members.sql`（向前兼容）
- [ ] meta：`RepoType::Group`、`set_group_members` / `list_group_members`（+ 单测）
- [ ] repo：`create` / `update` 支持 group + 成员格式一致性校验（+ 单测，红先行）
- [ ] api：group GET 解析 + 逐成员读授权过滤（`format_routes`，+ 鉴权矩阵穷举测试）
- [ ] api：group 写 / 删 → 405
- [ ] api：创建 / 配置 group 成员端点 + DTO
- [ ] 文档同步：PRD 状态、ADR 占位、ARCHITECTURE、API、CHANGELOG

## 5. 验收标准

- 建 group 含成员 A、B：制品仅在 B → 解析命中 B；A、B 都有 → 命中靠前成员 A；都无 → 404。
- **鉴权矩阵穷举**：成员 public/private × 调用方（Admin / User-有读ACL / User-无ACL / 匿名）逐格断言
  「有权命中 / 无权视同不存在不泄露」；匿名对 private 成员制品 GET → 404（不泄露存在性）；
  private 成员被无权调用方跳过、不影响后续 public 成员命中。
- 写 / 删 / POST 到 group → 405。
- 建 group 成员格式不一致（maven group 加 npm 成员）→ 拒绝（400）。
- proxy 成员命中触发回源缓存（复用既有，至少一条集成测试覆盖 group→proxy 成员 cache-miss）。
- `cargo test`（受影响：meta / repo / authz / format_routes / 新 group 测试）全绿；clippy 0 warn；fmt 通过。

## 6. 风险 / 限制（已拍板）

- **真实功能限制——不做跨成员 metadata 合并**：group 按「成员有序解析、首个命中即返回」。
  因此对依赖聚合视图的元数据文件存在限制：例如 maven group 的 `maven-metadata.xml`
  **只返回第一个含该文件的成员的内容，版本列表不跨成员聚合**（若版本分散在多个成员仓库，
  group 不会把它们合并为一份完整版本列表）。npm packument、PyPI Simple 索引等同理（只取首个命中成员）。
  此限制需对用户显式知悉（已在 CHANGELOG 注明），跨成员 metadata 合并作候选后续 FR。
- **Docker group 排除**：本 FR 明确不做 Docker `/v2/` group（独立路由树），属后续 FR（§2 范围外）。
- 空 group 行为：允许创建、解析恒 404（不报错）。
- group 自身可见性与成员可见性的**双层判定**：group 可见但所有成员对调用方不可见 → 404（与「group 内无此制品」同语义，不泄露成员存在）。
