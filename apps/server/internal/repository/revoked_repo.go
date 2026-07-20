package repository

import "github.com/wcpe/jianartifact/apps/server/internal/persistence"

// RevokedRepo 读写 revoked_token 表（登出会话黑名单）。
type RevokedRepo struct{ db *persistence.DB }

// NewRevokedRepo 构造 RevokedRepo。
func NewRevokedRepo(db *persistence.DB) *RevokedRepo { return &RevokedRepo{db: db} }

// Revoke 记录一枚会话 jti 直至其过期时间（Unix 秒）。重复登出幂等。
func (r *RevokedRepo) Revoke(jti string, expiresAt int64) error {
	_, err := r.db.Exec(
		`INSERT OR IGNORE INTO revoked_token (jti, expires_at) VALUES (?, ?)`,
		jti, expiresAt,
	)
	return err
}

// IsRevoked 判断会话 jti 是否在黑名单中。
func (r *RevokedRepo) IsRevoked(jti string) (bool, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM revoked_token WHERE jti = ?`, jti)
	return n > 0, err
}
