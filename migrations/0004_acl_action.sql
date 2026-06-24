-- 仓库 ACL 权限动作从 read | write 细化为四级动作 read | write | delete | admin（FR-48 / ADR-0007）。
-- 向后兼容：既有 read | write 数据原样保留并继续有效；本迁移仅把允许取值收敛到四级动作集合，
-- 并不改变既有读写判定。SQLite 无法对既有列直接加 CHECK，故按"建新表 → 搬数据 → 换名"的标准
-- 流程重建 repo_acl，重建后恢复原有唯一索引与按 (repo_id, user_id) 的查询索引。

-- 关闭外键以便重建期间搬运数据（迁移在单连接内顺序执行）。
PRAGMA foreign_keys = OFF;

-- 新表：permission 取值收敛为四级动作，其余结构与约束与原表一致。
CREATE TABLE repo_acl_new (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    permission TEXT NOT NULL
        CHECK (permission IN ('read', 'write', 'delete', 'admin')),  -- 四级动作
    FOREIGN KEY (repo_id) REFERENCES repositories (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

-- 原样搬运既有授权（既有仅含 read | write，全部满足新 CHECK，不丢数据）。
INSERT INTO repo_acl_new (id, repo_id, user_id, permission)
SELECT id, repo_id, user_id, permission FROM repo_acl;

-- 替换旧表。
DROP TABLE repo_acl;
ALTER TABLE repo_acl_new RENAME TO repo_acl;

-- 恢复索引：唯一约束（同一 仓库+用户+动作 不重复授予）与 (repo_id, user_id) 查询索引。
CREATE UNIQUE INDEX idx_repo_acl_unique ON repo_acl (repo_id, user_id, permission);
CREATE INDEX idx_repo_acl_repo_user ON repo_acl (repo_id, user_id);

PRAGMA foreign_keys = ON;
