-- 0002：Raw hosted 制品资产元数据
-- 引入版本：0.3.0
-- 影响：新增制品资产表及仓库路径约束
-- 数据处理：创建资产元数据结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-13 / ADR-0002
-- 前向追加迁移，不可修改已发布的迁移文件。
-- 见 docs/specs/0.3.0-raw-hosted.md 与 docs/ARCHITECTURE.md §3-5。

-- asset：仓库内某路径的制品资产，指向内容寻址 blob（blob_hash）。
-- 内容真源在文件系统 blob；此表仅记录路径→哈希的映射与元信息。
-- (repository_id, path) 唯一：同仓库同路径覆盖写（最新版本 last-writer-wins）。
CREATE TABLE asset (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
    path          TEXT NOT NULL,
    blob_hash     TEXT NOT NULL,
    size          INTEGER NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (repository_id, path)
);
CREATE INDEX idx_asset_repository ON asset (repository_id);
