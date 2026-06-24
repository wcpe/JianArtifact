-- 用户组/团队与组级仓库 ACL（FR-49 / ADR-0007）。
-- 在既有"全局角色 + 每仓库可见性 + 每用户 ACL"模型上扩展批量授权：
-- 可对组授予仓库读/写/删/管理四级动作，组成员据此经组继承权限。
-- 设计取舍：组 ACL 单列新表 repo_group_acl，不改既有 repo_acl 结构，
-- 既有直接-用户 ACL 的数据与判定结论完全不变，仅在判定取权限集合时把组授权并入。

-- 用户组表：组名唯一，便于管理界面按名辨识。
CREATE TABLE groups (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 组成员表：记录"某用户属于某组"，复合主键避免重复加入。
-- 删除用户或组时其成员关系经外键级联清理，不留悬挂记录。
CREATE TABLE user_groups (
    group_id TEXT NOT NULL,
    user_id  TEXT NOT NULL,
    PRIMARY KEY (group_id, user_id),
    FOREIGN KEY (group_id) REFERENCES groups (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

-- 组级仓库 ACL：对某组在某仓库授予一项四级动作，结构与 repo_acl 对齐但主体为组。
-- 删除仓库或组时其组 ACL 经外键级联清理。
CREATE TABLE repo_group_acl (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    group_id   TEXT NOT NULL,
    permission TEXT NOT NULL
        CHECK (permission IN ('read', 'write', 'delete', 'admin')),  -- 四级动作
    FOREIGN KEY (repo_id) REFERENCES repositories (id) ON DELETE CASCADE,
    FOREIGN KEY (group_id) REFERENCES groups (id) ON DELETE CASCADE
);

-- 索引：成员按用户反查所属组（判定取组权限）、组 ACL 唯一约束与按 (repo, group) 查询。
CREATE INDEX idx_user_groups_user ON user_groups (user_id);
CREATE UNIQUE INDEX idx_repo_group_acl_unique ON repo_group_acl (repo_id, group_id, permission);
CREATE INDEX idx_repo_group_acl_repo_group ON repo_group_acl (repo_id, group_id);
