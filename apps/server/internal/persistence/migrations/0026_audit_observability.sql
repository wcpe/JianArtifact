-- 0026：当前节点统一审计读模型与共享风险确认状态
-- 引入版本：0.8.0
-- 影响：新增审计关联字段、复制应用事件及风险确认表
-- 数据处理：已有审计关联标识使用空值默认值，无历史事件回填；预计耗时：随审计行数增长
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-118 / ADR-0002
ALTER TABLE audit_log ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_audit_log_correlation ON audit_log (correlation_id, ts DESC);

-- 每次复制接收尝试或状态转换都只追加一行；不保存 peer URL、原始 detail 或凭据。
CREATE TABLE replication_apply_event (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  source_node        TEXT NOT NULL,
  source_seq         INTEGER NOT NULL,
  operation_id       TEXT NOT NULL DEFAULT '',
  entity_type        TEXT NOT NULL,
  entity_key         TEXT NOT NULL,
  op                 TEXT NOT NULL,
  result             TEXT NOT NULL,
  error_class        TEXT NOT NULL DEFAULT '',
  occurred_at        TEXT NOT NULL,
  source_actor       TEXT NOT NULL DEFAULT '',
  source_user_id     INTEGER,
  source_auth_source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_replication_apply_event_occurred ON replication_apply_event (occurred_at DESC, id DESC);
CREATE INDEX idx_replication_apply_event_operation ON replication_apply_event (operation_id, occurred_at DESC);

-- 确认行只属于当前节点；身份为快照，不能因用户删除而消失。
CREATE TABLE audit_attention_ack (
  source                       TEXT NOT NULL CHECK (source IN ('audit', 'replication')),
  source_event_id              INTEGER NOT NULL,
  acknowledged_by_user_id      INTEGER,
  acknowledged_by_username     TEXT NOT NULL,
  acknowledged_by_auth_source  TEXT NOT NULL,
  acknowledged_at              TEXT NOT NULL,
  PRIMARY KEY (source, source_event_id)
);
CREATE INDEX idx_audit_attention_ack_time ON audit_attention_ack (acknowledged_at DESC);
