package repository

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AssetRepo 读写 asset 表。
type AssetRepo struct{ db *persistence.DB }

// NewAssetRepo 构造 AssetRepo。
func NewAssetRepo(db *persistence.DB) *AssetRepo { return &AssetRepo{db: db} }

// Upsert 覆盖写入仓库内某路径的资产：路径已存在则更新 blob/大小/类型/校验和与 updated_at，
// 否则插入新行。以 (repository_id, path) 唯一约束实现 last-writer-wins。
func (r *AssetRepo) Upsert(repoID int64, path, blobHash string, size int64, contentType, sha1, md5 string) error {
	_, err := r.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, path) DO UPDATE SET
			blob_hash    = excluded.blob_hash,
			size         = excluded.size,
			content_type = excluded.content_type,
			sha1         = excluded.sha1,
			md5          = excluded.md5,
			updated_at   = datetime('now')`,
		repoID, path, blobHash, size, contentType, sha1, md5,
	)
	return err
}

// GetByPath 按仓库与路径取资产；不存在返回 ErrNotFound。
func (r *AssetRepo) GetByPath(repoID int64, path string) (*Asset, error) {
	var a Asset
	err := r.db.Get(&a,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE repository_id = ? AND path = ?`,
		repoID, path,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteByPath 删除仓库内某路径的资产（blob 内容不即时清理）；不存在返回 ErrNotFound。
func (r *AssetRepo) DeleteByPath(repoID int64, path string) error {
	res, err := r.db.Exec(`DELETE FROM asset WHERE repository_id = ? AND path = ?`, repoID, path)
	return affected(res, err)
}

// ListByRepo 返回仓库内资产（按 path 升序分页）；prefix 非空时按路径前缀过滤。
func (r *AssetRepo) ListByRepo(repoID int64, prefix string, limit, offset int) ([]Asset, error) {
	var assets []Asset
	err := r.db.Select(&assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE repository_id = ? AND path LIKE ? ESCAPE '\' ORDER BY path LIMIT ? OFFSET ?`,
		repoID, likePrefix(prefix), limit, offset,
	)
	return assets, err
}

// CountByRepo 返回仓库内资产总数；prefix 非空时按路径前缀过滤。
func (r *AssetRepo) CountByRepo(repoID int64, prefix string) (int, error) {
	var n int
	err := r.db.Get(&n,
		`SELECT COUNT(*) FROM asset WHERE repository_id = ? AND path LIKE ? ESCAPE '\'`,
		repoID, likePrefix(prefix),
	)
	return n, err
}

// ListAllPaths 返回仓库内全部资产路径（迁移 skip 预加载用，避免逐条 Exists）。
func (r *AssetRepo) ListAllPaths(repoID int64) ([]string, error) {
	var paths []string
	err := r.db.Select(&paths, `SELECT path FROM asset WHERE repository_id = ?`, repoID)
	return paths, err
}

// RepoStats 是单仓库的制品统计（数量与总大小）。
type RepoStats struct {
	RepositoryID int64 `db:"repository_id"`
	Count        int64 `db:"count"`
	TotalSize    int64 `db:"total_size"`
}

// ListMissingChecksums 返回 sha1 或 md5 为空的资产（历史数据回填用）。
func (r *AssetRepo) ListMissingChecksums(limit int) ([]Asset, error) {
	if limit <= 0 {
		limit = 500
	}
	var assets []Asset
	err := r.db.Select(&assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE sha1 = '' OR md5 = '' ORDER BY id LIMIT ?`,
		limit,
	)
	return assets, err
}

// UpdateChecksums 按资产 id 更新已登记的 sha1/md5（仅回填路径使用）。
func (r *AssetRepo) UpdateChecksums(id int64, sha1, md5 string) error {
	_, err := r.db.Exec(
		`UPDATE asset SET sha1 = ?, md5 = ?, updated_at = datetime('now') WHERE id = ?`,
		sha1, md5, id,
	)
	return err
}

// CountMissingChecksums 返回仍缺 sha1/md5 的资产数量。
func (r *AssetRepo) CountMissingChecksums() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM asset WHERE sha1 = '' OR md5 = ''`)
	return n, err
}

// ListByRepos 返回多个仓库内资产（按 path 升序分页）；prefix 非空时按路径前缀过滤。
func (r *AssetRepo) ListByRepos(repoIDs []int64, prefix string, limit, offset int) ([]Asset, error) {
	if len(repoIDs) == 0 {
		return nil, nil
	}
	query, args, err := sqlx.In(
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\' ORDER BY path LIMIT ? OFFSET ?`,
		repoIDs, likePrefix(prefix), limit, offset,
	)
	if err != nil {
		return nil, err
	}
	var assets []Asset
	err = r.db.Select(&assets, r.db.Rebind(query), args...)
	return assets, err
}

// CountByRepos 返回多个仓库内资产总数；prefix 非空时按路径前缀过滤。
func (r *AssetRepo) CountByRepos(repoIDs []int64, prefix string) (int, error) {
	if len(repoIDs) == 0 {
		return 0, nil
	}
	query, args, err := sqlx.In(
		`SELECT COUNT(*) FROM asset WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\'`,
		repoIDs, likePrefix(prefix),
	)
	if err != nil {
		return 0, err
	}
	var n int
	err = r.db.Get(&n, r.db.Rebind(query), args...)
	return n, err
}

// CountAndSizeByRepo 返回单个仓库的制品数量与总字节数。
func (r *AssetRepo) CountAndSizeByRepo(repoID int64) (count int64, totalSize int64, err error) {
	var s RepoStats
	err = r.db.Get(&s,
		`SELECT ? AS repository_id, COUNT(*) AS count, COALESCE(SUM(size), 0) AS total_size FROM asset WHERE repository_id = ?`,
		repoID, repoID,
	)
	return s.Count, s.TotalSize, err
}

// CountAndSizeByRepos 一次性返回多个仓库的制品统计（GROUP BY 避免 N+1）。
// 返回以 repository_id 为键的映射；未出现在结果中的仓库表示无制品（count=0, totalSize=0）。
func (r *AssetRepo) CountAndSizeByRepos(repoIDs []int64) (map[int64]RepoStats, error) {
	result := make(map[int64]RepoStats, len(repoIDs))
	if len(repoIDs) == 0 {
		return result, nil
	}
	query, args, err := sqlx.In(
		`SELECT repository_id, COUNT(*) AS count, COALESCE(SUM(size), 0) AS total_size
			FROM asset WHERE repository_id IN (?) GROUP BY repository_id`,
		repoIDs,
	)
	if err != nil {
		return nil, err
	}
	var rows []RepoStats
	if err := r.db.Select(&rows, r.db.Rebind(query), args...); err != nil {
		return nil, err
	}
	// 预置零值，确保无制品的仓库也有条目。
	for _, id := range repoIDs {
		result[id] = RepoStats{RepositoryID: id}
	}
	for i := range rows {
		result[rows[i].RepositoryID] = rows[i]
	}
	return result, nil
}

// SearchByPath 跨仓库搜索制品路径（全局 LIKE）。repoIDs 为空搜全部，keyword 必填。
func (r *AssetRepo) SearchByPath(keyword string, repoIDs []int64, limit, offset int) ([]Asset, int, error) {
	pattern := "%" + escapeLike(keyword) + "%"
	var total int
	var assets []Asset
	if len(repoIDs) == 0 {
		err := r.db.Get(&total, `SELECT COUNT(*) FROM asset WHERE path LIKE ? ESCAPE '\'`, pattern)
		if err != nil {
			return nil, 0, err
		}
		err = r.db.Select(&assets,
			`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
				FROM asset WHERE path LIKE ? ESCAPE '\' ORDER BY path LIMIT ? OFFSET ?`,
			pattern, limit, offset,
		)
		return assets, total, err
	}
	query, args, err := sqlx.In(
		`SELECT COUNT(*) FROM asset WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\'`,
		repoIDs, pattern,
	)
	if err != nil {
		return nil, 0, err
	}
	if err := r.db.Get(&total, r.db.Rebind(query), args...); err != nil {
		return nil, 0, err
	}
	query2, args2, err := sqlx.In(
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\' ORDER BY path LIMIT ? OFFSET ?`,
		repoIDs, pattern, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	err = r.db.Select(&assets, r.db.Rebind(query2), args2...)
	return assets, total, err
}

// ListDirectoryEntries 列出指定前缀下的「当前层级」目录与文件（用于 tree endpoint 按目录懒加载）。
// 返回去重后的目录名列表与文件 Asset 列表（不递归子目录）。
func (r *AssetRepo) ListDirectoryEntries(repoIDs []int64, prefix string) (dirs []string, files []Asset, err error) {
	var assets []Asset
	if len(repoIDs) == 0 {
		return nil, nil, nil
	}
	query, args, qErr := sqlx.In(
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\' ORDER BY path`,
		repoIDs, likePrefix(prefix),
	)
	if qErr != nil {
		return nil, nil, qErr
	}
	if err = r.db.Select(&assets, r.db.Rebind(query), args...); err != nil {
		return nil, nil, err
	}
	dirSet := make(map[string]struct{})
	prefixLen := len(prefix)
	for i := range assets {
		rel := assets[i].Path[prefixLen:] // path after prefix
		slashIdx := strings.Index(rel, "/")
		if slashIdx == -1 {
			// 直接子文件
			files = append(files, assets[i])
		} else {
			// 子目录：返回自仓库根起算的完整路径（与文件 path 一致），
			// 避免前端将相对目录名误当完整路径，导致嵌套目录展开时前缀错误。
			dirName := prefix + rel[:slashIdx]
			dirSet[dirName] = struct{}{}
		}
	}
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	return dirs, files, nil
}

// escapeLike 转义 LIKE 特殊字符。
func escapeLike(s string) string {
	var b strings.Builder
	for _, ch := range s {
		switch ch {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// likePrefix 把路径前缀转为 LIKE 模式：转义 %/_/\ 后追加通配 %（配合 ESCAPE '\'），
// 空前缀匹配全部。
func likePrefix(prefix string) string {
	if prefix == "" {
		return "%"
	}
	var b strings.Builder
	for _, ch := range prefix {
		switch ch {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(ch)
	}
	b.WriteByte('%')
	return b.String()
}
