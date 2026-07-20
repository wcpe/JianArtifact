package repository

import (
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AclRepo 读写 acl 表。
type AclRepo struct{ db *persistence.DB }

// NewAclRepo 构造 AclRepo。
func NewAclRepo(db *persistence.DB) *AclRepo { return &AclRepo{db: db} }

// ListByRepo 返回某仓库的全部 ACL 条目。
func (r *AclRepo) ListByRepo(repoID int64) ([]Acl, error) {
	var acls []Acl
	err := r.db.Select(&acls,
		`SELECT subject_id, action FROM acl WHERE repository_id = ? ORDER BY subject_id, action`, repoID)
	return acls, err
}

// Replace 以事务方式覆盖某仓库的全部 ACL。
func (r *AclRepo) Replace(repoID int64, entries []Acl) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM acl WHERE repository_id = ?`, repoID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, e := range entries {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO acl (repository_id, subject_id, action) VALUES (?, ?, ?)`,
			repoID, e.SubjectID, e.Action,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// HasPermission 判断主体对仓库是否拥有指定动作的授权。
// admin 授权蕴含 read/write；write 蕴含 read。
func (r *AclRepo) HasPermission(repoID, subjectID int64, action string) (bool, error) {
	var implied []string
	switch action {
	case "read":
		implied = []string{"read", "write", "admin"}
	case "write":
		implied = []string{"write", "admin"}
	default:
		implied = []string{"admin"}
	}
	query, args, err := sqlx.In(
		`SELECT COUNT(*) FROM acl WHERE repository_id = ? AND subject_id = ? AND action IN (?)`,
		repoID, subjectID, implied,
	)
	if err != nil {
		return false, err
	}
	var n int
	if err := r.db.Get(&n, r.db.Rebind(query), args...); err != nil {
		return false, err
	}
	return n > 0, nil
}
