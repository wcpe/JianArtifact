-- 0003：Nexus 迁移任务主表
-- 引入版本：0.4.0
-- 影响：新增迁移任务表
-- 数据处理：创建任务结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-20 / FR-21 / FR-22 / ADR-0012
-- migration_task：Nexus 迁移任务主表（0.4.0 foundation）
-- 状态：planned / running / completed / failed / cancelled
-- 凭据仅存引用名 credential_ref，明文密钥永不入库
CREATE TABLE migration_task (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  status          TEXT NOT NULL,
  source_type     TEXT NOT NULL,
  source_config   TEXT NOT NULL DEFAULT '{}',
  credential_ref  TEXT,
  conflict_policy TEXT NOT NULL DEFAULT 'skip',
  plan_json       TEXT NOT NULL DEFAULT '{}',
  checkpoint_json TEXT NOT NULL DEFAULT '{}',
  report_json     TEXT NOT NULL DEFAULT '{}',
  error_message   TEXT,
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
  started_at      TEXT,
  finished_at     TEXT
);

CREATE INDEX idx_migration_task_status ON migration_task(status);
CREATE INDEX idx_migration_task_created ON migration_task(created_at);
