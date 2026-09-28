package repository

import (
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// PinnedRepoRepo 读写 pinned_repository 表：置顶仓库的服务端持久化（用户级 + 全局兜底）。
//
// 语义（见迁移 0043 的头注释）：
//   - userID == nil 表示**全局置顶**（管理员维护，作为匿名访问与无个人置顶用户的兜底）；
//   - 非 nil 表示某个登录用户的私有置顶，与全局置顶互不覆盖；
//   - 置顶按 repository_id 关联，仓库重命名不影响置顶；名字在读取时实时解析。
type PinnedRepoRepo struct{ db *persistence.DB }

// NewPinnedRepoRepo 构造 PinnedRepoRepo。
func NewPinnedRepoRepo(db *persistence.DB) *PinnedRepoRepo { return &PinnedRepoRepo{db: db} }

// List 返回某作用域的置顶仓库 ID，按**置顶顺序**（插入顺序，经 rowid 稳定）返回。
// userID == nil 查全局置顶，否则查该用户的私有置顶。无置顶时返回空切片。
func (r *PinnedRepoRepo) List(userID *int64) ([]int64, error) {
	var ids []int64
	var err error
	if userID == nil {
		err = r.db.Select(&ids, `SELECT repository_id FROM pinned_repository WHERE user_id IS NULL ORDER BY rowid`)
	} else {
		err = r.db.Select(&ids, `SELECT repository_id FROM pinned_repository WHERE user_id = ? ORDER BY rowid`, *userID)
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, err
}

// ListGlobal 返回全局置顶仓库 ID；等价于 List(nil)，供公开列表等语义明确处调用。
func (r *PinnedRepoRepo) ListGlobal() ([]int64, error) { return r.List(nil) }

// Set **覆盖式**写入某作用域的置顶集合：单个事务内先删该作用域的全部置顶、再按给定顺序插入，
// 保证整体替换的原子性；传入空集合即清空。userID == nil 写全局置顶。
//
// 传入的仓库 ID 会先去重（保持首次出现顺序），任一 ID 不存在时返回 ErrNotFound 并整体回滚——
// 先校验存在性再删除，避免「请求里有非法 ID」时把用户既有的置顶清空。
// 用户/仓库被删除时的清理由外键 ON DELETE CASCADE 负责。
func (r *PinnedRepoRepo) Set(userID *int64, repoIDs []int64) error {
	ordered := dedupeIDs(repoIDs)
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, id := range ordered {
		var one int
		if err := tx.Get(&one, `SELECT 1 FROM repository WHERE id = ?`, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
	}

	if userID == nil {
		if _, err := tx.Exec(`DELETE FROM pinned_repository WHERE user_id IS NULL`); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`DELETE FROM pinned_repository WHERE user_id = ?`, *userID); err != nil {
		return err
	}

	for _, id := range ordered {
		var err error
		if userID == nil {
			_, err = tx.Exec(`INSERT INTO pinned_repository (user_id, repository_id) VALUES (NULL, ?)`, id)
		} else {
			_, err = tx.Exec(`INSERT INTO pinned_repository (user_id, repository_id) VALUES (?, ?)`, *userID, id)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NamesByIDs 返回「仓库 ID → 当前仓库名」的映射（仅包含仍然存在的仓库），
// 供置顶响应把 ID 解析为主名（重命名后仍取到最新名字）。空 id 集合返回空 map，不发起查询。
func (r *PinnedRepoRepo) NamesByIDs(ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(`SELECT id, name FROM repository WHERE id IN (?)`, ids)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	if err := r.db.Select(&rows, r.db.Rebind(query), args...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row.Name
	}
	return out, nil
}

// dedupeIDs 去重并保持首次出现顺序，避免一次请求里的重复 ID 产生重复行。
func dedupeIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
