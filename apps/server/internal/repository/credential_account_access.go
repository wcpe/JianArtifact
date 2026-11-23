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
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// AccountAccessChange 已由入口绑定到真实操作者，空字段表示保持原值。
type AccountAccessChange struct {
	ID               int64
	Role, Status     string
	WebLoginDisabled *bool
	Proof            *auth.CredentialProof
	Event            SecurityEvent
	ReservedUsername string
}

var ErrAccountAccessUnconfirmed = errors.New("账号资格提交结果未确认")
var ErrAccountAccessInvalid = errors.New("账号资格参数无效")

func (r *UserRepo) ApplyAccountAccess(ctx context.Context, c AccountAccessChange) (*User, bool, error) {
	if (c.Role != "" && c.Role != "admin" && c.Role != "user") || (c.Status != "" && c.Status != "active" && c.Status != "disabled") {
		return nil, false, ErrAccountAccessInvalid
	}
	u, changed, err := r.writeAccountAccess(ctx, c)
	if !errors.Is(err, ErrAccountAccessUnconfirmed) {
		return u, changed, err
	}
	lookup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	ok, err := readAccountAccessReceipt(lookup, r.db, c)
	if !ok || err != nil {
		return nil, false, ErrAccountAccessUnconfirmed
	}
	return u, changed, nil
}

func (r *UserRepo) writeAccountAccess(ctx context.Context, c AccountAccessChange) (*User, bool, error) {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if c.Proof != nil {
		if c.Proof.Protocol || c.Event.ActorKind != "user" || c.Event.ActorUserID == nil || *c.Event.ActorUserID != c.Proof.UserID {
			return nil, false, ErrCredentialRejected
		}
		actor, err := r.RevalidateCredentialTx(ctx, tx, *c.Proof)
		if err != nil {
			return nil, false, err
		}
		if actor.Role != "admin" {
			return nil, false, ErrCredentialRejected
		}
	} else if c.Event.ActorKind != "local_operator" || c.Event.ActorUserID != nil {
		return nil, false, ErrCredentialRejected
	}
	u, err := readCredentialAccount(ctx, tx, "u.id", strconv.FormatInt(c.ID, 10))
	if err != nil {
		return nil, false, err
	}
	if u.Username == c.ReservedUsername {
		return nil, false, ErrAccountAccessInvalid
	}
	if replay, err := readAccountAccessReceipt(ctx, tx, c); err != nil || replay {
		return &u.User, false, err
	}
	role, status, disabled := u.Role, u.Status, u.WebLoginDisabled
	if c.Role != "" {
		role = c.Role
	}
	if c.Status != "" {
		status = c.Status
	}
	if c.WebLoginDisabled != nil {
		disabled = *c.WebLoginDisabled
	}
	if role == u.Role && status == u.Status && disabled == u.WebLoginDisabled {
		return &u.User, false, nil
	}
	epoch := u.Epoch
	narrow := (u.Status == "active" && status == "disabled") || (u.Role == "admin" && role == "user") || (!u.WebLoginDisabled && disabled)
	restoreLegacy := epoch == 1 && ((u.Status == "disabled" && status == "active") || (u.WebLoginDisabled && !disabled))
	if narrow || restoreLegacy {
		if epoch == math.MaxInt64 {
			return nil, false, ErrPasswordVersionConflict
		}
		epoch++
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user SET role=?,status=?,web_login_disabled=?,account_epoch=?,updated_at=datetime('now') WHERE id=?`, role, status, disabled, epoch, u.ID); err != nil {
		return nil, false, err
	}
	e := c.Event
	object := SecurityEventObject{Ordinal: 1, ObjectKind: "user", ObjectID: strconv.FormatInt(u.ID, 10)}
	if err = NewSecurityEventRepo(r.db).InsertTx(ctx, tx, e, []SecurityEventObject{object}); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO credential_account_access_receipt(operation_id,event_id,actor_kind,actor_user_id,target_user_id) VALUES(?,?,?,?,?)`, e.OperationID, e.EventID, e.ActorKind, e.ActorUserID, u.ID); err != nil {
		return nil, false, err
	}
	u.Role, u.Status, u.WebLoginDisabled = role, status, disabled
	if err = tx.Commit(); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return &u.User, true, ErrAccountAccessUnconfirmed
	}
	return &u.User, true, nil
}

func readAccountAccessReceipt(ctx context.Context, q sqlx.QueryerContext, c AccountAccessChange) (bool, error) {
	e := c.Event
	var same int
	err := sqlx.GetContext(ctx, q, &same, `SELECT coalesce(r.event_id=? AND r.actor_kind=? AND r.actor_user_id IS ? AND r.target_user_id=?
  AND e.operation_id=r.operation_id AND e.event_type='auth.account_access_changed' AND e.actor_kind=r.actor_kind AND e.actor_user_id IS r.actor_user_id
  AND e.committed='committed' AND e.outcome='succeeded' AND o.object_kind='user' AND o.object_id=CAST(r.target_user_id AS TEXT),0)
  FROM credential_account_access_receipt r JOIN security_event e ON e.event_id=r.event_id
  LEFT JOIN security_event_object o ON o.event_sequence=e.sequence AND o.ordinal=1 WHERE r.operation_id=?`, e.EventID, e.ActorKind, e.ActorUserID, c.ID, e.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if same != 1 {
		return false, ErrPasswordVersionConflict
	}
	return true, nil
}
