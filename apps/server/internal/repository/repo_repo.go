package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// RepoRepo 读写 repository 表。
type RepoRepo struct{ db *persistence.DB }

// NewRepoRepo 构造 RepoRepo。
func NewRepoRepo(db *persistence.DB) *RepoRepo { return &RepoRepo{db: db} }

// Create 插入仓库，返回新 ID。config 为结构化配置 JSON（空则传 "{}"）。
func (r *RepoRepo) Create(name, format, typ, visibility, config string) (int64, error) {
	if config == "" {
		config = "{}"
	}
	res, err := r.db.Exec(
		`INSERT INTO repository (name, format, type, visibility, config) VALUES (?, ?, ?, ?, ?)`,
		name, format, typ, visibility, config,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetByName 按名取仓库；不存在返回 ErrNotFound。
func (r *RepoRepo) GetByName(name string) (*Repository, error) {
	var repo Repository
	err := r.db.Get(&repo, `SELECT id, name, format, type, visibility, description, config, created_at FROM repository WHERE name = ?`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &repo, nil
}

// GetByID 按 ID 取仓库；不存在返回 ErrNotFound。
func (r *RepoRepo) GetByID(id int64) (*Repository, error) {
	var repo Repository
	err := r.db.Get(&repo, `SELECT id, name, format, type, visibility, description, config, created_at FROM repository WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &repo, nil
}

// List 返回分页仓库（按 id 升序）。
func (r *RepoRepo) List(limit, offset int) ([]Repository, error) {
	var repos []Repository
	err := r.db.Select(&repos, `SELECT id, name, format, type, visibility, description, config, created_at FROM repository ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
	return repos, err
}

// ListSorted 返回分页仓库，支持排序字段与方向。
// sortBy 可选: name, created_at；order 可选: asc, desc。
func (r *RepoRepo) ListSorted(limit, offset int, sortBy, order string) ([]Repository, error) {
	col := "id"
	switch sortBy {
	case "name":
		col = "name"
	case "created_at":
		col = "created_at"
	}
	dir := "ASC"
	if order == "desc" {
		dir = "DESC"
	}
	var repos []Repository
	q := fmt.Sprintf(`SELECT id, name, format, type, visibility, description, config, created_at FROM repository ORDER BY %s %s LIMIT ? OFFSET ?`, col, dir)
	err := r.db.Select(&repos, q, limit, offset)
	return repos, err
}

// ListPublic 返回所有 visibility=public 的仓库。
func (r *RepoRepo) ListPublic() ([]Repository, error) {
	var repos []Repository
	err := r.db.Select(&repos, `SELECT id, name, format, type, visibility, description, config, created_at FROM repository WHERE visibility = 'public' ORDER BY name`)
	return repos, err
}

// Count 返回仓库总数。
func (r *RepoRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM repository`)
	return n, err
}

// UpdateVisibility 更新可见性（空串表示不改）。
func (r *RepoRepo) UpdateVisibility(name, visibility string) error {
	res, err := r.db.Exec(
		`UPDATE repository SET
			visibility = COALESCE(NULLIF(?, ''), visibility),
			updated_at = datetime('now')
		WHERE name = ?`,
		visibility, name,
	)
	return affected(res, err)
}

// UpdateDescription 覆盖写入仓库描述（允许清空为空串）。
func (r *RepoRepo) UpdateDescription(name, description string) error {
	res, err := r.db.Exec(
		`UPDATE repository SET description = ?, updated_at = datetime('now') WHERE name = ?`,
		description, name,
	)
	return affected(res, err)
}

// UpdateConfig 覆盖写入仓库的结构化配置 JSON（config 列）。
func (r *RepoRepo) UpdateConfig(name, config string) error {
	if config == "" {
		config = "{}"
	}
	res, err := r.db.Exec(
		`UPDATE repository SET config = ?, updated_at = datetime('now') WHERE name = ?`,
		config, name,
	)
	return affected(res, err)
}

// Delete 删除仓库（级联删除其 ACL）。
func (r *RepoRepo) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM repository WHERE name = ?`, name)
	return affected(res, err)
}
