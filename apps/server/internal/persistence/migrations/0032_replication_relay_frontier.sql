-- 0032：relay 仅向直接下级暴露已经整轮确认的连续前缀
-- 引入版本：0.8.0（开发中）
CREATE TABLE replication_relay_frontier (
  stream_generation TEXT PRIMARY KEY,
  forwardable_seq   INTEGER NOT NULL DEFAULT 0 CHECK (forwardable_seq >= 0),
  updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
