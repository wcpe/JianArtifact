-- 全局出站代理池：管理员集中维护命名代理（HTTP/HTTPS、SOCKS5、Mihomo 节点协议），
-- proxy 仓库经 repository.config.outboundProxyId 按需挂载，凭据/节点配置经 AES-256-GCM 密封落库。
CREATE TABLE IF NOT EXISTS outbound_proxy (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL UNIQUE,
    kind              TEXT    NOT NULL CHECK (kind IN ('http', 'socks5', 'mihomo')),
    description       TEXT    NOT NULL DEFAULT '',
    endpoint_preview  TEXT    NOT NULL DEFAULT '',
    config_ciphertext BLOB    NOT NULL,
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT    NOT NULL DEFAULT (datetime('now'))
);
