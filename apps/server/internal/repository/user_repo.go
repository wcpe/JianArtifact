package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ErrNotFound 表示按条件未查到记录。
var ErrNotFound = errors.New("记录不存在")

// UserRepo 读写 user 表。
type UserRepo struct{ db *persistence.DB }

// NewUserRepo 构造 UserRepo。
func NewUserRepo(db *persistence.DB) *UserRepo { return &UserRepo{db: db} }

// Count 返回用户总数（用于自举判断与状态统计）。
func (r *UserRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM user`)
	return n, err
}

// Create 插入用户，返回新 ID。
func (r *UserRepo) Create(username, passwordHash, role string) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO user (username, password_hash, role) VALUES (?, ?, ?)`,
		username, passwordHash, role,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetByID 按 ID 取用户；不存在返回 ErrNotFound。
func (r *UserRepo) GetByID(id int64) (*User, error) {
	var u User
	err := r.db.Get(&u, `SELECT id, username, password_hash, role, status, created_at FROM user WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByUsername 按用户名取用户；不存在返回 ErrNotFound。
func (r *UserRepo) GetByUsername(username string) (*User, error) {
	var u User
	err := r.db.Get(&u, `SELECT id, username, password_hash, role, status, created_at FROM user WHERE username = ?`, username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// List 返回分页用户（按 id 升序）。
func (r *UserRepo) List(limit, offset int) ([]User, error) {
	var us []User
	err := r.db.Select(&us, `SELECT id, username, password_hash, role, status, created_at FROM user ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
	return us, err
}

// Update 更新角色与状态（空串表示不改）。
func (r *UserRepo) Update(id int64, role, status string) error {
	res, err := r.db.Exec(
		`UPDATE user SET
			role = COALESCE(NULLIF(?, ''), role),
			status = COALESCE(NULLIF(?, ''), status),
			updated_at = datetime('now')
		WHERE id = ?`,
		role, status, id,
	)
	return affected(res, err)
}

// UpdatePassword 重置口令哈希。
func (r *UserRepo) UpdatePassword(id int64, passwordHash string) error {
	res, err := r.db.Exec(
		`UPDATE user SET password_hash = ?, updated_at = datetime('now') WHERE id = ?`,
		passwordHash, id,
	)
	return affected(res, err)
}

// Delete 删除用户。
func (r *UserRepo) Delete(id int64) error {
	res, err := r.db.Exec(`DELETE FROM user WHERE id = ?`, id)
	return affected(res, err)
}

// affected 统一处理 Exec 结果：无错但影响 0 行视为 ErrNotFound。
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
