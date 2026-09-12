-- 0017：资产变更 intent、前镜像与隔离回收记录
-- 引入版本：0.8.0
-- 影响：新增 asset_mutation 及其明细/回收记录表
-- 数据处理：创建变更协调结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-104 / ADR-0014
-- 0017（FR-104）：资产变更 intent、前镜像与隔离回收记录。
-- 状态机使 SQLite 事务与文件系统原子移动之间具备可恢复边界。
CREATE TABLE asset_mutation (
  id           TEXT PRIMARY KEY,
  status       TEXT NOT NULL CHECK (status IN ('prepared', 'staged', 'committing', 'completed', 'rolling_back', 'rolled_back')),
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
  error        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE asset_mutation_item (
  operation_id       TEXT NOT NULL REFERENCES asset_mutation (id) ON DELETE CASCADE,
  ordinal            INTEGER NOT NULL,
  repository_id      INTEGER NOT NULL,
  path               TEXT NOT NULL,
  before_exists      INTEGER NOT NULL DEFAULT 0,
  before_blob_hash   TEXT NOT NULL DEFAULT '',
  before_size        INTEGER NOT NULL DEFAULT 0,
  before_type        TEXT NOT NULL DEFAULT '',
  before_sha1        TEXT NOT NULL DEFAULT '',
  before_md5         TEXT NOT NULL DEFAULT '',
  before_created_at  TEXT NOT NULL DEFAULT '',
  before_updated_at  TEXT NOT NULL DEFAULT '',
  after_exists       INTEGER NOT NULL DEFAULT 0,
  after_blob_hash    TEXT NOT NULL DEFAULT '',
  after_size         INTEGER NOT NULL DEFAULT 0,
  after_type         TEXT NOT NULL DEFAULT '',
  after_sha1         TEXT NOT NULL DEFAULT '',
  after_md5          TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (operation_id, ordinal)
);
CREATE INDEX idx_asset_mutation_item_asset ON asset_mutation_item(repository_id, path);

CREATE TABLE blob_quarantine (
  operation_id    TEXT NOT NULL REFERENCES asset_mutation (id) ON DELETE CASCADE,
  blob_hash       TEXT NOT NULL,
  quarantine_path TEXT NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('staged', 'restored', 'pending_gc', 'deleted')),
  error           TEXT NOT NULL DEFAULT '',
  updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (operation_id, blob_hash)
);
CREATE INDEX idx_blob_quarantine_gc ON blob_quarantine(status, updated_at);
