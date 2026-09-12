-- 0033：复制 operation 不可变接收凭据与 relay 源序号唯一性
-- 引入版本：0.8.0（开发中）
ALTER TABLE asset_mutation ADD COLUMN origin TEXT NOT NULL DEFAULT 'local'
  CHECK (origin IN ('local', 'received'));
ALTER TABLE asset_mutation ADD COLUMN receipt_stream_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE asset_mutation ADD COLUMN receipt_source_node TEXT NOT NULL DEFAULT '';
ALTER TABLE asset_mutation ADD COLUMN receipt_source_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE asset_mutation ADD COLUMN receipt_manifest_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE asset_mutation ADD COLUMN receipt_item_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE replication_operation_receipt (
  operation_id      TEXT PRIMARY KEY,
  stream_generation TEXT NOT NULL DEFAULT '',
  source_node       TEXT NOT NULL,
  source_seq        INTEGER NOT NULL DEFAULT 0 CHECK (source_seq >= 0),
  manifest_sha256   TEXT NOT NULL,
  item_count        INTEGER NOT NULL CHECK (item_count > 0),
  created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_replication_operation_receipt_source
  ON replication_operation_receipt (stream_generation, source_node, source_seq)
  WHERE source_seq > 0;

-- 0.8.0 尚未发布：上一版开发库中的 relay 行没有不可变 provenance，不能
-- 直接被新 frontier 信任。升级时清空本地中继 inbox 与父边水位，从直接父节点重建。
DELETE FROM replication_relay_blob;
DELETE FROM replication_relay_record;
DELETE FROM replication_relay_frontier;
DELETE FROM setting WHERE key LIKE 'repl:watermark:%'
  OR key LIKE 'repl:stream_generation:%'
  OR key LIKE 'repl:stream_valid:%'
  OR key LIKE 'repl:parent_source_node:%'
  OR key LIKE 'repl:parent_relay:%';

-- 一个根源 stream 的 seq 只能标识一条 record；source_node 是载荷身份，
-- 不能让同 seq 换来源后并存并被下游跨页漏读。
CREATE UNIQUE INDEX idx_replication_relay_record_stream_seq_unique
  ON replication_relay_record (stream_generation, source_seq);
