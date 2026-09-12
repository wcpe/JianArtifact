-- 0031：级联复制的逐跳 relay 日志与凭据来源绑定
-- 引入版本：0.8.0（开发中）
-- 说明：relay 节点保存上游原始 v2 record，按来源 stream 与源序号向直接下级继续提供。
-- 不使用本地 repl_change.seq 作为下游水位，避免多跳转发时改变源顺序。
CREATE TABLE replication_relay_record (
  stream_generation TEXT NOT NULL,
  source_node       TEXT NOT NULL,
  source_seq        INTEGER NOT NULL,
  record_type       TEXT NOT NULL,
  record_json       TEXT NOT NULL,
  created_at        TEXT NOT NULL,
  PRIMARY KEY (stream_generation, source_node, source_seq)
);
CREATE INDEX idx_replication_relay_record_stream_seq
  ON replication_relay_record (stream_generation, source_seq);

-- relay 日志引用的 blob 在下游未追平前必须保留。当前开发实现采用保守保留策略，
-- 后续可在直接子边确认水位后做可证明安全的定向回收。
CREATE TABLE replication_relay_blob (
  stream_generation TEXT NOT NULL,
  source_node       TEXT NOT NULL,
  source_seq        INTEGER NOT NULL,
  blob_hash         TEXT NOT NULL,
  PRIMARY KEY (stream_generation, source_node, source_seq, blob_hash),
  FOREIGN KEY (stream_generation, source_node, source_seq)
    REFERENCES replication_relay_record(stream_generation, source_node, source_seq)
    ON DELETE CASCADE
);
CREATE INDEX idx_replication_relay_blob_hash ON replication_relay_blob (blob_hash);

-- 旧版本创建的凭据没有来源节点字段；空值按旧开发夹具兼容，新增凭据必须写入来源节点。
ALTER TABLE replication_pull_credential
  ADD COLUMN source_node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE replication_pull_credential
  ADD COLUMN stream_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE replication_local_credential
  ADD COLUMN stream_generation TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_replication_pull_credential_source_node
  ON replication_pull_credential (source_node_id, stream_generation, node_id, revoked_at, created_at DESC);
