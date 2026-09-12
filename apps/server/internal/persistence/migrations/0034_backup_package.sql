-- 0034：节点备份包登记（FR-132）
-- 引入版本：0.8.0（开发中）
--
-- 备份包是搬迁的传输单位，取代原集群复制通道。本表只登记元数据（含计数与摘要），
-- 包体本身落在 ${JIAN_DATA_DIR}/backups/<package_id>.tar.gz，边车 .json 由归档层负责。
CREATE TABLE backup_package (
  package_id        TEXT PRIMARY KEY,
  mode              TEXT NOT NULL CHECK (mode IN ('hot', 'frozen')),
  base_package_id   TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL CHECK (status IN ('queued', 'snapshotting', 'packing', 'done', 'failed')),
  label             TEXT NOT NULL DEFAULT '',
  size_bytes        INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
  counts_json       TEXT NOT NULL DEFAULT '{}',
  node_id           TEXT NOT NULL DEFAULT '',
  app_version       TEXT NOT NULL DEFAULT '',
  db_schema_version INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL,
  finished_at       TEXT,
  error_summary     TEXT NOT NULL DEFAULT ''
);

-- 列表按最近优先。
CREATE INDEX idx_backup_package_created ON backup_package (created_at DESC);

-- 增量差包引用基线：删除基线前需查是否仍有派生包依赖。
CREATE INDEX idx_backup_package_base ON backup_package (base_package_id)
  WHERE base_package_id <> '';
