package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// 安全治理 持久事故处置：动作生命周期（预览→确认→执行）、事故限制、证据保全。

const (
	SecurityActionStatePreviewed = "previewed"
	SecurityActionStateConfirmed = "confirmed"
	SecurityActionStateExpired   = "expired"
	SecurityActionStateApplied   = "applied"
	SecurityActionStatePartially = "partially_applied"
	SecurityActionStateRejected  = "rejected"
	RestrictionStateActive       = "active"
	RestrictionStateLifted       = "lifted"
	RestrictionKindInstanceLock  = "instance_write_lock"
	EvidenceHoldStateActive      = "active"
	EvidenceHoldBytePresent      = "present"
	EvidenceHoldByteMetadataOnly = "metadata_only"
	EvidenceHoldByteUnknown      = "unknown"
)

var ErrSecurityActionRejected = errors.New("security action rejected")

type SecurityAction struct {
	ActionID       string `db:"action_id"`
	ActionType     string `db:"action_type"`
	State          string `db:"state"`
	ActorUserID    int64  `db:"actor_user_id"`
	Reason         string `db:"reason"`
	ActionDigest   string `db:"action_digest"`
	IdempotencyKey string `db:"idempotency_key"`
	ExpiresAt      int64  `db:"expires_at"`
	CreatedAt      int64  `db:"created_at"`
	FinalizedAt    *int64 `db:"finalized_at"`
}

type SecurityActionItem struct {
	ActionID string `db:"action_id"`
	Seq      int    `db:"seq"`
	Target   string `db:"target"`
	Outcome  string `db:"outcome"`
	Detail   string `db:"detail"`
}

type IncidentRestriction struct {
	RestrictionID string  `db:"restriction_id"`
	Kind          string  `db:"kind"`
	Target        string  `db:"target"`
	Effects       string  `db:"effects"`
	State         string  `db:"state"`
	Version       int     `db:"version"`
	EffectiveAt   int64   `db:"effective_at"`
	ExpiresAt     *int64  `db:"expires_at"`
	ActionID      string  `db:"action_id"`
	LiftedAction  *string `db:"lifted_action"`
	CreatedAt     int64   `db:"created_at"`
}

type EvidenceHold struct {
	HoldID     string `db:"hold_id"`
	ObjectID   string `db:"object_id"`
	Digest     string `db:"digest"`
	ByteState  string `db:"byte_state"`
	SizeBytes  int64  `db:"size_bytes"`
	State      string `db:"state"`
	ActionID   string `db:"action_id"`
	CreatedAt  int64  `db:"created_at"`
	ReleasedAt *int64 `db:"released_at"`
}

func InsertSecurityActionTx(ctx context.Context, tx *sqlx.Tx, a SecurityAction, items []SecurityActionItem) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO security_action
		(action_id, action_type, state, actor_user_id, reason, action_digest, idempotency_key, expires_at, created_at, finalized_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ActionID, a.ActionType, a.State, a.ActorUserID, a.Reason, a.ActionDigest, a.IdempotencyKey, a.ExpiresAt, a.CreatedAt, a.FinalizedAt); err != nil {
		return ErrSecurityActionRejected
	}
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO security_action_item (action_id, seq, target, outcome, detail) VALUES (?,?,?,?,?)`,
			a.ActionID, it.Seq, it.Target, it.Outcome, it.Detail); err != nil {
			return ErrSecurityActionRejected
		}
	}
	return nil
}

// SecurityActionByKeyTx 幂等回放：同主体同键命中既有动作。
func SecurityActionByKeyTx(ctx context.Context, tx *sqlx.Tx, actorUserID int64, key string) (*SecurityAction, error) {
	var a SecurityAction
	err := tx.GetContext(ctx, &a, `SELECT action_id, action_type, state, actor_user_id, reason, action_digest,
		idempotency_key, expires_at, created_at, finalized_at FROM security_action
		WHERE actor_user_id=? AND idempotency_key=?`, actorUserID, key)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func SecurityActionByIDTx(ctx context.Context, tx *sqlx.Tx, actionID string) (*SecurityAction, error) {
	var a SecurityAction
	err := tx.GetContext(ctx, &a, `SELECT action_id, action_type, state, actor_user_id, reason, action_digest,
		idempotency_key, expires_at, created_at, finalized_at FROM security_action WHERE action_id=?`, actionID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func SecurityActionItemsTx(ctx context.Context, tx *sqlx.Tx, actionID string) ([]SecurityActionItem, error) {
	var items []SecurityActionItem
	err := tx.SelectContext(ctx, &items, `SELECT action_id, seq, target, outcome, detail
		FROM security_action_item WHERE action_id=? ORDER BY seq`, actionID)
	return items, err
}

// FinalizeSecurityActionTx 终态落写；in-place 过期：过期未终结先转 expired。
func FinalizeSecurityActionTx(ctx context.Context, tx *sqlx.Tx, actionID, state string, now int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE security_action SET state=?, finalized_at=? WHERE action_id=? AND state IN ('previewed','confirmed')`,
		state, now, actionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecurityActionRejected
	}
	return nil
}

// SetSecurityActionItemTx 逐项结果。
func SetSecurityActionItemTx(ctx context.Context, tx *sqlx.Tx, actionID string, seq int, outcome, detail string) error {
	res, err := tx.ExecContext(ctx, `UPDATE security_action_item SET outcome=?, detail=? WHERE action_id=? AND seq=?`,
		outcome, detail, actionID, seq)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecurityActionRejected
	}
	return nil
}

// InstanceWriteLockedTx 当前是否存在活跃的实例业务写锁。
func InstanceWriteLockedTx(ctx context.Context, tx *sqlx.Tx, now int64) (bool, error) {
	var n int
	err := tx.GetContext(ctx, &n, `SELECT count(*) FROM incident_restriction
		WHERE kind='instance_write_lock' AND state='active'`)
	return n > 0, err
}

// InstanceWriteLocked 非事务读：供入口裁决。
func InstanceWriteLocked(ctx context.Context, db *sqlx.DB) (bool, error) {
	var n int
	err := db.GetContext(ctx, &n, `SELECT count(*) FROM incident_restriction WHERE kind='instance_write_lock' AND state='active'`)
	return n > 0, err
}

func InsertIncidentRestrictionTx(ctx context.Context, tx *sqlx.Tx, r IncidentRestriction) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO incident_restriction
		(restriction_id, kind, target, effects, state, version, effective_at, expires_at, action_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.RestrictionID, r.Kind, r.Target, r.Effects, r.State, r.Version, r.EffectiveAt, r.ExpiresAt, r.ActionID, r.CreatedAt)
	return err
}

func ActiveIncidentRestrictionTx(ctx context.Context, tx *sqlx.Tx, restrictionID string) (*IncidentRestriction, error) {
	var r IncidentRestriction
	err := tx.GetContext(ctx, &r, `SELECT restriction_id, kind, target, effects, state, version, effective_at,
		expires_at, action_id, lifted_action, created_at FROM incident_restriction
		WHERE restriction_id=? AND state='active'`, restrictionID)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func LiftIncidentRestrictionTx(ctx context.Context, tx *sqlx.Tx, restrictionID, liftActionID string, now int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE incident_restriction SET state='lifted', lifted_action=?
		WHERE restriction_id=? AND state='active'`, liftActionID, restrictionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecurityActionRejected
	}
	return nil
}

func InsertEvidenceHoldTx(ctx context.Context, tx *sqlx.Tx, h EvidenceHold) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO evidence_hold
		(hold_id, object_id, digest, byte_state, size_bytes, state, action_id, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		h.HoldID, h.ObjectID, h.Digest, h.ByteState, h.SizeBytes, h.State, h.ActionID, h.CreatedAt)
	return err
}

// EvidenceHoldByDigestTx 摘要防重：同 digest 活跃保全返回既有持有。
func EvidenceHoldByDigestTx(ctx context.Context, tx *sqlx.Tx, digest string) (*EvidenceHold, error) {
	var h EvidenceHold
	err := tx.GetContext(ctx, &h, `SELECT hold_id, object_id, digest, byte_state, size_bytes, state, action_id, created_at, released_at
		FROM evidence_hold WHERE digest=? AND state='active' ORDER BY created_at DESC LIMIT 1`, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func ReleaseEvidenceHoldTx(ctx context.Context, tx *sqlx.Tx, holdID, actionID string, now int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE evidence_hold SET state='released', released_at=?, action_id=?
		WHERE hold_id=? AND state='active'`, now, actionID, holdID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecurityActionRejected
	}
	return nil
}

// ── 恢复隔离闸与安全状态水位─────────────────────────

type RecoveryGate struct {
	State             string  `db:"state"`
	RestoreID         *string `db:"restore_id"`
	SecurityEpoch     int64   `db:"security_epoch"`
	CheckpointStatus  string  `db:"checkpoint_status"`
	VerifiedWatermark int64   `db:"verified_watermark"`
	GapStatus         string  `db:"gap_status"`
	Reason            string  `db:"reason"`
	UpdatedAt         int64   `db:"updated_at"`
}

const (
	RecoveryGateNormal         = "normal"
	RecoveryGateIsolated       = "isolated"
	RecoveryGateReconciling    = "reconciling"
	RecoveryGateReadyForReview = "ready_for_review"
	CheckpointNone             = "none"
	CheckpointVerified         = "verified"
	CheckpointStale            = "stale"
	CheckpointMissing          = "missing"
	CheckpointMismatch         = "mismatch"
)

func RecoveryGateTx(ctx context.Context, tx *sqlx.Tx) (*RecoveryGate, error) {
	var g RecoveryGate
	if err := tx.GetContext(ctx, &g, `SELECT state, restore_id, security_epoch, checkpoint_status,
		verified_watermark, gap_status, reason, updated_at FROM recovery_gate WHERE row_id=1`); err != nil {
		return nil, err
	}
	return &g, nil
}

// SetRecoveryGateTx 状态推进由领域裁决（合法迁移在调用方保证）。
func SetRecoveryGateTx(ctx context.Context, tx *sqlx.Tx, g RecoveryGate) error {
	res, err := tx.ExecContext(ctx, `UPDATE recovery_gate SET state=?, restore_id=?, security_epoch=?,
		checkpoint_status=?, verified_watermark=?, gap_status=?, reason=?, updated_at=? WHERE row_id=1`,
		g.State, g.RestoreID, g.SecurityEpoch, g.CheckpointStatus, g.VerifiedWatermark, g.GapStatus, g.Reason, g.UpdatedAt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSecurityActionRejected
	}
	return nil
}

// BumpSecurityWatermarkTx 受保护安全写在同一最终事务内抬水位。
func BumpSecurityWatermarkTx(ctx context.Context, tx *sqlx.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE security_state_watermark SET version=version+1 WHERE row_id=1`); err != nil {
		return 0, err
	}
	var v int64
	if err := tx.GetContext(ctx, &v, `SELECT version FROM security_state_watermark WHERE row_id=1`); err != nil {
		return 0, err
	}
	return v, nil
}

func SecurityWatermarkTx(ctx context.Context, tx *sqlx.Tx) (int64, error) {
	var v int64
	if err := tx.GetContext(ctx, &v, `SELECT version FROM security_state_watermark WHERE row_id=1`); err != nil {
		return 0, err
	}
	return v, nil
}
