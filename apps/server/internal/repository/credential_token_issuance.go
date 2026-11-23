package repository

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

const credentialTokenTTL = 30 * 24 * time.Hour

// IssueCredentialToken 提交签发时绑定当前账号代次；来自请求的签发必须提供原认证证明。
func (r *UserRepo) IssueCredentialToken(ctx context.Context, userID int64, name, digest string, proof *auth.CredentialProof) (int64, error) {
	var id int64
	err := r.CredentialWrite(ctx, func(tx *sqlx.Tx) error {
		if proof != nil {
			if proof.UserID != userID {
				return ErrCredentialRejected
			}
			if _, err := r.RevalidateCredentialTx(ctx, tx, *proof); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO api_token(user_id,name,token_digest,account_epoch,expires_at)
  SELECT id,?,?,account_epoch,? FROM user WHERE id=? AND status='active'`, name, digest, time.Now().Add(credentialTokenTTL).Unix(), userID)
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
