-- 0024：仓库删除与资产物理回收的持久回滚快照
-- 引入版本：0.8.0
-- 影响：新增 repository_delete_journal 表
-- 数据处理：创建删除快照结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-104 / ADR-0024
CREATE TABLE repository_delete_journal (
  operation_id TEXT PRIMARY KEY REFERENCES asset_mutation(id) ON DELETE CASCADE,
  repository_json TEXT NOT NULL,
  acl_json TEXT NOT NULL,
  format_metadata_json TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
