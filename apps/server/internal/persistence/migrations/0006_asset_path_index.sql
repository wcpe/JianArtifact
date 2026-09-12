-- 0006：asset 路径查询性能索引
-- 引入版本：0.6.0
-- 影响：新增 asset 仓库路径与全局路径索引
-- 数据处理：扫描既有 asset 行建立索引，无数据回填；预计耗时：随 asset 行数增长
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-58 / ADR-0002
-- FR-58: 性能索引补齐——加速按 (repository_id, path) 前缀查询与全局 path 搜索。
CREATE INDEX IF NOT EXISTS idx_asset_repo_path ON asset(repository_id, path);
CREATE INDEX IF NOT EXISTS idx_asset_path ON asset(path);
