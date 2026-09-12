-- 0029：同步诊断事件追加式安全投影
-- 引入版本：0.8.0
-- 影响：新增 replication_sync_event 表及同步诊断索引
-- 数据处理：创建同步诊断结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-119 / ADR-0023
CREATE TABLE replication_sync_event (
  event_id TEXT PRIMARY KEY,
  trigger_type TEXT NOT NULL,
  actor_username TEXT NOT NULL,
  actor_user_id INTEGER,
  actor_auth_source TEXT NOT NULL,
  direction TEXT NOT NULL,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  result TEXT NOT NULL,
  from_seq INTEGER NOT NULL,
  to_seq INTEGER NOT NULL,
  observed_primary_seq INTEGER NOT NULL,
  changes INTEGER NOT NULL,
  applied INTEGER NOT NULL,
  failed INTEGER NOT NULL,
  blobs INTEGER NOT NULL,
  stage TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  error_summary TEXT NOT NULL DEFAULT '',
  recovery_hint TEXT NOT NULL DEFAULT ''
  ,sync_log_id INTEGER NOT NULL
);
CREATE INDEX idx_replication_sync_event_time ON replication_sync_event (started_at DESC, event_id DESC);
