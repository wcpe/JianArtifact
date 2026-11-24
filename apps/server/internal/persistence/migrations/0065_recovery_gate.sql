-- 安全治理：恢复隔离闸与库内安全状态水位。
-- recovery_gate：单实例单行；normal → isolated → reconciling → ready_for_review → normal，
-- 任一核对失败回 isolated 并保留原因。
CREATE TABLE IF NOT EXISTS recovery_gate (
    row_id              INTEGER PRIMARY KEY CHECK (row_id = 1),
    state               TEXT NOT NULL DEFAULT 'normal' CHECK (state IN ('normal','isolated','reconciling','ready_for_review')),
    restore_id          TEXT,
    security_epoch      INTEGER NOT NULL DEFAULT 0,
    checkpoint_status   TEXT NOT NULL DEFAULT 'none' CHECK (checkpoint_status IN ('none','verified','stale','missing','mismatch')),
    verified_watermark  INTEGER NOT NULL DEFAULT 0,
    gap_status          TEXT NOT NULL DEFAULT 'none' CHECK (gap_status IN ('none','gap','unknown')),
    reason              TEXT NOT NULL DEFAULT '',
    updated_at          INTEGER NOT NULL
);
INSERT OR IGNORE INTO recovery_gate (row_id, state, updated_at) VALUES (1, 'normal', 0);

-- security_state_version：受保护安全状态的单调水位；
-- 事故处置/限制/保全等安全写在同一最终事务内递增（本迁移先纳事故域）。
CREATE TABLE IF NOT EXISTS security_state_watermark (
    row_id  INTEGER PRIMARY KEY CHECK (row_id = 1),
    version INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO security_state_watermark (row_id, version) VALUES (1, 0);
