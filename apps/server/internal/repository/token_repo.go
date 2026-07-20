package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// TokenRepo 读写 api_token 表。仅存摘要，明文不落库。
type TokenRepo struct{ db *persistence.DB }

// NewTokenRepo 构造 TokenRepo。
func NewTokenRepo(db *persistence.DB) *TokenRepo { return &TokenRepo{db: db} }

// Create 记录一枚 Token 的摘要，返回新 ID。
func (r *TokenRepo) Create(userID int64, name, digest string) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO api_token (user_id, name, token_digest) VALUES (?, ?, ?)`,
		userID, name, digest,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListByUser 返回某用户未吊销的 Token（不含摘要）。
func (r *TokenRepo) ListByUser(userID int64) ([]Token, error) {
	var ts []Token
	err := r.db.Select(&ts,
		`SELECT id, user_id, name, created_at FROM api_token
		 WHERE user_id = ? AND revoked_at IS NULL ORDER BY id`, userID)
	return ts, err
}

// Delete 吊销某用户名下的 Token（置 revoked_at）。
func (r *TokenRepo) Delete(id, userID int64) error {
	res, err := r.db.Exec(
		`UPDATE api_token SET revoked_at = datetime('now')
		 WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		id, userID,
	)
	return affected(res, err)
}

// UserIDByDigest 按摘要查未吊销 Token 所属用户 ID；无匹配返回 ErrNotFound。
func (r *TokenRepo) UserIDByDigest(digest string) (int64, error) {
	var uid int64
	err := r.db.Get(&uid,
		`SELECT user_id FROM api_token WHERE token_digest = ? AND revoked_at IS NULL`, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return uid, err
}
