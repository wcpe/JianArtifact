-- 0013：记录每个复制实体已接受的最新 LWW 版本，防止多对端旧变更覆盖新变更。
CREATE TABLE repl_entity_version (
  entity_type TEXT NOT NULL,
  entity_key  TEXT NOT NULL,
  node_id     TEXT NOT NULL,
  ts          TEXT NOT NULL,
  PRIMARY KEY (entity_type, entity_key)
);

-- 从既有本地变更日志回填当前版本，保持升级兼容。
INSERT INTO repl_entity_version (entity_type, entity_key, node_id, ts)
SELECT c.entity_type, c.entity_key, c.node_id, c.ts
FROM repl_change c
WHERE NOT EXISTS (
  SELECT 1
  FROM repl_change newer
  WHERE newer.entity_type = c.entity_type
    AND newer.entity_key = c.entity_key
    AND (
      newer.ts > c.ts
      OR (newer.ts = c.ts AND newer.node_id > c.node_id)
      OR (newer.ts = c.ts AND newer.node_id = c.node_id AND newer.seq > c.seq)
    )
);
