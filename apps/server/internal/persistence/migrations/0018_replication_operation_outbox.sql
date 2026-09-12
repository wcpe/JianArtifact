-- 0018：原子资产操作复制 outbox
-- 引入版本：0.8.0
-- 影响：新增 replication_operation_outbox 表及操作索引
-- 数据处理：创建 outbox 结构，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-105 / ADR-0014
-- 0018（FR-105）：原子资产操作复制 outbox。
-- repl_change 中的 operation 标记占用唯一全局 seq，outbox 保存不可拆分的完整清单。
CREATE TABLE replication_operation_outbox (
  seq              INTEGER PRIMARY KEY REFERENCES repl_change(seq),
  operation_id     TEXT NOT NULL UNIQUE,
  source_node      TEXT NOT NULL,
  version_node     TEXT NOT NULL,
  version_ts       TEXT NOT NULL,
  item_count       INTEGER NOT NULL,
  manifest_sha256  TEXT NOT NULL,
  items_json       TEXT NOT NULL,
  created_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_replication_operation_outbox_operation ON replication_operation_outbox(operation_id);
