# ADR-XXXX：group / 虚拟聚合仓库（新增第三种仓库类型）

> 占位编号 `XXXX`，由主控落地时统一分配。

## 状态
已接受

## 背景

在此决策前，仓库只有 hosted（直传）与 proxy（代理缓存）两种类型（见 p1 相关 ADR）。
实际使用中，同一格式的多个仓库（如 `maven-releases` / `maven-snapshots` / `maven-central-proxy`）
需被客户端用**单一 URL** 统一消费，而不必在客户端逐个配置。这正是 Nexus / Artifactory 的
**group（虚拟聚合）仓库**：自身不存储制品，而是聚合一组**有序成员仓库**，GET 制品时按成员顺序解析，
返回第一个命中成员的制品。

`scope-discipline`（范围纪律）此前把「group/virtual 聚合仓库」列为 MVP 之外的镀金项而排除。
本决策**推翻该排除**——用户已明确授权此范围扩张（对应 FR-136），故不再视为镀金；
按防漂移规则，范围决策变更须以 ADR 记录，本 ADR 即承载该决策。

## 决策

新增第三种仓库类型 **group**，遵循以下模型：

- **有序成员解析**：group 自身不存 blob，聚合一组有序成员仓库（hosted / proxy）。
  GET 制品时按成员 `position` 升序遍历，命中第一个「调用方有读权限且存在该制品」的成员即返回；全部未命中 → 404。
- **成员 ACL 过滤（命门）**：逐成员施加**调用方读权限判定**（复用既有 `authz`：public/private +
  直接 ACL + 组继承 ACL + 角色）。无读权限的成员**视同不存在**、跳过、不泄露其存在性
  （匿名跳过 private 成员）。group 自身也有可见性与 ACL：先过 group 自身读判定
  （不可见 → 整体 404、隐藏 group 存在），再逐成员过成员读判定（双层判定）。
- **只读**：PUT / POST / DELETE 到 group → 405 Method Not Allowed（与 Nexus 行为一致）。
  私有 group 对无权调用方仍先返 404（不泄露存在性），再谈 405。
- **禁止嵌套**：成员仅限 hosted / proxy；建仓时校验成员非 group（防环与递归解析复杂度）。
- **格式一致**：成员格式须与 group 一致（maven group 只聚合 maven 成员），否则拒绝创建（400）。

### 成员存储

新增 migration `0013_group_members.sql`（遵 ADR-0031 向前兼容，只增表不改既有表）：

```sql
CREATE TABLE repository_group_members (
    group_repo_id  TEXT NOT NULL,    -- group 仓库 id（repositories.id）
    member_repo_id TEXT NOT NULL,    -- 成员仓库 id（repositories.id）
    position       INTEGER NOT NULL, -- 解析顺序（升序遍历）
    PRIMARY KEY (group_repo_id, member_repo_id),
    FOREIGN KEY (group_repo_id) REFERENCES repositories (id) ON DELETE CASCADE,
    FOREIGN KEY (member_repo_id) REFERENCES repositories (id) ON DELETE CASCADE
);
CREATE INDEX idx_group_members_order ON repository_group_members (group_repo_id, position);
```

- 删除 group → 经外键级联清成员关联；删除某成员仓库 → 经外键级联从所有 group 中移除其关联。
- `repositories.type` 增加合法取值 `group`（不改表结构，仅 `RepoType` 枚举 + 解析新增分支）。
- `meta` 作为元数据唯一访问入口，新增 `set_repo_group_members` / `list_repo_group_members`
  （与 FR-49 用户组的 `MetaStore::list_group_members` 正交、命名区分，避免复用）。

### 与既有机制的关系

- **RepoType**：`RepoType::Group` 为第三个变体；`as_str`→`"group"`、`from_db_str` 增 `"group"` 分支
  （未知值仍回退 Hosted，绝不误引入聚合解析行为）。
- **authz**：group 解析不新增判定逻辑，逐成员复用 `repo_access::build_repo_view`（含直接 ACL ∪ 组继承）
  + `authz::authorize(Read)`，守「private 对无权一律 404、不泄露存在性」的红线。
- **format 分发**：group 命中成员后走既有 `get_artifact_inner`（按成员格式分派 + proxy 成员回源缓存），
  不新增格式处理器、不用 `if-else` 按格式名堆叠可变逻辑。
- **分层**：解析编排在 `api::format_routes`（handler 保持薄，抽出 `resolve_group_get` 薄函数），
  经 `repo` 做建仓校验、经 `meta` 读写成员，依赖方向 `api → repo/authz/format → meta` 不变、无环。

## 理由

- **有序成员解析 + 首个命中即返回**是 group 的最小可用语义，实现直接、无跨成员聚合的额外复杂度。
- **逐成员施加读权限**而非「group 可见即放行全部成员」，守数据不泄露红线——group 不能成为绕过成员
  private ACL 的旁路。
- **只读**避免「写路由到哪个成员」的歧义（Nexus 的 deployment 成员写入属后续增强），本期保持最简。
- **禁止嵌套 + 格式一致**在建仓时前置校验，避免运行时环检测与跨格式解析分支。
- **只增表**满足向前兼容：旧数据无 group 行、新表为空，升级无损。

## 后果

正面：

- 客户端可用单一 URL 消费多个同格式仓库，贴合 Nexus / Artifactory 使用习惯。
- 成员级 ACL 过滤使 group 在聚合便利与权限隔离之间取得平衡。

负面 / 限制（**已拍板、需用户显式知悉**）：

- **不做跨成员 metadata 合并**：按「成员有序解析、首个命中即返回」，故依赖聚合视图的元数据文件受限——
  maven group 的 `maven-metadata.xml` **只返回第一个含该文件的成员**内容、**版本列表不跨成员聚合**
  （版本分散在多成员时不会合并为完整版本列表）；npm packument、PyPI Simple 索引等同理只取首个命中成员。
  跨成员 metadata 合并作候选后续 FR。
- **Docker group 不在本 FR 范围**：Docker（OCI registry v2）走独立 `/v2/` 路由树，其 manifest/blob 解析
  与 group 成员聚合是另一套代码路径；group 仅支持经 `format_routes` 的格式
  （maven / npm / raw / go / cargo / pypi / nuget）。Docker group 若需要另开 FR。
- 空 group 允许创建，解析恒 404（不报错）。

## 备选方案

- **group 可见即放行全部成员**（不逐成员判权）：落选——会绕过成员 private ACL、泄露私有制品，触红线。
- **跨成员 metadata 合并**（聚合完整版本列表）：本期不做——实现复杂（需按格式解析并合并各成员元数据），
  MVP+ 范围内以「首个命中」满足主用例，合并作后续增强。
- **group 支持写入路由到指定 deployment 成员**（Nexus 行为）：落选——写路由语义歧义、本期只读最简。
- **允许嵌套 group**：落选——引入环检测与递归解析复杂度，成员限 hosted/proxy 已够用。
