package repository

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
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
//
// 名字既可以是仓库主名，也可以是别名（repository_alias）：先按主名查，未命中再经
// 别名解析到仓库。别名只是访问入口——返回的始终是该仓库本身（其 Name 为主名），
// 使协议路由与管理端 API 全域都能用别名等价访问主名仓库。
func (r *RepoRepo) GetByName(name string) (*Repository, error) {
	var repo Repository
	err := r.db.Get(&repo, `SELECT id, name, format, type, visibility, description, config, online, created_at FROM repository WHERE name = ?`, name)
	if err == nil {
		return &repo, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// 主名未命中：经别名解析到所属仓库（别名唯一，至多一行）。
	err = r.db.Get(&repo, `SELECT r.id, r.name, r.format, r.type, r.visibility, r.description, r.config, r.online, r.created_at
		FROM repository r JOIN repository_alias a ON a.repository_id = r.id WHERE a.alias = ?`, name)
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
	err := r.db.Get(&repo, `SELECT id, name, format, type, visibility, description, config, online, created_at FROM repository WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &repo, nil
}

// GetByIDTx 在调用方事务中按 ID 取仓库，供提交点判定（FR-41 存储配额）读取
// 与本次写入同一事务视图下的仓库配置。不存在返回 ErrNotFound。
func (r *RepoRepo) GetByIDTx(tx *sqlx.Tx, id int64) (*Repository, error) {
	var repo Repository
	err := tx.Get(&repo, `SELECT id, name, format, type, visibility, description, config, online, created_at FROM repository WHERE id = ?`, id)
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
	err := r.db.Select(&repos, `SELECT id, name, format, type, visibility, description, config, online, created_at FROM repository ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
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
	q := fmt.Sprintf(`SELECT id, name, format, type, visibility, description, config, online, created_at FROM repository ORDER BY %s %s, id %s LIMIT ? OFFSET ?`, col, dir, dir)
	err := r.db.Select(&repos, q, limit, offset)
	return repos, err
}

// ListPublic 返回所有 visibility=public 的仓库。
func (r *RepoRepo) ListPublic() ([]Repository, error) {
	var repos []Repository
	err := r.db.Select(&repos, `SELECT id, name, format, type, visibility, description, config, online, created_at FROM repository WHERE visibility = 'public' ORDER BY name`)
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

// SetOnline 设置仓库 online/offline 状态并直接落库（FR-113）。
// 仅更新 online 列，不动 config / description 等复制字段。
func (r *RepoRepo) SetOnline(name string, online bool) error {
	res, err := r.db.Exec(
		`UPDATE repository SET online = ?, updated_at = datetime('now') WHERE name = ?`,
		online, name,
	)
	return affected(res, err)
}

// Delete 删除仓库（级联删除其 ACL）。
func (r *RepoRepo) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM repository WHERE name = ?`, name)
	return affected(res, err)
}

// DeleteTx 在调用方事务中删除仓库，供资产生命周期协调器统一提交。
func (r *RepoRepo) DeleteTx(tx *sqlx.Tx, name string) error {
	res, err := tx.Exec(`DELETE FROM repository WHERE name = ?`, name)
	return affected(res, err)
}

// ListAliases 返回仓库的别名列表（按字典序）；无别名返回空切片。
func (r *RepoRepo) ListAliases(repoID int64) ([]string, error) {
	var aliases []string
	err := r.db.Select(&aliases, `SELECT alias FROM repository_alias WHERE repository_id = ? ORDER BY alias`, repoID)
	if aliases == nil {
		aliases = []string{}
	}
	return aliases, err
}

// ListAliasesByRepos 批量返回多个仓库的别名（repository_id -> 别名列表，按字典序），
// 供列表页一次 IN 查询填充，避免逐仓 N+1。空 id 集合返回空 map，不发起查询。
func (r *RepoRepo) ListAliasesByRepos(repoIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(repoIDs))
	if len(repoIDs) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(
		`SELECT repository_id, alias FROM repository_alias WHERE repository_id IN (?) ORDER BY repository_id, alias`,
		repoIDs,
	)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(r.db.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var alias string
		if err := rows.Scan(&id, &alias); err != nil {
			return nil, err
		}
		out[id] = append(out[id], alias)
	}
	return out, rows.Err()
}

// SetAliases 覆盖式写入仓库别名：单个事务内先删该仓库的全部别名再插入新集合，
// 保证别名集合整体替换的原子性；传入空集合即清空别名。
// 别名与任何主名/别名冲突时由 alias 主键拒绝（返回 UNIQUE 约束错误，调用方映射为冲突语义）。
func (r *RepoRepo) SetAliases(repoID int64, aliases []string) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM repository_alias WHERE repository_id = ?`, repoID); err != nil {
		return err
	}
	for _, alias := range aliases {
		if _, err := tx.Exec(`INSERT INTO repository_alias (alias, repository_id) VALUES (?, ?)`, alias, repoID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NameTaken 判定名字是否已被占用：命中任何仓库主名或任何别名都算占用。
// 别名与主名共享同一命名空间，故重命名与别名校验共用此判定。
func (r *RepoRepo) NameTaken(name string) (bool, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM (
		SELECT 1 FROM repository WHERE name = ?
		UNION ALL
		SELECT 1 FROM repository_alias WHERE alias = ?
	)`, name, name)
	return n > 0, err
}

// Rename 重命名仓库（同一事务）：更新主名并把旧名登记为别名，使旧名仍可解析到该仓库。
// 调用方需先校验 newName 未被占用；旧名称为当前主名，故必不与既有别名冲突。
// affected 语义与既有方法一致：无受影响行视为 ErrNotFound。
func (r *RepoRepo) Rename(oldName, newName string) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE repository SET name = ?, updated_at = datetime('now') WHERE name = ?`, newName, oldName)
	if err := affected(res, err); err != nil {
		return err
	}
	// 旧名转别名：按新名定位仓库 id，避免额外往返查询。
	res, err = tx.Exec(`INSERT INTO repository_alias (alias, repository_id) SELECT ?, id FROM repository WHERE name = ?`, oldName, newName)
	if err := affected(res, err); err != nil {
		return err
	}
	return tx.Commit()
}
