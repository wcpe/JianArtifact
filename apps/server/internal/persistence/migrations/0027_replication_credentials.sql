-- 0027：主备拉取凭据密封存储与摘要
-- 引入版本：0.8.0
-- 影响：新增 replication_pull_credential 表及活动凭据索引
-- 数据处理：创建凭据存储结构，无历史凭据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-121 / ADR-0026
CREATE TABLE replication_pull_credential (
  credential_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  salt BLOB NOT NULL,
  verifier BLOB NOT NULL,
  created_at TEXT NOT NULL,
  verified_at TEXT,
  revoked_at TEXT,
  replaces_credential_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_replication_pull_credential_node_active
  ON replication_pull_credential (node_id, revoked_at, created_at DESC);

CREATE TABLE replication_local_credential (
  credential_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  primary_url TEXT NOT NULL,
  token_cipher BLOB NOT NULL,
  created_at TEXT NOT NULL
);
