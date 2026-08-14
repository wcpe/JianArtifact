-- 0010（FR-83）：复制变更日志表。记录本地全部写操作，供节点间复制（ADR-0013）。
-- seq 本地单调递增；node_id 标识写入节点；op 为 put/delete；entity_key 为跨节点自然键；
-- data 为变更后数据 JSON（delete 时为 tombstone）；ts 为写入节点本地时钟（RFC3339Nano）。
CREATE TABLE repl_change (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id     TEXT    NOT NULL,
  op          TEXT    NOT NULL,
  entity_type TEXT    NOT NULL,
  entity_key  TEXT    NOT NULL,
  data        TEXT    NOT NULL,
  ts          TEXT    NOT NULL
);
CREATE INDEX idx_repl_change_entity ON repl_change(entity_type, entity_key, ts);
