-- FR-58: 性能索引补齐——加速按 (repository_id, path) 前缀查询与全局 path 搜索。
CREATE INDEX IF NOT EXISTS idx_asset_repo_path ON asset(repository_id, path);
CREATE INDEX IF NOT EXISTS idx_asset_path ON asset(path);
