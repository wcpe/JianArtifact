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

// ListStoredByUser 返回某用户未吊销的令牌（含摘要，供复制历史回填使用）。
func (r *TokenRepo) ListStoredByUser(userID int64) ([]StoredToken, error) {
	var ts []StoredToken
	err := r.db.Select(&ts,
		`SELECT id, user_id, name, token_digest, revoked_at FROM api_token
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

// StoredToken 是 api_token 表含摘要的完整行模型（仅内部/复制应用用，不对外暴露摘要）。
type StoredToken struct {
	ID        int64   `db:"id"`
	UserID    int64   `db:"user_id"`
	Name      string  `db:"name"`
	Digest    string  `db:"token_digest"`
	RevokedAt *string `db:"revoked_at"`
}

// GetByDigest 按摘要查 Token（含已吊销），供复制应用（FR-83）定位本地记录；无匹配返回 ErrNotFound。
func (r *TokenRepo) GetByDigest(digest string) (*StoredToken, error) {
	var t StoredToken
	err := r.db.Get(&t,
		`SELECT id, user_id, name, token_digest, revoked_at FROM api_token WHERE token_digest = ?`, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetByIDAndUser 按 ID + 所属用户查 Token（含摘要与吊销状态），供吊销时记录变更日志；无匹配返回 ErrNotFound。
func (r *TokenRepo) GetByIDAndUser(id, userID int64) (*StoredToken, error) {
	var t StoredToken
	err := r.db.Get(&t,
		`SELECT id, user_id, name, token_digest, revoked_at FROM api_token WHERE id = ? AND user_id = ?`, id, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
