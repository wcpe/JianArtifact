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
