-- 动态配置入库表（FR-106，ADR-0028）：非密钥动态配置项的持久化覆盖层。
-- 键为点分路径（如 limits / protection / observability.audit），值为该节的 JSON 片段。
-- 仅存「非密钥」动态项：凭据（代理账密 / update token / OIDC·LDAP 密钥 / JWT 密钥）与 bootstrap
-- 项（server.* / data.* / DB 路径）绝不入此表——前者真源是文件 + env（ADR-0022），后者文件 only。
-- SQLite 仍是元数据唯一真源，本表经 meta 读写，不绕过直连。
CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,   -- 点分路径，如 limits / protection / observability.audit
    value_json TEXT NOT NULL,      -- 该配置节的 JSON 片段
    updated_at INTEGER NOT NULL    -- 最近更新时间（Unix 秒，UTC）
);
