package repository

import (
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AssetRepo 读写 asset 表。
type AssetRepo struct{ db *persistence.DB }

// NewAssetRepo 构造 AssetRepo。
func NewAssetRepo(db *persistence.DB) *AssetRepo { return &AssetRepo{db: db} }

// DB 返回底层数据库连接，供同一领域操作协调器复用事务边界。
func (r *AssetRepo) DB() *persistence.DB { return r.db }

// Upsert 覆盖写入仓库内某路径的资产：路径已存在则更新 blob/大小/类型/校验和与 updated_at，
// 否则插入新行。以 (repository_id, path) 唯一约束实现 last-writer-wins。时间取数据库当前值。
func (r *AssetRepo) Upsert(repoID int64, path, blobHash string, size int64, contentType, sha1, md5 string) error {
	return r.UpsertWithTime(repoID, path, blobHash, size, contentType, sha1, md5, "", "")
}

// UpsertWithTime 覆盖写入资产，并按源端时间回填 created_at/updated_at：
// createdAt/updatedAt 非空（UTC "YYYY-MM-DD HH:MM:SS"）时使用该值，否则回退 datetime('now')。
// 已存在路径的 created_at 仅在提供新值（非空）时更新，用于复制时间同步 / 回填场景。
func (r *AssetRepo) UpsertWithTime(repoID int64, path, blobHash string, size int64, contentType, sha1, md5, createdAt, updatedAt string) error {
	_, err := r.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?,''), datetime('now')), COALESCE(NULLIF(?,''), datetime('now')))
		ON CONFLICT (repository_id, path) DO UPDATE SET
			blob_hash    = excluded.blob_hash,
			size         = excluded.size,
			content_type = excluded.content_type,
			sha1         = excluded.sha1,
			md5          = excluded.md5,
			created_at   = COALESCE(NULLIF(excluded.created_at,''), asset.created_at),
			updated_at   = COALESCE(NULLIF(excluded.updated_at,''), datetime('now'))`,
		repoID, path, blobHash, size, contentType, sha1, md5, createdAt, updatedAt,
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

// UpdateTimes 回填资产时间（created_at/updated_at，均为 UTC "YYYY-MM-DD HH:MM:SS"）；
// 仓库内路径不存在则影响 0 行。返回受影响行数。
func (r *AssetRepo) UpdateTimes(repoID int64, path, createdAt, updatedAt string) (int64, error) {
	res, err := r.db.Exec(
		`UPDATE asset SET created_at = ?, updated_at = ? WHERE repository_id = ? AND path = ?`,
		createdAt, updatedAt, repoID, path,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// SearchFilter 路径搜索的结构化条件（由 domain 解析高级表达式得到）。
// 各字段间为 AND 语义；IncludeExts 内部为 OR（任一扩展名命中即可）。
type SearchFilter struct {
	IncludeTerms []string // path 必须包含的子串
	ExcludeTerms []string // path 不得包含的子串（负筛选）
	IncludeExts  []string // 扩展名白名单（不含点，小写）
	ExcludeExts  []string // 扩展名黑名单（不含点，小写）
}

// buildSearchWhere 把结构化搜索条件拼成 WHERE 子句与参数（SearchByFilter / SearchFacets 共用）。
func buildSearchWhere(f SearchFilter, repoIDs []int64) (string, []any) {
	conds := make([]string, 0, 8)
	args := make([]any, 0, 8)
	for _, t := range f.IncludeTerms {
		conds = append(conds, `path LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(t)+"%")
	}
	for _, t := range f.ExcludeTerms {
		conds = append(conds, `path NOT LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(t)+"%")
	}
	if len(f.IncludeExts) > 0 {
		ors := make([]string, 0, len(f.IncludeExts))
		for _, e := range f.IncludeExts {
			ors = append(ors, `path LIKE ? ESCAPE '\'`)
			args = append(args, "%."+escapeLike(e))
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	for _, e := range f.ExcludeExts {
		conds = append(conds, `path NOT LIKE ? ESCAPE '\'`)
		args = append(args, "%."+escapeLike(e))
	}
	if len(repoIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(repoIDs)), ",")
		conds = append(conds, "repository_id IN ("+ph+")")
		for _, id := range repoIDs {
			args = append(args, id)
		}
	}
	if len(conds) == 0 {
		return "1=1", args
	}
	return strings.Join(conds, " AND "), args
}

// searchOrderBy 把排序键映射为 ORDER BY 表达式（白名单，防注入）。
// name 用 SQLite 技巧提取路径 basename；repo 用子查询取仓库名。
func searchOrderBy(sort, order string) string {
	col := "path"
	switch sort {
	case "name":
		col = `replace(path, rtrim(path, replace(path, '/', '')), '')`
	case "repo":
		col = `(SELECT name FROM repository WHERE id = asset.repository_id)`
	case "size":
		col = "size"
	case "updated":
		col = "updated_at"
	}
	dir := "ASC"
	if order == "desc" {
		dir = "DESC"
	}
	// path 作次级排序，保证分页稳定
	if col == "path" {
		return "path " + dir
	}
	return col + " " + dir + ", path ASC"
}

// SearchByFilter 跨仓库搜索制品路径，全部条件下推 SQL。repoIDs 为空搜全部。
// sort 取值 name/repo/path/size/updated（其余按 path），order 取值 asc/desc。
func (r *AssetRepo) SearchByFilter(f SearchFilter, repoIDs []int64, sort, order string, limit, offset int) ([]Asset, int, error) {
	where, args := buildSearchWhere(f, repoIDs)
	var total int
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM asset WHERE `+where, args...); err != nil {
		return nil, 0, err
	}
	var assets []Asset
	err := r.db.Select(&assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE `+where+` ORDER BY `+searchOrderBy(sort, order)+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...,
	)
	return assets, total, err
}

// SearchFacet 是搜索结果按仓库聚合的单条计数。
type SearchFacet struct {
	RepositoryID int64 `db:"repository_id"`
	Count        int   `db:"count"`
}

// SearchFacetsByFilter 返回同一搜索条件下各仓库的命中数（按命中数降序），
// 供前端做「按仓库聚合」的钻取导航。repoIDs 为空统计全部仓库。
func (r *AssetRepo) SearchFacetsByFilter(f SearchFilter, repoIDs []int64) ([]SearchFacet, error) {
	where, args := buildSearchWhere(f, repoIDs)
	var facets []SearchFacet
	err := r.db.Select(&facets,
		`SELECT repository_id, COUNT(*) AS count FROM asset WHERE `+where+
			` GROUP BY repository_id ORDER BY count DESC`,
		args...,
	)
	return facets, err
}

// DirStat 是某前缀下单个直接子目录的聚合信息：其子树内的制品总数与最近一次更新时间
// （UTC "YYYY-MM-DD HH:MM:SS"）。目录页据此在目录行上直接给出「多少项 / 最近何时改动」，
// 无需为每个目录再发一次查询。
type DirStat struct {
	Path   string `db:"path"`   // 自仓库根起算的完整目录路径（与文件 path 同口径）
	Count  int    `db:"count"`  // 子树内制品总数（含更深层级）
	Latest string `db:"latest"` // 子树内最大的 updated_at；无数据时为空串
}

// DirectChildren 是某前缀下的「当前层级」子项：直接子目录与直接文件。
// Dirs / DirStats 为自仓库根起算的完整目录路径，始终全量且同序；FileTotal / FileBytes 是
// 直接文件的总数与体积合计，不受 Files 分页影响，供调用方给出精确计数。
type DirectChildren struct {
	Dirs      []string
	DirStats  []DirStat
	Files     []Asset
	FileTotal int
	FileBytes int64
}

// ListDirectChildren 返回前缀下的直接子目录与直接文件（不递归）。
//
// 子目录由 SQL 侧聚合出层级段名——只回传去重后的段名与其子树计数/最近更新时间，不把子树里的
// 制品行读进内存——并始终全量返回；直接文件单独计数并按 path 升序分页，fileLimit <= 0 表示取全部。
// 旧实现先把前缀下的**所有**制品行取出、再在内存里去重目录：十万级目录会把整棵子树读进
// 内存，而且一旦对取数做条数截断，同层的其它子目录就会整片消失（列表既不完整也不正确）。
// 本方法从数据层消除该根因，无论目录多大，返回规模都只与「同层项数」相关。
func (r *AssetRepo) ListDirectChildren(repoIDs []int64, prefix string, fileLimit, fileOffset int) (*DirectChildren, error) {
	out := &DirectChildren{Dirs: []string{}, Files: []Asset{}}
	if len(repoIDs) == 0 {
		return out, nil
	}
	// SQLite 的 substr/instr 对 TEXT 按**字符**计数，故层级起点取前缀的字符数 + 1（不是字节数）。
	start := utf8.RuneCountInString(prefix) + 1
	prefixLike := likePrefix(prefix)

	// 一次聚合拿到直接子目录的段名、子树制品数与最近更新时间：段名来自 DISTINCT 的
	// 等价写法，GROUP BY 不改变分组键集合，故省掉一次单独的 DISTINCT 查询。
	statQuery, statArgs, err := sqlx.In(
		`SELECT seg AS path, COUNT(*) AS count, COALESCE(MAX(updated_at), '') AS latest
			FROM (
				SELECT substr(path, ?, instr(substr(path, ?), '/') - 1) AS seg, updated_at
				FROM asset
				WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\'
					AND instr(substr(path, ?), '/') > 0
			)
			GROUP BY seg
			ORDER BY seg`,
		start, start, repoIDs, prefixLike, start,
	)
	if err != nil {
		return nil, err
	}
	var segStats []DirStat
	if err := r.db.Select(&segStats, r.db.Rebind(statQuery), statArgs...); err != nil {
		return nil, err
	}
	if len(segStats) > 0 {
		// 子目录返回自仓库根起算的完整路径（与文件 path 一致），避免前端把相对目录名
		// 误当完整路径、导致嵌套目录展开时前缀错误。
		dirs := make([]string, 0, len(segStats))
		stats := make([]DirStat, 0, len(segStats))
		for _, s := range segStats {
			s.Path = prefix + s.Path
			dirs = append(dirs, s.Path)
			stats = append(stats, s)
		}
		out.Dirs, out.DirStats = dirs, stats
	}

	var stat struct {
		Count int   `db:"count"`
		Bytes int64 `db:"bytes"`
	}
	countQuery, countArgs, err := sqlx.In(
		`SELECT COUNT(*) AS count, COALESCE(SUM(size), 0) AS bytes FROM asset
			WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\'
				AND instr(substr(path, ?), '/') = 0 AND path <> ?`,
		repoIDs, prefixLike, start, prefix,
	)
	if err != nil {
		return nil, err
	}
	if err := r.db.Get(&stat, r.db.Rebind(countQuery), countArgs...); err != nil {
		return nil, err
	}
	out.FileTotal, out.FileBytes = stat.Count, stat.Bytes

	fileQuery := `SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
		FROM asset
		WHERE repository_id IN (?) AND path LIKE ? ESCAPE '\'
			AND instr(substr(path, ?), '/') = 0 AND path <> ?
		ORDER BY path`
	fileArgs := []any{repoIDs, prefixLike, start, prefix}
	if fileLimit > 0 {
		fileQuery += ` LIMIT ? OFFSET ?`
		fileArgs = append(fileArgs, fileLimit, fileOffset)
	}
	query, args, err := sqlx.In(fileQuery, fileArgs...)
	if err != nil {
		return nil, err
	}
	if err := r.db.Select(&out.Files, r.db.Rebind(query), args...); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDirectoryEntries 列出指定前缀下的「当前层级」目录与文件（用于 tree endpoint 按目录懒加载）。
// 返回自仓库根起算的完整目录路径列表与直接文件 Asset 列表（不递归子目录）。
func (r *AssetRepo) ListDirectoryEntries(repoIDs []int64, prefix string) (dirs []string, files []Asset, err error) {
	if len(repoIDs) == 0 {
		return nil, nil, nil
	}
	children, err := r.ListDirectChildren(repoIDs, prefix, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	return children.Dirs, children.Files, nil
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
