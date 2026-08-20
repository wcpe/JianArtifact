-- 0014：复制接收审计日志。仅记录本节点接收远端变更的应用结果，不写入 repl_change。
CREATE TABLE replication_apply_log (
  source_node    TEXT    NOT NULL,
  source_seq     INTEGER NOT NULL,
  peer_url       TEXT    NOT NULL,
  entity_type    TEXT    NOT NULL,
  entity_key     TEXT    NOT NULL,
  op             TEXT    NOT NULL,
  result         TEXT    NOT NULL,
  detail         TEXT    NOT NULL DEFAULT '',
  last_error     TEXT    NOT NULL DEFAULT '',
  last_error_at  TEXT    NOT NULL DEFAULT '',
  first_seen_at  TEXT    NOT NULL,
  last_seen_at   TEXT    NOT NULL,
  attempt_count  INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (source_node, source_seq)
);
CREATE INDEX idx_replication_apply_log_seen ON replication_apply_log(last_seen_at DESC);
CREATE INDEX idx_replication_apply_log_result ON replication_apply_log(result);
CREATE INDEX idx_replication_apply_log_entity ON replication_apply_log(entity_type, entity_key);
