package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// ErrCredentialRejected 表示已确认账号或凭据不再具备资格。
var ErrCredentialRejected = errors.New("凭据已失效")
var ErrExternalCredentialBinding = errors.New("外部绑定不匹配")

const credentialUserColumns = `u.id,u.username,u.password_hash,u.role,u.status,u.web_login_disabled,u.created_at,u.email,u.auth_source,u.external_subject,u.account_epoch,u.credential_revision`

// CredentialAccount 是库内认证时使用的账号快照，不对外返回。
type CredentialAccount struct {
	User
	Epoch    int64 `db:"account_epoch"`
	Revision int64 `db:"credential_revision"`
}

func readCredentialAccount(ctx context.Context, q sqlx.QueryerContext, column, value string) (CredentialAccount, error) {
	var u CredentialAccount
	err := sqlx.GetContext(ctx, q, &u, `SELECT `+credentialUserColumns+` FROM user u WHERE `+column+`=?`, value)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

func (r *UserRepo) CredentialAccountByUsername(ctx context.Context, username string) (CredentialAccount, error) {
	return readCredentialAccount(ctx, r.db, "u.username", username)
}

// CredentialRevision 返回与账号安全变更同序的库内签发水位。
func (r *UserRepo) CredentialRevision(ctx context.Context) (int64, error) {
	var rev int64
	err := r.db.GetContext(ctx, &rev, `SELECT revision FROM credential_revision_clock WHERE singleton=1`)
	return rev, err
}

// CredentialWrite 供短签发事务复用。业务计算和外部身份验证在锁外完成。
func (r *UserRepo) CredentialWrite(ctx context.Context, apply func(*sqlx.Tx) error) error {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := apply(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	return nil
}

// CredentialSessionAccount 校验已验签载体的持久绑定；无行的旧载体仅允许初始代次。
func (r *UserRepo) CredentialSessionAccount(ctx context.Context, p auth.CredentialProof) (CredentialAccount, error) {
	if p.SessionID == "" && p.Epoch == 0 {
		p.Epoch = 1
	}
	return credentialAccountForProof(ctx, r.db, p, false)
}

// RevalidateCredentialTx 在调用方同一事务内重验身份，不从请求数据重建证明。
func (r *UserRepo) RevalidateCredentialTx(ctx context.Context, tx *sqlx.Tx, p auth.CredentialProof) (*User, error) {
	if tx == nil {
		return nil, ErrCredentialRejected
	}
	u, err := credentialAccountForProof(ctx, tx, p, !p.Protocol)
	if err != nil {
		return nil, err
	}
	return &u.User, nil
}

func credentialAccountForProof(ctx context.Context, q sqlx.QueryerContext, p auth.CredentialProof, web bool) (CredentialAccount, error) {
	var u CredentialAccount
	if p.UserID <= 0 || p.Epoch <= 0 {
		return u, ErrCredentialRejected
	}
	query := `SELECT ` + credentialUserColumns + ` FROM user u WHERE u.id=? AND u.account_epoch=? AND u.status='active'`
	args := []any{p.UserID, p.Epoch}
	if web {
		query += ` AND u.web_login_disabled=0`
	}
	now := time.Now().Unix()
	switch p.Kind {
	case auth.KindSession:
		if p.JTI == "" || p.ExpiresAt.Unix() <= now {
			return u, ErrCredentialRejected
		}
		query += ` AND NOT EXISTS (SELECT 1 FROM revoked_token WHERE jti=?)`
		args = append(args, p.JTI)
		if p.SessionID == "" {
			query += ` AND u.account_epoch=1`
		} else {
			query += ` AND EXISTS (SELECT 1 FROM credential_session s WHERE s.session_id=? AND s.jti=? AND s.user_id=u.id
    AND s.account_epoch=u.account_epoch AND s.expires_at=? AND s.expires_at>? AND s.revoked_at IS NULL)`
			args = append(args, p.SessionID, p.JTI, p.ExpiresAt.Unix(), now)
		}
	case auth.KindToken:
		if p.TokenID <= 0 {
			return u, ErrCredentialRejected
		}
		query += ` AND EXISTS (SELECT 1 FROM api_token t WHERE t.id=? AND t.user_id=u.id AND ` + tokenCurrentSQL + `)`
		args = append(args, p.TokenID, now)
	case "":
		// Basic 口令在锁外核验，以观察到的账号代次和当前通道资格重验。
	default:
		return u, ErrCredentialRejected
	}
	err := sqlx.GetContext(ctx, q, &u, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrCredentialRejected
	}
	return u, err
}

const tokenCurrentSQL = `t.revoked_at IS NULL AND ((t.account_epoch IS NULL AND t.expires_at IS NULL AND u.account_epoch=1)
 OR (t.account_epoch=u.account_epoch AND t.expires_at>?))`

// CredentialTokenAccount 以一次索引查询裁决秘密摘要、单项吊销、期限与账号代次。
func (r *UserRepo) CredentialTokenAccount(ctx context.Context, digest string) (CredentialAccount, int64, string, error) {
	var row struct {
		CredentialAccount
		TokenID   int64  `db:"credential_token_id"`
		TokenName string `db:"credential_token_name"`
	}
	err := r.db.GetContext(ctx, &row, `SELECT `+credentialUserColumns+`,t.id AS credential_token_id,t.name AS credential_token_name
  FROM api_token t JOIN user u ON u.id=t.user_id WHERE t.token_digest=? AND u.status='active' AND `+tokenCurrentSQL, digest, time.Now().Unix())
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrCredentialRejected
	}
	return row.CredentialAccount, row.TokenID, row.TokenName, err
}

func InsertCredentialSessionTx(ctx context.Context, tx *sqlx.Tx, u CredentialAccount, claims *auth.Claims) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO credential_session(session_id,jti,user_id,account_epoch,expires_at) VALUES(?,?,?,?,?)`,
		claims.SessionID, claims.ID, u.ID, u.Epoch, claims.ExpiresAt.Unix())
	return err
}

// LoginCredentialAccountTx 只接受由已观察账号快照授权的本地会话签发。
func (r *UserRepo) LoginCredentialAccountTx(ctx context.Context, tx *sqlx.Tx, u CredentialAccount) (CredentialAccount, error) {
	current, err := credentialAccountForProof(ctx, tx, auth.CredentialProof{UserID: u.ID, Epoch: u.Epoch}, true)
	if err == nil && (current.PasswordHash != u.PasswordHash || current.Revision != u.Revision) {
		err = ErrCredentialRejected
	}
	return current, err
}

func BootstrapCredentialAccountTx(ctx context.Context, tx *sqlx.Tx, username, hash, reserved string) (CredentialAccount, error) {
	var n int
	if err := tx.GetContext(ctx, &n, `SELECT count(*) FROM user WHERE username<>?`, reserved); err != nil {
		return CredentialAccount{}, err
	}
	if n != 0 {
		return CredentialAccount{}, ErrCredentialRejected
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user(username,password_hash,role) VALUES(?,?,'admin')`, username, hash); err != nil {
		return CredentialAccount{}, err
	}
	return readCredentialAccount(ctx, tx, "u.username", username)
}

// ExternalCredentialAccountTx 在同一写锁内完成身份绑定或新建及签发资格裁决。
func ExternalCredentialAccountTx(ctx context.Context, tx *sqlx.Tx, source, subject, username, email string, started int64) (CredentialAccount, error) {
	u, err := readCredentialAccount(ctx, tx, "u.external_subject", subject)
	if err == nil {
		if u.AuthSource != source {
			return u, ErrExternalCredentialBinding
		}
		if u.Revision > started || u.Status != "active" || u.WebLoginDisabled {
			return u, ErrCredentialRejected
		}
		return u, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return u, err
	}
	u, err = readCredentialAccount(ctx, tx, "u.username", username)
	if err == nil {
		if u.ExternalSubject != "" || u.Role != "user" {
			return u, ErrExternalCredentialBinding
		}
		if u.Revision > started || u.Status != "active" || u.WebLoginDisabled {
			return u, ErrCredentialRejected
		}
		if _, err = tx.ExecContext(ctx, `UPDATE user SET auth_source=?,external_subject=? WHERE id=?`, source, subject, u.ID); err != nil {
			return u, err
		}
	} else if errors.Is(err, ErrNotFound) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user(username,password_hash,role,email,auth_source,external_subject) VALUES(?,'','user',?,?,?)`, username, email, source, subject); err != nil {
			return u, err
		}
	} else {
		return u, err
	}
	return readCredentialAccount(ctx, tx, "u.external_subject", subject)
}
