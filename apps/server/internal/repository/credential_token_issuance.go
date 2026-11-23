package repository

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

const credentialTokenTTL = 30 * 24 * time.Hour

// MachineTokenLimit 是每账号同时有效的机器凭据上限。
const MachineTokenLimit = 32

// CredentialTokenIssue 是一次机器凭据签发输入；用途/作用域/期限在提交前已定形。
type CredentialTokenIssue struct {
	Name       string
	Digest     string
	Purpose    string
	ScopesJSON string
	ExpiresAt  int64
}

// IssueCredentialToken 提交签发时绑定当前账号代次；来自请求的签发必须提供原认证证明。
// 同一事务内核对签发主体资格与有效凭据数量上限。
func (r *UserRepo) IssueCredentialToken(ctx context.Context, userID int64, in CredentialTokenIssue, proof *auth.CredentialProof) (int64, error) {
	var id int64
	expires := in.ExpiresAt
	if expires <= 0 {
		expires = time.Now().Add(credentialTokenTTL).Unix()
	}
	err := r.CredentialWrite(ctx, func(tx *sqlx.Tx) error {
		if proof != nil {
			if proof.UserID != userID {
				return ErrCredentialRejected
			}
			if _, err := r.RevalidateCredentialTx(ctx, tx, *proof); err != nil {
				return err
			}
		}
		var active int
		if err := tx.GetContext(ctx, &active, `SELECT count(*) FROM api_token WHERE user_id=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)`, userID, time.Now().Unix()); err != nil {
			return err
		}
		if active >= MachineTokenLimit {
			return ErrCredentialRejected
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO api_token(user_id,name,token_digest,account_epoch,expires_at,purpose,scopes_json)
  SELECT id,?,?,account_epoch,?,?,? FROM user WHERE id=? AND status='active'`, in.Name, in.Digest, expires, in.Purpose, in.ScopesJSON, userID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCredentialRejected
		}
		id, err = result.LastInsertId()
		return err
	})
	return id, err
}
