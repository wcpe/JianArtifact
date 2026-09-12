-- 0035：备份包导入（URL 拉取）记录（FR-137）
-- 引入版本：0.8.0（开发中）
--
-- 与备份包生成（0034）对应，记录「从 URL 拉取备份包并导入」的每一次尝试：
-- 拉取 → 校验 → 合并 blob → 暂存 db → 写 restore.pending，需重启后才生效。
-- 状态机：queued → fetching → staging → pending_restart；任意失败置 failed。
CREATE TABLE backup_import (
  import_id          TEXT PRIMARY KEY,
  origin             TEXT NOT NULL CHECK (origin IN ('url', 'upload', 'cli')),
  status             TEXT NOT NULL CHECK (status IN ('queued', 'fetching', 'staging', 'pending_restart', 'done', 'failed')),
  source_url         TEXT NOT NULL DEFAULT '',
  operator           TEXT NOT NULL,
  overwrite          INTEGER NOT NULL DEFAULT 0 CHECK (overwrite IN (0, 1)),
  deep               INTEGER NOT NULL DEFAULT 0 CHECK (deep IN (0, 1)),
  total_bytes        INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
  fetched_bytes      INTEGER NOT NULL DEFAULT 0 CHECK (fetched_bytes >= 0),
  blob_count         INTEGER NOT NULL DEFAULT 0 CHECK (blob_count >= 0),
  package_id         TEXT NOT NULL DEFAULT '',
  error_code         TEXT NOT NULL DEFAULT '',
  error_summary      TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  finished_at        TEXT,
  restore_pending_at TEXT
);

-- 列表按最近优先。
CREATE INDEX idx_backup_import_created ON backup_import (created_at DESC);

-- 运维按状态筛选（进行中 / 失败）。
CREATE INDEX idx_backup_import_status ON backup_import (status);
