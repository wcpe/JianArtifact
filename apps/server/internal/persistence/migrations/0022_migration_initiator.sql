-- 0022：异步迁移发起人审计身份
-- 引入版本：0.8.0
-- 影响：新增 migration_task 发起人快照字段
-- 数据处理：已有任务使用空值默认值，不回填敏感信息；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-31 / ADR-0022
-- 0022（FR-31）：异步迁移完成时保留请求发起人的非敏感审计身份。
-- 不保存来源用户、口令、令牌、凭据引用或来源配置解析结果。
ALTER TABLE migration_task ADD COLUMN initiator_username TEXT NOT NULL DEFAULT '';
ALTER TABLE migration_task ADD COLUMN initiator_user_id INTEGER;
ALTER TABLE migration_task ADD COLUMN initiator_auth_source TEXT NOT NULL DEFAULT '';
