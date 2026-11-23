package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
)

const SecurityEventPasswordChanged = "auth.password_changed"

var (
	ErrPasswordVersionConflict = errors.New("账号或口令版本已变更")
	ErrPasswordCommitUnknown   = errors.New("改密提交结果未确认")
)

// CredentialPasswordSnapshot 只供改密服务比较，不能用于对外响应或审计。
type CredentialPasswordSnapshot struct {
	User
	Epoch int64 `db:"account_epoch"`
}

const passwordSnapshotSQL = `SELECT id, username, password_hash, role, status, web_login_disabled, created_at, account_epoch FROM user WHERE id=?`

func (r *UserRepo) ReadCredentialPassword(ctx context.Context, id int64) (CredentialPasswordSnapshot, error) {
	var snapshot CredentialPasswordSnapshot
	err := r.db.GetContext(ctx, &snapshot, passwordSnapshotSQL, id)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return snapshot, err
}

// CredentialPasswordChange 由服务端构建；NewHash 为空代表已在锁外证实相同口令。
type CredentialPasswordChange struct {
	Event   SecurityEvent
	Target  CredentialPasswordSnapshot
	NewHash string
}

// ApplyCredentialPassword 以同一连接的短事务复核和写入；仅 Commit 报错才从独立连接回读。
func (r *UserRepo) ApplyCredentialPassword(ctx context.Context, change CredentialPasswordChange) (bool, error) {
	if change.Event.EventType != SecurityEventPasswordChanged || change.Target.ID <= 0 || change.Event.EventID == "" ||
		change.Event.OperationID == "" || (change.Event.ActorKind == "user") != (change.Event.ActorUserID != nil) ||
		(change.Event.ActorKind != "user" && change.Event.ActorKind != "local_operator") {
		return false, ErrInvalidSecurityEvent
	}
	changed, err := r.writeCredentialPassword(ctx, change)
	if !errors.Is(err, ErrPasswordCommitUnknown) {
		return changed, err
	}
	return r.confirmCredentialPassword(ctx, change)
}

func (r *UserRepo) confirmCredentialPassword(ctx context.Context, change CredentialPasswordChange) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	confirmed, err := readCredentialPasswordReceipt(ctx, r.db, change)
	if err == nil && confirmed {
		return true, nil
	}
	return false, ErrPasswordCommitUnknown
}

func (r *UserRepo) writeCredentialPassword(ctx context.Context, change CredentialPasswordChange) (bool, error) {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkCredentialPasswordActor(ctx, tx, change); err != nil {
		return false, err
	}
	if replay, err := readCredentialPasswordReceipt(ctx, tx, change); err != nil || replay {
		return false, err
	}
	var current CredentialPasswordSnapshot
	if err := tx.GetContext(ctx, &current, passwordSnapshotSQL, change.Target.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrPasswordVersionConflict
		}
		return false, err
	}
	if current.Epoch != change.Target.Epoch || current.PasswordHash != change.Target.PasswordHash || current.Username != change.Target.Username {
		return false, ErrPasswordVersionConflict
	}
	if change.NewHash == "" {
		// 不写入；读取已在写事务持锁期间序列化，无需提交一个空事务。
		return false, nil
	}
	if current.Epoch == math.MaxInt64 {
		return false, ErrPasswordVersionConflict
	}
	if err := r.persistCredentialPassword(ctx, tx, change); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		// 不重试提交。丢弃连接后才确认，避免读到仍挂在同一连接上的未提交状态。
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return false, ErrPasswordCommitUnknown
	}
	return true, nil
}

func checkCredentialPasswordActor(ctx context.Context, tx *sqlx.Tx, change CredentialPasswordChange) error {
	if change.Event.ActorUserID == nil {
		return nil
	}
	var allowed int
	id := *change.Event.ActorUserID
	err := tx.GetContext(ctx, &allowed, `SELECT 1 FROM user WHERE id=? AND status='active' AND web_login_disabled=0 AND (id=? OR role='admin')`, id, change.Target.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPasswordVersionConflict
	}
	return err
}

func (r *UserRepo) persistCredentialPassword(ctx context.Context, tx *sqlx.Tx, change CredentialPasswordChange) error {
	target := change.Target
	res, err := tx.ExecContext(ctx, `UPDATE user SET password_hash=?, account_epoch=account_epoch+1, updated_at=datetime('now')
		WHERE id=? AND account_epoch=? AND password_hash=?`, change.NewHash, target.ID, target.Epoch, target.PasswordHash)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrPasswordVersionConflict
	}
	object := SecurityEventObject{Ordinal: 1, ObjectKind: "user", ObjectID: strconv.FormatInt(target.ID, 10)}
	e := change.Event
	if err := NewSecurityEventRepo(r.db).InsertTx(ctx, tx, e, []SecurityEventObject{object}); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credential_password_receipt (operation_id,event_id,actor_kind,actor_user_id,target_user_id) VALUES (?,?,?,?,?)`,
		e.OperationID, e.EventID, e.ActorKind, e.ActorUserID, target.ID)
	return err
}

func readCredentialPasswordReceipt(ctx context.Context, q sqlx.QueryerContext, change CredentialPasswordChange) (bool, error) {
	e := change.Event
	var matches int
	err := sqlx.GetContext(ctx, q, &matches, `SELECT coalesce(r.event_id=? AND r.actor_kind=? AND r.actor_user_id IS ? AND r.target_user_id=?
		AND e.operation_id=r.operation_id AND e.event_type=? AND e.actor_kind=r.actor_kind AND e.actor_user_id IS r.actor_user_id
		AND e.committed='committed' AND e.outcome='succeeded' AND o.object_kind='user' AND o.object_id=CAST(r.target_user_id AS TEXT), 0)
		FROM credential_password_receipt r JOIN security_event e ON e.event_id=r.event_id
		LEFT JOIN security_event_object o ON o.event_sequence=e.sequence AND o.ordinal=1 WHERE r.operation_id=?`,
		e.EventID, e.ActorKind, e.ActorUserID, change.Target.ID, SecurityEventPasswordChanged, e.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if matches != 1 {
		return false, ErrPasswordVersionConflict
	}
	return true, nil
}
