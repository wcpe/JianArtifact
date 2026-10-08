package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// UserGroupRepo 读写 user_group 与 user_group_member 两张表（FR-36，迁移 0046）。
//
// 组是授权主体之一：组本身只承载「名字 + 描述」，授权关系由 acl 表指向组 ID，
// 成员关系由 user_group_member 表承载。因此删组需要连带清理 acl 条目（由
// AclRepo.DeleteByGroup 承担，本仓储不感知 acl），成员与组名唯一性由表约束兜底。
type UserGroupRepo struct{ db *persistence.DB }

// NewUserGroupRepo 构造 UserGroupRepo。
func NewUserGroupRepo(db *persistence.DB) *UserGroupRepo { return &UserGroupRepo{db: db} }

// Create 新建用户组，返回新 ID。组名重复由 UNIQUE 约束拒绝（调用方负责转成友好错误）。
func (r *UserGroupRepo) Create(name, description string) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO user_group (name, description) VALUES (?, ?)`,
		name, description,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// List 返回全部分组（按 name 升序，便于管理面稳定展示）。
func (r *UserGroupRepo) List() ([]UserGroup, error) {
	var groups []UserGroup
	err := r.db.Select(&groups,
		`SELECT id, name, description, created_at FROM user_group ORDER BY name`)
	return groups, err
}

// GetByID 按 ID 取组；不存在返回 ErrNotFound。
func (r *UserGroupRepo) GetByID(id int64) (*UserGroup, error) {
	var g UserGroup
	err := r.db.Get(&g, `SELECT id, name, description, created_at FROM user_group WHERE id = ?`, id)
	return &g, mapNoRows(err)
}

// GetByName 按组名取组；不存在返回 ErrNotFound。
func (r *UserGroupRepo) GetByName(name string) (*UserGroup, error) {
	var g UserGroup
	err := r.db.Get(&g, `SELECT id, name, description, created_at FROM user_group WHERE name = ?`, name)
	return &g, mapNoRows(err)
}

// Update 更新组名与描述（空串表示不改）。不存在返回 ErrNotFound。
func (r *UserGroupRepo) Update(id int64, name, description string) error {
	res, err := r.db.Exec(
		`UPDATE user_group SET
			name = COALESCE(NULLIF(?, ''), name),
			description = COALESCE(NULLIF(?, ''), description)
		WHERE id = ?`,
		name, description, id,
	)
	return affected(res, err)
}

// Delete 删除用户组。成员关系由外键 CASCADE 清理；ACL 条目由调用方显式清理。
func (r *UserGroupRepo) Delete(id int64) error {
	res, err := r.db.Exec(`DELETE FROM user_group WHERE id = ?`, id)
	return affected(res, err)
}

// AddMember 把用户加入组；已存在或组/用户不存在时返回错误（由约束或外键拒绝）。
func (r *UserGroupRepo) AddMember(groupID, userID int64) error {
	_, err := r.db.Exec(
		`INSERT OR IGNORE INTO user_group_member (group_id, user_id) VALUES (?, ?)`,
		groupID, userID,
	)
	return err
}

// RemoveMember 把用户移出组；关系不存在返回 ErrNotFound。
func (r *UserGroupRepo) RemoveMember(groupID, userID int64) error {
	res, err := r.db.Exec(
		`DELETE FROM user_group_member WHERE group_id = ? AND user_id = ?`,
		groupID, userID,
	)
	return affected(res, err)
}

// ListMembers 列出组的全部成员用户（按用户名升序）。
func (r *UserGroupRepo) ListMembers(groupID int64) ([]User, error) {
	var us []User
	err := r.db.Select(&us,
		`SELECT u.id, u.username, u.password_hash, u.role, u.status, u.web_login_disabled, u.created_at,
			u.email, u.auth_source, u.external_subject
		FROM user_group_member m JOIN user u ON u.id = m.user_id
		WHERE m.group_id = ? ORDER BY u.username`, groupID)
	return us, err
}

// ListGroupsOfUser 列出某用户所属的全部组 ID（升序）。
//
// 这是鉴权热路径：一次授权判定要先展开该用户的全部组，故只取 ID（不 JOIN，
// 不多取列），交给 Subject.GroupIDs 下推到 SQL 的 IN 子句。
func (r *UserGroupRepo) ListGroupsOfUser(userID int64) ([]int64, error) {
	var ids []int64
	err := r.db.Select(&ids,
		`SELECT group_id FROM user_group_member WHERE user_id = ? ORDER BY group_id`, userID)
	return ids, err
}

// mapNoRows 把 sql.ErrNoRows 归一为本包约定的 ErrNotFound。
func mapNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
