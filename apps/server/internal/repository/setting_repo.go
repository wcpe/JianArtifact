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

// SettingValue 表示一次设置键值更新。
type SettingValue struct {
	Key   string
	Value string
}

// Set 写入或覆盖键值。
func (r *SettingRepo) Set(key, value string) error {
	_, err := r.db.Exec(settingUpsertSQL, key, value)
	return err
}

// SetMany 在同一事务内写入全部键值；任一失败时整体回滚。
func (r *SettingRepo) SetMany(values []SettingValue) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, value := range values {
		if _, err := tx.Exec(settingUpsertSQL, value.Key, value.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const settingUpsertSQL = `INSERT INTO setting (key, value, updated_at) VALUES (?, ?, datetime('now'))
 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
