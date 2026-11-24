-- 安全治理：持久事故处置动作、事故限制与证据保全。
CREATE TABLE IF NOT EXISTS security_action (
    action_id        TEXT PRIMARY KEY,
    action_type      TEXT NOT NULL CHECK (action_type IN
        ('revoke_credential','suspend_user','restrict','lift_restriction','hold_evidence','release_hold')),
    state            TEXT NOT NULL DEFAULT 'previewed' CHECK (state IN
        ('previewed','confirmed','expired','applied','partially_applied','rejected')),
    actor_user_id    INTEGER NOT NULL REFERENCES user (id),
    reason           TEXT NOT NULL,
    action_digest    TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    expires_at       INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    finalized_at     INTEGER,
    UNIQUE (actor_user_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_security_action_actor ON security_action (actor_user_id, created_at);

CREATE TABLE IF NOT EXISTS security_action_item (
    action_id  TEXT NOT NULL REFERENCES security_action(action_id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    target     TEXT NOT NULL,
    outcome    TEXT NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending','applied','rejected','skipped')),
    detail     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (action_id, seq)
);

CREATE TABLE IF NOT EXISTS incident_restriction (
    restriction_id TEXT PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('instance_write_lock','ip_cidr','repository','path','version')),
    target         TEXT NOT NULL DEFAULT '',
    effects        TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','lifted','expired')),
    version        INTEGER NOT NULL DEFAULT 1,
    effective_at   INTEGER NOT NULL,
    expires_at     INTEGER,
    action_id      TEXT NOT NULL REFERENCES security_action(action_id),
    lifted_action  TEXT,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_incident_restriction_active ON incident_restriction (kind, state);
-- 同一时刻仅一条活跃的实例写锁。
CREATE UNIQUE INDEX IF NOT EXISTS idx_incident_restriction_instance ON incident_restriction (kind) WHERE kind='instance_write_lock' AND state='active';

CREATE TABLE IF NOT EXISTS evidence_hold (
    hold_id     TEXT PRIMARY KEY,
    object_id   TEXT NOT NULL,
    digest      TEXT NOT NULL,
    byte_state  TEXT NOT NULL DEFAULT 'unknown' CHECK (byte_state IN ('present','metadata_only','unknown')),
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','released')),
    action_id   TEXT NOT NULL REFERENCES security_action(action_id),
    created_at  INTEGER NOT NULL,
    released_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_evidence_hold_digest ON evidence_hold (digest, state);
