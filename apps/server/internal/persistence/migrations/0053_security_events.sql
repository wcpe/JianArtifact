-- 新增安全事件与独立对象明细，不回填或修改既有审计。
-- 用户与仓库 ID 是历史证据引用，不对活表设外键，也无自动留存清理。
CREATE TABLE security_event (
    sequence             INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id             TEXT NOT NULL UNIQUE,
    event_version        INTEGER NOT NULL CHECK (event_version = 1),
    event_type           TEXT NOT NULL,
    occurred_at          INTEGER NOT NULL,
    ingested_at          INTEGER NOT NULL,
    request_id           TEXT NOT NULL,
    origin_request_id    TEXT NOT NULL,
    operation_id         TEXT NOT NULL,
    task_id              TEXT NOT NULL,
    action_id            TEXT NOT NULL,
    actor_kind           TEXT NOT NULL CHECK (actor_kind IN ('user','anonymous','unknown','system','local_operator')),
    actor_user_id        INTEGER CHECK (actor_user_id > 0),
    actor_name_snapshot  TEXT NOT NULL,
    credential_id        TEXT NOT NULL,
    session_id           TEXT NOT NULL,
    auth_method          TEXT NOT NULL,
    auth_strength        TEXT NOT NULL,
    peer_ip_family       INTEGER NOT NULL,
    peer_ip              BLOB NOT NULL,
    client_ip_family     INTEGER NOT NULL,
    client_ip            BLOB NOT NULL,
    proxy_trust          TEXT NOT NULL CHECK (proxy_trust IN ('direct','trusted','invalid_forwarding','unknown')),
    source_trust_version TEXT NOT NULL,
    forwarded_chain      BLOB NOT NULL CHECK (length(forwarded_chain) <= 136 AND length(forwarded_chain) % 17 = 0),
    user_agent           TEXT NOT NULL,
    repository_id        INTEGER CHECK (repository_id > 0),
    outcome              TEXT NOT NULL CHECK (outcome IN ('succeeded','denied','failed','pending','unknown')),
    committed            TEXT NOT NULL CHECK (committed IN ('not_applicable','not_committed','committed','unknown')),
    http_status          INTEGER CHECK (http_status BETWEEN 100 AND 599),
    error_code           TEXT NOT NULL,
    CHECK ((peer_ip_family = 0 AND length(peer_ip) = 0)
        OR (peer_ip_family = 4 AND length(peer_ip) = 4) OR (peer_ip_family = 6 AND length(peer_ip) = 16)),
    CHECK ((client_ip_family = 0 AND length(client_ip) = 0)
        OR (client_ip_family = 4 AND length(client_ip) = 4) OR (client_ip_family = 6 AND length(client_ip) = 16)),
    CHECK (committed <> 'committed' OR (outcome = 'succeeded' AND operation_id <> '' AND http_status IS NULL)),
    CHECK (event_type NOT IN ('auth.session_created','asset.change_committed') OR committed = 'committed')
) STRICT;

-- 同操作每类已提交事实只能存在一条；响应/尝试不占用此键。
CREATE UNIQUE INDEX idx_security_event_committed_operation
    ON security_event (operation_id, event_type) WHERE committed = 'committed';
CREATE INDEX idx_security_event_occurred ON security_event (occurred_at, sequence);
CREATE INDEX idx_security_event_client_ip ON security_event (client_ip_family, client_ip, occurred_at, sequence);
CREATE INDEX idx_security_event_operation ON security_event (operation_id, sequence) WHERE operation_id <> '';
CREATE INDEX idx_security_event_actor ON security_event (actor_user_id, occurred_at, sequence) WHERE actor_user_id IS NOT NULL;
CREATE INDEX idx_security_event_credential ON security_event (credential_id, occurred_at, sequence) WHERE credential_id <> '';
CREATE INDEX idx_security_event_session ON security_event (session_id, occurred_at, sequence) WHERE session_id <> '';

CREATE TABLE security_event_object (
    event_sequence           INTEGER NOT NULL REFERENCES security_event (sequence),
    ordinal                  INTEGER NOT NULL CHECK (ordinal BETWEEN 1 AND 256),
    repository_id            INTEGER CHECK (repository_id > 0),
    canonical_path           TEXT NOT NULL,
    object_kind              TEXT NOT NULL,
    object_id                TEXT NOT NULL,
    digest                   TEXT NOT NULL,
    size_bytes               INTEGER CHECK (size_bytes >= 0),
    manifest_digest          TEXT NOT NULL,
    target_version           TEXT NOT NULL,
    repository_name_snapshot TEXT NOT NULL,
    PRIMARY KEY (event_sequence, ordinal)
) STRICT, WITHOUT ROWID;
