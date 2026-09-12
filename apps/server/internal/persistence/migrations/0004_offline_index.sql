-- 0004：离线目录（Nexus blob）持久化索引
-- 引入版本：0.4.0
-- 影响：新增离线目录索引表
-- 数据处理：创建索引结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-20 / FR-21 / ADR-0012
-- 离线目录（Nexus blob）持久化索引：一次扫描，多次迁移复用
CREATE TABLE offline_dir_index (
  root_path      TEXT PRIMARY KEY,
  status         TEXT NOT NULL DEFAULT 'idle',
  mode           TEXT NOT NULL DEFAULT 'full',
  total_entries  INTEGER NOT NULL DEFAULT 0,
  scanned_props  INTEGER NOT NULL DEFAULT 0,
  repo_count     INTEGER NOT NULL DEFAULT 0,
  message        TEXT NOT NULL DEFAULT '',
  error_message  TEXT,
  started_at     TEXT,
  finished_at    TEXT,
  updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE offline_dir_index_entry (
  root_path   TEXT NOT NULL,
  repo        TEXT NOT NULL,
  asset_path  TEXT NOT NULL,
  bytes_path  TEXT NOT NULL,
  prop_path   TEXT NOT NULL,
  prop_mtime  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (root_path, repo, asset_path)
);

CREATE INDEX idx_offline_dir_index_entry_repo
  ON offline_dir_index_entry(root_path, repo);

CREATE INDEX idx_offline_dir_index_entry_prop
  ON offline_dir_index_entry(root_path, prop_path);
