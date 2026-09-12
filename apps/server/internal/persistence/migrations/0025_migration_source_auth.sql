-- 0025：online REST 迁移认证类型与 AES-256-GCM 密文
-- 引入版本：0.8.0
-- 影响：新增 migration_task 来源认证字段
-- 数据处理：已有任务认证字段保持 NULL，不回填明文材料；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-116 / ADR-0025
-- 明文认证材料不得写入 source_config、计划、断点、报告或错误字段。
ALTER TABLE migration_task ADD COLUMN source_auth_type TEXT;
ALTER TABLE migration_task ADD COLUMN source_auth_ciphertext BLOB;
