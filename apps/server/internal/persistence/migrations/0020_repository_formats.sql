-- 0020：扩展仓库格式枚举
-- 引入版本：0.8.0
-- 影响：重建 repository 表并扩展 format 枚举
-- 数据处理：迁移既有 repository、ACL 和关联数据；预计耗时：随仓库及关联行数增长
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-25 / FR-26 / FR-27 / FR-28 / FR-29 / FR-32 / ADR-0002
-- 0020：扩展仓库格式枚举（FR-25～29、FR-32）。
-- 通过新表替换保留原表名；子表外键仍指向 repository，历史数据与 ID 不变。
PRAGMA foreign_keys=OFF;

CREATE TABLE repository_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    format     TEXT NOT NULL CHECK (format IN ('raw', 'maven', 'npm', 'docker', 'cargo', 'pypi', 'gomod', 'nuget')),
    type       TEXT NOT NULL CHECK (type IN ('hosted', 'proxy', 'group')),
    visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private')),
    config     TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    description TEXT NOT NULL DEFAULT '',
    online     INTEGER NOT NULL DEFAULT 1
);

INSERT INTO repository_new (id, name, format, type, visibility, config, created_at, updated_at, description, online)
SELECT id, name, format, type, visibility, config, created_at, updated_at, description, online
FROM repository;

DROP TABLE repository;
ALTER TABLE repository_new RENAME TO repository;

PRAGMA foreign_keys=ON;
