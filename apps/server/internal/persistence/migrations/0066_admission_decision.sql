-- FR-169 准入裁决：放宽候选状态机（awaiting_review/published），记录人工驳回与提升资产号。
CREATE TABLE private_admission_candidate_v2 (
    operation_id TEXT PRIMARY KEY,
    object_key TEXT NOT NULL UNIQUE,
    boot_id TEXT NOT NULL,
    repository_id INTEGER NOT NULL,
    actor_user_id INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    auth_method TEXT NOT NULL,
    account_epoch INTEGER NOT NULL,
    origin_request_id TEXT NOT NULL,
    client_ip TEXT NOT NULL,
    proxy_trust TEXT NOT NULL,
    source_trust_version TEXT NOT NULL,
    canonical_path TEXT NOT NULL,
    content_type TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('receiving','staged','awaiting_review','published','rejected','expired')),
    target_version INTEGER NOT NULL DEFAULT 1 CHECK (target_version >= 1),
    manifest_version INTEGER,
    manifest_digest TEXT,
    blob_hash TEXT,
    size_bytes INTEGER,
    staged_event_id TEXT UNIQUE,
    error_code TEXT NOT NULL DEFAULT '',
    expires_unix INTEGER NOT NULL,
    published_asset_id INTEGER,
    CHECK (length(object_key) = 32 AND object_key NOT GLOB '*[^0-9a-f]*'),
    CHECK (repository_id > 0 AND actor_user_id > 0 AND length(session_id) > 0 AND account_epoch >= 0),
    CHECK (proxy_trust IN ('direct','trusted')),
    CHECK (error_code IN ('','admission_integrity_failed','manual_reject')),
    CHECK ((manifest_version IS NULL AND manifest_digest IS NULL AND blob_hash IS NULL AND size_bytes IS NULL AND staged_event_id IS NULL)
        OR (manifest_version = 1 AND manifest_digest IS NOT NULL AND blob_hash IS NOT NULL AND size_bytes IS NOT NULL AND size_bytes >= 0 AND staged_event_id IS NOT NULL)),
    CHECK (state NOT IN ('staged','awaiting_review','published') OR staged_event_id IS NOT NULL),
    CHECK (state != 'receiving' OR staged_event_id IS NULL),
    CHECK (state != 'published' OR published_asset_id IS NOT NULL)
);
INSERT INTO private_admission_candidate_v2
    SELECT operation_id, object_key, boot_id, repository_id, actor_user_id, session_id, auth_method, account_epoch,
           origin_request_id, client_ip, proxy_trust, source_trust_version, canonical_path, content_type,
           state, target_version, manifest_version, manifest_digest, blob_hash, size_bytes, staged_event_id,
           error_code, expires_unix, NULL
    FROM private_admission_candidate;
DROP TABLE private_admission_candidate;
ALTER TABLE private_admission_candidate_v2 RENAME TO private_admission_candidate;
