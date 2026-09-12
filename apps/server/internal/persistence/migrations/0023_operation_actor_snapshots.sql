-- 0023：operation 与接收审计操作者快照
-- 引入版本：0.8.0
-- 影响：新增复制 outbox 与接收审计的操作者快照字段
-- 数据处理：已有记录使用空值默认值，不回填凭据；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-105 / ADR-0020
ALTER TABLE replication_operation_outbox ADD COLUMN actor TEXT NOT NULL DEFAULT '';
ALTER TABLE replication_operation_outbox ADD COLUMN actor_user_id INTEGER;
ALTER TABLE replication_operation_outbox ADD COLUMN actor_auth_source TEXT NOT NULL DEFAULT '';

ALTER TABLE replication_apply_log ADD COLUMN source_actor TEXT NOT NULL DEFAULT '';
ALTER TABLE replication_apply_log ADD COLUMN source_user_id INTEGER;
ALTER TABLE replication_apply_log ADD COLUMN source_auth_source TEXT NOT NULL DEFAULT '';
