-- 0021：operation 接收审计按制品项保存同一 operation_id
-- 引入版本：0.8.0
-- 影响：重建 replication_apply_log 以支持逐实体主键
-- 数据处理：迁移既有接收审计记录；预计耗时：随审计行数增长
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-105 / ADR-0021
-- 0021（FR-105）：operation 接收审计按制品项保存同一 operation_id。
-- 旧主键只能标识一条来源 seq，无法承载同一 operation 的多个制品项，故重建为逐实体主键。
CREATE TABLE replication_apply_log_v2 (
  source_node    TEXT    NOT NULL,
  source_seq     INTEGER NOT NULL,
  operation_id   TEXT    NOT NULL DEFAULT '',
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
  PRIMARY KEY (source_node, source_seq, entity_type, entity_key)
);

INSERT INTO replication_apply_log_v2
  (source_node, source_seq, operation_id, peer_url, entity_type, entity_key, op, result, detail,
   last_error, last_error_at, first_seen_at, last_seen_at, attempt_count)
SELECT source_node, source_seq, '', peer_url, entity_type, entity_key, op, result, detail,
  last_error, last_error_at, first_seen_at, last_seen_at, attempt_count
FROM replication_apply_log;

DROP TABLE replication_apply_log;
ALTER TABLE replication_apply_log_v2 RENAME TO replication_apply_log;
CREATE INDEX idx_replication_apply_log_seen ON replication_apply_log(last_seen_at DESC);
CREATE INDEX idx_replication_apply_log_result ON replication_apply_log(result);
CREATE INDEX idx_replication_apply_log_entity ON replication_apply_log(entity_type, entity_key);
CREATE INDEX idx_replication_apply_log_operation ON replication_apply_log(operation_id, last_seen_at DESC);
