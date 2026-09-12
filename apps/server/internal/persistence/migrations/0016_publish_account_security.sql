-- 0016：发布账号防护与审计身份增强
-- 引入版本：0.8.0
-- 影响：新增用户登录限制字段和发布策略表
-- 数据处理：已有用户/策略使用默认值，无历史数据回填；预计耗时：毫秒级
-- 回滚：不支持 down migration；需要恢复升级前备份
-- 相关：FR-109 / ADR-0015
ALTER TABLE user ADD COLUMN web_login_disabled INTEGER NOT NULL DEFAULT 0;

-- 用户×仓库的原生协议发布附加策略。前缀为空表示该仓库全部路径。
CREATE TABLE publish_policy (
  user_id            INTEGER NOT NULL REFERENCES user (id) ON DELETE CASCADE,
  repository_id      INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  path_prefixes_json TEXT NOT NULL DEFAULT '[]',
  max_assets_hour    INTEGER NOT NULL DEFAULT 0,
  max_bytes_day      INTEGER NOT NULL DEFAULT 0,
  max_file_bytes     INTEGER NOT NULL DEFAULT 0,
  updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (user_id, repository_id)
);

-- 配额使用与预留持久化，避免进程重启或并发上传绕过额度。
CREATE TABLE quota_usage (
  user_id       INTEGER NOT NULL REFERENCES user (id) ON DELETE CASCADE,
  repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  window_kind   TEXT NOT NULL CHECK (window_kind IN ('hour', 'day')),
  window_start  TEXT NOT NULL,
  used_assets   INTEGER NOT NULL DEFAULT 0,
  used_bytes    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, repository_id, window_kind, window_start)
);
CREATE TABLE quota_reservation (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES user (id) ON DELETE CASCADE,
  repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
  assets        INTEGER NOT NULL,
  bytes         INTEGER NOT NULL,
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  settled_at    TEXT
);
CREATE INDEX idx_quota_reservation_scope ON quota_reservation (user_id, repository_id, settled_at);

-- 审计身份字段只保存稳定标识和来源元数据，不保存口令、令牌正文或摘要。
ALTER TABLE audit_log ADD COLUMN user_id INTEGER;
ALTER TABLE audit_log ADD COLUMN auth_source TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN token_id INTEGER;
ALTER TABLE audit_log ADD COLUMN token_name TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN source_node TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_audit_log_user ON audit_log (user_id);
CREATE INDEX idx_audit_log_auth_source ON audit_log (auth_source);
CREATE INDEX idx_audit_log_request ON audit_log (request_id);
