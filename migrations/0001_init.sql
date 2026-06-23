-- 初始化元数据库结构（五张表），字段对齐 docs/ARCHITECTURE.md §3。
-- 约定：布尔用 INTEGER（0/1），时间用 TEXT（ISO8601）默认 CURRENT_TIMESTAMP。

-- 用户表：本地账号，口令以 argon2 哈希存储，不存明文。
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL,           -- 全局角色：admin | user
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- API Token 表：供 CLI 使用，仅以哈希存储，不回显明文。
CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

-- 仓库表：格式 + 类型（hosted|proxy）+ 可见性（public|private）。
-- 上游凭据不入库明文，仅以 upstream_auth_ref 存引用，真值走配置/env。
CREATE TABLE repositories (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    format            TEXT NOT NULL,       -- maven | npm | docker | raw
    type              TEXT NOT NULL,       -- hosted | proxy
    visibility        TEXT NOT NULL,       -- public | private
    upstream_url      TEXT,
    upstream_auth_ref TEXT,
    created_at        TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 每仓库读写 ACL：permission 取 read | write。
CREATE TABLE repo_acl (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    permission TEXT NOT NULL,              -- read | write
    FOREIGN KEY (repo_id) REFERENCES repositories (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

-- 制品索引表：DB 仅存索引与多校验和，blob 本体在文件系统。
CREATE TABLE artifacts (
    id           TEXT PRIMARY KEY,
    repo_id      TEXT NOT NULL,
    path         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    sha1         TEXT NOT NULL,
    md5          TEXT NOT NULL,
    sha512       TEXT NOT NULL,
    content_type TEXT,
    cached       INTEGER NOT NULL DEFAULT 0,  -- proxy 缓存制品标记
    created_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (repo_id) REFERENCES repositories (id) ON DELETE CASCADE
);

-- 索引：ACL 按 (repo_id, user_id) 查询，制品按 (repo_id, path) 定位。
CREATE INDEX idx_repo_acl_repo_user ON repo_acl (repo_id, user_id);
CREATE UNIQUE INDEX idx_artifacts_repo_path ON artifacts (repo_id, path);
