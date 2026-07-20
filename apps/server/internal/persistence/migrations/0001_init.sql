-- 0001_init：认证授权与仓库管理的初始 schema（0.2.0）。
-- 前向追加迁移，不可修改已发布的迁移文件；后续变更新增 000N_*.sql。
-- 见 docs/adr/0002-sqlite-filesystem-storage.md 与 docs/ARCHITECTURE.md §3。

-- 用户：账号、argon2 口令哈希、角色与状态。明文口令不落库。
CREATE TABLE user (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- API Token：仅存 sha256 摘要，明文仅签发时返回一次；吊销为置 revoked_at。
CREATE TABLE api_token (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES user (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_digest TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    revoked_at   TEXT
);
CREATE INDEX idx_api_token_user ON api_token (user_id);

-- 已吊销的会话 JWT：按 jti 记录直至其过期，登出后拒绝复用。
CREATE TABLE revoked_token (
    jti        TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- 仓库：格式 × 类型 × 可见性 + 结构化配置（上游 URL、成员列表等，JSON 文本）。
CREATE TABLE repository (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    format     TEXT NOT NULL CHECK (format IN ('raw', 'maven', 'npm')),
    type       TEXT NOT NULL CHECK (type IN ('hosted', 'proxy', 'group')),
    visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private')),
    config     TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ACL：主体（用户）× 仓库 × 动作（读/写/管理）。
CREATE TABLE acl (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repository_id INTEGER NOT NULL REFERENCES repository (id) ON DELETE CASCADE,
    subject_id    INTEGER NOT NULL REFERENCES user (id) ON DELETE CASCADE,
    action        TEXT NOT NULL CHECK (action IN ('read', 'write', 'admin')),
    UNIQUE (repository_id, subject_id, action)
);
CREATE INDEX idx_acl_repository ON acl (repository_id);
CREATE INDEX idx_acl_subject ON acl (subject_id);
