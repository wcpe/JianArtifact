# 功能规格：迁移覆盖 group 本体（FR-137）

> 状态：开发中　·　关联 PRD：FR-137（依赖 FR-136）　·　分支：fr-137-migrate-group

## 1. 背景与目标

FR-136 落地了 group / 虚拟聚合仓库类型，但 Nexus 迁移流程（FR-36/38/39）尚不覆盖 group 仓库：
只迁 hosted 与 proxy，不迁 group 本体及其成员映射。

FR-137 补齐这一缺口：通过在线 REST 枚举源 Nexus 的 group 仓库（`type == "group"`），
在本系统建对应 group 仓库并映射成员，使迁后 group 解析命中。

## 2. 需求（要什么）

- 解析 Nexus REST `/service/rest/v1/repositories` 响应，取 group 仓库的成员名列表
  （`attributes.group.memberNames`，字符串数组）。
- 新增 group 迁移编排：在线 REST 枚举 → 建 group 仓库 + 设成员映射。
- 迁移顺序：成员仓库（hosted/proxy，已由 FR-38/39 先建）先在本系统存在，group 后建。
- 幂等：同名 group 已存在则更新其成员映射（覆盖）；无已存在则新建。
- 成员缺失（在本系统找不到对应仓库）→ 记告警 + 跳过该成员（不中断整 group 建立）。
- 格式一致校验复用 FR-136 校验逻辑（`resolve_and_validate_group_members`）。
- Docker group 不做（FR-136 已界定 Docker format 不支持 group）。
- 范围只做 group 本体迁移（建 group + 映射成员）；成员制品本身的搬运走 FR-38/39/125。

## 3. 接口变化

### 3.1 后端结构体扩展

`NexusRepoSummary` 新增字段：
```rust
pub group_members: Vec<String>,  // group 仓库成员名列表；非 group 为空
```

`parse_repositories` 扩展解析 `attributes.group.memberNames`。

### 3.2 新增 REST 端点

```
POST /api/v1/migrate/nexus/group/migrate
```

请求体：
```json
{
  "base_url": "https://nexus.example",
  "auth_ref": "PROD"      // 可选，凭据引用
}
```

成功返回 `200 + GroupMigrationReport`：
```json
{
  "migrated": [
    { "name": "maven-group", "format": "maven", "created": true, "member_count": 3, "skipped_members": [] }
  ],
  "skipped": ["docker-group"]
}
```

说明：
- group 迁移为纯元数据操作（建仓库 + 设成员），无 blob 搬运，故不异步（无需 job_id）。
- 仅管理员可调用。
- 同步返回完整报告（无需 202 异步）。

## 4. 实现要点

### 4.1 parse_repositories 扩展

在 `src/migrate/mod.rs` 中：

```rust
#[derive(serde::Deserialize, Default)]
struct RawAttributes {
    #[serde(default)] proxy: Option<RawProxy>,
    #[serde(default)] group: Option<RawGroup>,
}

#[derive(serde::Deserialize)]
struct RawGroup {
    #[serde(rename = "memberNames", default)]
    member_names: Vec<String>,
}
```

### 4.2 group.rs（新文件）

`src/migrate/group.rs` 提供：
- `GroupRepoOutcome`：单个 group 迁移结果（名称 / 格式 / 是否新建 / 成员数 / 跳过成员）
- `GroupMigrationReport`：整批报告
- `migrate_group_repositories(meta, source_repos)`：核心函数

逻辑：
1. 筛 `type == "group"` 的源仓库；
2. 格式映射（`map_nexus_format`），无法映射则跳过整 group；
3. Docker format 整体跳过（FR-136 界定）；
4. 按成员名查本系统仓库（`meta.get_repository_by_name`），
   缺失记告警 + 加 `skipped_members`，存在的收集 id；
5. 同名 group 已存在 → 调 `meta.set_repo_group_members` 更新成员（幂等覆盖）；
   不存在 → 调 `meta.create_repository(type=Group)` + `meta.set_repo_group_members`；
6. 返回报告。

### 4.3 api/migrate.rs 新端点

`migrate_nexus_group` handler：
- 管理员鉴权；
- 在线枚举（`discover_repositories`）；
- 调 `migrate::migrate_group_repositories`；
- 同步返回 JSON 报告。

不走单飞门（group 迁移是纯元数据操作，无需排他锁）。

## 5. 测试用例（先行）

### 5.1 parse_repositories group 成员解析

- 含 group 仓库（带 memberNames）→ `group_members` 正确解析；
- 无 attributes.group 的 proxy/hosted → `group_members` 为空；
- memberNames 为空数组 → `group_members` 为空；

### 5.2 migrate_group_repositories

- 成员 A、B 已在本系统 → 建 group + 成员映射正确；
- 同名 group 已存在 → 更新成员（幂等覆盖）；
- 部分成员缺失 → 告警 + 跳过缺失成员 + group 仍建成（含剩余成员）；
- 全部成员缺失 → group 建成（空成员）；
- 格式无法映射（如 rubygems）→ 跳过；
- docker format → 跳过；
- 非 group 类型源仓库 → 跳过（不混入）；

## 6. 验收标准

- 单测全绿（5.1 + 5.2 所有用例）；
- `cargo clippy --all-targets -- -D warnings` 0 warn；
- `cargo fmt --check` 通过；
- 真机：对真实 Nexus group 迁一次，本系统有对应 group + 成员、解析命中（**待真机**）。
