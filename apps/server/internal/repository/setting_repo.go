package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// SettingRepo 读写 setting 表（实例级键值设置）。
type SettingRepo struct{ db *persistence.DB }

// NewSettingRepo 构造 SettingRepo。
func NewSettingRepo(db *persistence.DB) *SettingRepo { return &SettingRepo{db: db} }

// Get 按键取值；不存在返回 ErrNotFound。
func (r *SettingRepo) Get(key string) (string, error) {
	var v string
	err := r.db.Get(&v, `SELECT value FROM setting WHERE key = ?`, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// Set 写入或覆盖键值。
func (r *SettingRepo) Set(key, value string) error {
	_, err := r.db.Exec(
		`INSERT INTO setting (key, value, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value,
	)
	return err
}
