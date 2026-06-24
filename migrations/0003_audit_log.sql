-- 审计日志表（FR-31，ADR-0015）：只记元数据级安全 / 管理事件，不记请求体与制品内容。
-- 约定沿用既有表：时间用 TEXT（ISO8601）默认 CURRENT_TIMESTAMP；可空字段允许 NULL。
-- 凭据 / 密钥（口令、Token、JWT、上游凭据）一律不入此表，actor 只记用户名。

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,  -- UTC 事件时间
    actor       TEXT NOT NULL,                            -- 用户名或 anonymous，不记凭据
    actor_kind  TEXT NOT NULL,                            -- session | token | basic | anonymous
    request_id  TEXT,                                     -- 关联 api 既有请求 ID（x-request-id）
    source_ip   TEXT,                                     -- 来源 IP（连接 IP）
    action      TEXT NOT NULL,                            -- 事件枚举：login / token.issue / repo.create 等
    target_repo TEXT,                                     -- 受影响仓库名（可空）
    target      TEXT,                                     -- 受影响对象坐标 / 路径（可空）
    result      TEXT NOT NULL,                            -- success | denied | error
    detail      TEXT                                      -- 结构化补充（JSON 文本），禁含凭据 / 隐私
);

-- 查询按时间倒序浏览，并支持按动作 / 仓库过滤；保留期轮转按 ts 删旧。
CREATE INDEX idx_audit_log_ts ON audit_log (ts DESC);
CREATE INDEX idx_audit_log_action ON audit_log (action);
CREATE INDEX idx_audit_log_target_repo ON audit_log (target_repo);
