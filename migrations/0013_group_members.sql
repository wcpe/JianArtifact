-- group / 虚拟聚合仓库的有序成员关联（FR-136 / ADR-group-virtual-repository）。
-- group 仓库自身不存 blob，仅聚合一组有序成员仓库；GET 制品时按 position 升序解析。
-- 遵 ADR-0031 向前兼容：只新增表与索引，不改既有 repositories / repo_acl 结构。
-- repositories.type 新增合法取值 'group'，仅在 RepoType 枚举层新增分支，无需改表。

CREATE TABLE repository_group_members (
    group_repo_id  TEXT NOT NULL,    -- group 仓库 id（repositories.id）
    member_repo_id TEXT NOT NULL,    -- 成员仓库 id（repositories.id）
    position       INTEGER NOT NULL, -- 解析顺序（升序遍历）
    PRIMARY KEY (group_repo_id, member_repo_id),
    FOREIGN KEY (group_repo_id) REFERENCES repositories (id) ON DELETE CASCADE,
    FOREIGN KEY (member_repo_id) REFERENCES repositories (id) ON DELETE CASCADE
);

-- 索引：按 group 取成员时按 position 升序排（解析热路径）。
CREATE INDEX idx_group_members_order ON repository_group_members (group_repo_id, position);
