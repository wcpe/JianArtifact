package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
//
// 索引同步（FR-149）：asset_path_fts 维护与本次写入放在**同一短事务**内，避免「元数据已提交、
// 索引未写」的窗口（那种漏索引无法靠一次性的历史回填补救）。冲突键就是 path，因此覆盖写时
// path 必然未变、索引内容无需变更——索引镜像的就是 path 本身，只在真正新增行时补索引。
// 事务内只做两次主键/唯一索引级别的点查与一次 FTS 写，不含任何耗时操作。
func (r *AssetRepo) UpsertWithTime(repoID int64, path, blobHash string, size int64, contentType, sha1, md5, createdAt, updatedAt string) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 先判存在性：已存在的行 path 未变，索引无需变更；新行才需要建索引。
	_, existed, err := assetRowID(tx, repoID, path)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
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
	); err != nil {
		return err
	}
	if !existed {
		id, ok, err := assetRowID(tx, repoID, path)
		if err != nil {
			return err
		}
		if !ok {
			// 刚写入的行在本事务内查不到属于异常：本事务以 BEGIN IMMEDIATE 开启
			// （db.go 的 _txlock），提交前一直持有写锁，不存在并发删除的窗口。
			// 报错而不是静默跳过索引，避免留下一个「元数据在、索引缺」的漏索引行。
			return fmt.Errorf("资产写入后查不到：%d/%s", repoID, path)
		}
		if err := searchIndexInsert(tx, id, path); err != nil {
			return err
		}
	}
	return tx.Commit()
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
//
// 索引同步（FR-149）：与删除同事务维护 asset_path_fts。顺序是**先删索引行、再删 asset 行**——
// external content 表要靠内容行才知道该 rowid 有哪些词要移除，内容行先消失就清不掉词
// （见 search_index_repo.go 文件头第 3 条）。
func (r *AssetRepo) DeleteByPath(repoID int64, path string) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	id, existed, err := assetRowID(tx, repoID, path)
	if err != nil {
		return err
	}
	if !existed {
		return ErrNotFound
	}
	if err := searchIndexDelete(tx, id, path); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM asset WHERE repository_id = ? AND path = ?`, repoID, path)
	if err := affected(res, err); err != nil {
		return err
	}
	return tx.Commit()
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

// repoStatsQuery 是单仓库占用的唯一聚合 SQL（制品数 COUNT(*) 与逻辑字节 SUM(size)）：
// 仓库存储配额（FR-41）的预检与提交点复检共用它，避免两处口径漂移。
const repoStatsQuery = `SELECT ? AS repository_id, COUNT(*) AS count, COALESCE(SUM(size), 0) AS total_size FROM asset WHERE repository_id = ?`

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
	err = r.db.Get(&s, repoStatsQuery, repoID, repoID)
	return s.Count, s.TotalSize, err
}

// CountAndSizeByRepoTx 在调用方事务中返回单个仓库的制品数量与总字节数。
// 供仓库存储配额的提交点权威复检（FR-41）读取与本次写入同一事务视图下的占用，
// 复用与 CountAndSizeByRepo 完全相同的聚合 SQL（不另写等价查询）。
func (r *AssetRepo) CountAndSizeByRepoTx(tx *sqlx.Tx, repoID int64) (count int64, totalSize int64, err error) {
	var s RepoStats
	err = tx.Get(&s, repoStatsQuery, repoID, repoID)
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

	// IncludeVersions / ExcludeVersions 是 `version:` / `-version:` 的版本段严格条件（FR-150）。
	// IncludeVersions 内部为 OR（命中任一版本即可），ExcludeVersions 内部为 AND（都不相等）。
	// 它们**不是**子串条件：`1.17.12` 不得命中 `1.17.120/` 或 `1.17.12-rc1/`（见 buildSearchWhere）。
	IncludeVersions []string
	ExcludeVersions []string

	// UseFullTextIndex 允许本次查询把可下推的包含词换成 asset_path_fts 候选集条件。
	//
	// 由 domain 在**每次搜索入口**按 search_index_state.completed_at 判定后置位，且**不做
	// 进程内缓存**：全量重建会把 completed_at 打回 NULL（索引此时已被清空），缓存住 true
	// 会让重建期间的搜索继续走 FTS 并静默漏结果——这是本批最危险的错误模式。
	// 为 false 时行为与引入索引之前逐字一致（全部条件仍走 path LIKE）。
	UseFullTextIndex bool
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
	if len(f.IncludeVersions) > 0 {
		ors := make([]string, 0, len(f.IncludeVersions))
		for _, v := range f.IncludeVersions {
			ors = append(ors, versionSegmentPattern)
			args = append(args, versionSegmentArgs(v)...)
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	for _, v := range f.ExcludeVersions {
		conds = append(conds, `(path NOT LIKE ? ESCAPE '\' AND path NOT LIKE ? ESCAPE '\')`)
		args = append(args, versionSegmentArgs(v)...)
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

// versionSegmentPattern 是 `version:` 条件的**段边界**形态（FR-150 §3.6）：
//
//	( path LIKE '%/'||v||'/%'  OR  path LIKE '%-'||v||'.tgz' )
//
// 第一支覆盖 Maven 与 raw 的版本目录形态（`.../1.17.12/file`，整段相等）；
// 第二支覆盖 npm tarball 形态（`pkg/-/pkg-1.17.12.tgz`，版本不在独立路径段里）。
// 两支都是严格相等：`1.17.120/` 与 `1.17.12-rc1/` 都不命中。
//
// 严格性落在 SQL 里而不是 Go 侧后置过滤：后置过滤会让 total 变成过滤后的计数、分页也
// 得搬到 Go 侧，`total` 与 `page` 的既有语义会一起失真（规格 §3.6 末尾）。
const versionSegmentPattern = `(path LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\')`

// versionSegmentArgs 返回 versionSegmentPattern 两个占位符对应的 LIKE 模式。
// 版本值必须转义（`_` / `%` / `\` 在版本串里虽罕见，但拼错会静默放宽成通配）。
func versionSegmentArgs(version string) []any {
	v := escapeLike(version)
	return []any{"%/" + v + "/%", "%-" + v + ".tgz"}
}

// searchIndexMinTermRunes 是 trigram 分词器可匹配的最短子串长度。
//
// trigram 以 3 个字符为一个词元，短于 3 个字符的词在索引里不存在任何词元，强行走 MATCH 会
// **静默漏结果**，因此这类词一律留在原 LIKE 路径上。判定按 rune 数而不是字节数：中文 2 字词
// （如「标签」）是 6 字节但只有 2 个字符，按字节判定会误以为可索引，结果一个都匹配不到。
const searchIndexMinTermRunes = 3

// indexableTerms 挑出可下推到全文索引的包含词（非空且 rune 数 >= 3）。
// 返回空切片表示本次查询没有可用的正向候选驱动条件，调用方应整体退回 LIKE 路径。
func indexableTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if utf8.RuneCountInString(t) < searchIndexMinTermRunes {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ftsPhraseLiteral 把用户输入的一个词包成 FTS5 **字符串字面量**。
//
// 用户输入绝不能直接拼进 MATCH：FTS5 的匹配串有独立语法（AND / OR / NOT / NEAR / * / ^ / :
// / 括号），把 `a OR b` 直接交给 MATCH 会变成布尔查询而不是「包含子串 a OR b」——既是语义
// 回退，也是注入面。包成字面量后内部只剩字面量文本（内部的 `"` 按 FTS5 规则双写转义），
// trigram 分词器再把它切成三字词元，等价于原来的 `path LIKE '%词%'`。
func ftsPhraseLiteral(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

// ftsMatchExpression 把多个可下推词组合成一个 FTS5 匹配串：每词一个字面量短语、词间 AND。
// 与原来的「多条 path LIKE '%词%' 用 AND 连接」逐字等价（包含词之间本来就是 AND 语义）。
func ftsMatchExpression(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		parts = append(parts, ftsPhraseLiteral(t))
	}
	return strings.Join(parts, " AND ")
}

// buildSearchWhereIndexed 是 SearchByFilter / SearchFacetsByFilter / SearchPerRepoByFilter
// 实际使用的 WHERE 构造点，也是**唯一**的候选集下推点（三条语句共用同一段 WHERE，避免各拼
// 一遍而漂移）。
//
// 与 buildSearchWhere 的唯一差别：UseFullTextIndex 为真且存在可下推的包含词时，把这些词合并成
// 一个 FTS 候选集条件前置到 WHERE：
//
//	id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)
//
// 形态固定为「只读 rowid 的子查询」，绝不读 asset_path_fts 的列值：external content 表一旦按
// 列过滤，仓库级联删除留下的孤儿索引行会让 SQLite 报 267 missing row from content table
// （见 search_index_repo.go 文件头第 4 条）。子查询形态还顺带保住了 buildSearchWhere 与
// searchOrderBy 里的裸列名 `path`（JOIN 形态会让它变成 ambiguous column name）。
//
// 等价性：trigram 的子串匹配与 `path LIKE '%词%'` 同义；多个词用 FTS 的 AND 组合，与原多条
// LIKE 的 AND 一致。不可下推的部分（短于 3 字符的词、排除词、扩展名、仓库范围）原样留在
// LIKE / IN 条件里，作为候选集上的**后置**谓词——它们只会进一步收窄，不会放宽。
//
// 降级：索引未就绪、或没有任何可下推词时，整体退回 buildSearchWhere，行为与今天逐字一致。
func buildSearchWhereIndexed(f SearchFilter, repoIDs []int64) (string, []any) {
	pushed := indexableTerms(f.IncludeTerms)
	if !f.UseFullTextIndex || len(pushed) == 0 {
		return buildSearchWhere(f, repoIDs)
	}
	// 剩下的短词仍用 LIKE 表达（AND 语义不变），排除词、扩展名与版本条件一并原样保留。
	// 版本条件尤其不能漏：它没有 FTS 等价形态，丢了就等于「走索引时 version: 静默失效」。
	pushSet := make(map[string]bool, len(pushed))
	for _, t := range pushed {
		pushSet[t] = true
	}
	rest := make([]string, 0, len(f.IncludeTerms))
	for _, t := range f.IncludeTerms {
		if !pushSet[t] {
			rest = append(rest, t)
		}
	}
	remainder := SearchFilter{
		IncludeTerms:    rest,
		ExcludeTerms:    f.ExcludeTerms,
		IncludeExts:     f.IncludeExts,
		ExcludeExts:     f.ExcludeExts,
		IncludeVersions: f.IncludeVersions,
		ExcludeVersions: f.ExcludeVersions,
	}
	where, args := buildSearchWhere(remainder, repoIDs)
	cond := `id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)`
	// 候选集条件放在最前：它是最具选择性的谓词，先收敛行集再让后置 LIKE 复核。
	args = append([]any{ftsMatchExpression(pushed)}, args...)
	if where == "1=1" {
		return cond, args
	}
	return cond + " AND " + where, args
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
//
// ctx 承载领域层施加的搜索总预算：COUNT 与分页 SELECT 共享同一个 deadline（不是各给一份），
// 超时即中止，避免病态查询长时间占住池里的连接（主库池见 db.go 的 OpenWithMaxConns）。
func (r *AssetRepo) SearchByFilter(ctx context.Context, f SearchFilter, repoIDs []int64, sort, order string, limit, offset int) ([]Asset, int, error) {
	where, args := buildSearchWhereIndexed(f, repoIDs)
	var total int
	if err := r.db.GetContext(ctx, &total, `SELECT COUNT(*) FROM asset WHERE `+where, args...); err != nil {
		return nil, 0, err
	}
	var assets []Asset
	err := r.db.SelectContext(ctx, &assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE `+where+` ORDER BY `+searchOrderBy(sort, order)+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...,
	)
	return assets, total, err
}

// SearchAllByFilter 返回全部命中行（无分页），供包级聚合扫描候选集（FR-150 §3.1）。
//
// 与 SearchByFilter 共用同一段 WHERE（buildSearchWhereIndexed），因此聚合看到的行集与
// 分页/计数看到的完全一致。行按 **path 升序**返回：聚合的扫描预算要靠一个确定性顺序截断，
// 否则截断点会随执行计划抖动、同一请求两次返回不同的包集合。
//
// maxRows 是扫描上限；命中行超过上限时返回前 maxRows 行并置 truncated——此时聚合得到的
// 计数是下界，但**不影响 total**（total 仍由 SearchByFilter 的 COUNT(*) 精确给出）。
func (r *AssetRepo) SearchAllByFilter(ctx context.Context, f SearchFilter, repoIDs []int64, maxRows int) ([]Asset, bool, error) {
	if maxRows <= 0 {
		maxRows = 1
	}
	where, args := buildSearchWhereIndexed(f, repoIDs)
	var assets []Asset
	// 多取一行用于判定是否被截断，避免再发一条 COUNT。
	err := r.db.SelectContext(ctx, &assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM asset WHERE `+where+` ORDER BY path ASC LIMIT ?`,
		append(args, maxRows+1)...,
	)
	if err != nil {
		return nil, false, err
	}
	if len(assets) > maxRows {
		return assets[:maxRows], true, nil
	}
	return assets, false, nil
}

// SearchFacet 是搜索结果按仓库聚合的单条计数。
type SearchFacet struct {
	RepositoryID int64 `db:"repository_id"`
	Count        int   `db:"count"`
}

// SearchFacetsByFilter 返回同一搜索条件下各仓库的命中数（按命中数降序），
// 供前端做「按仓库聚合」的钻取导航。repoIDs 为空统计全部仓库。
// 与分页 SELECT 共用同一段 WHERE（含候选集条件），因此计数与分页不会互相矛盾。
func (r *AssetRepo) SearchFacetsByFilter(ctx context.Context, f SearchFilter, repoIDs []int64) ([]SearchFacet, error) {
	where, args := buildSearchWhereIndexed(f, repoIDs)
	var facets []SearchFacet
	err := r.db.SelectContext(ctx, &facets,
		`SELECT repository_id, COUNT(*) AS count FROM asset WHERE `+where+
			` GROUP BY repository_id ORDER BY count DESC`,
		args...,
	)
	return facets, err
}

// SearchPerRepoByFilter 在**每个**仓库内分别取前 perRepoLimit 条命中（各自按 path 升序），
// 用一条语句完成原本「按仓库循环调用」的工作（npm registry 搜索的成员聚合）。
//
// 语义与「对每个仓库各跑一次 SearchByFilter(limit=perRepoLimit, offset=0)」逐条一致：
// 窗口函数按 repository_id 分区、按 path 升序编号，只保留编号 <= perRepoLimit 的行；返回按
// (repository_id, path) 升序，调用方按自己的成员顺序分组即可还原成员循环的顺序。
// repoIDs 为空时无事可做（成员集合为空意味着调用方已判定无须搜索）。
func (r *AssetRepo) SearchPerRepoByFilter(ctx context.Context, f SearchFilter, repoIDs []int64, perRepoLimit int) ([]Asset, error) {
	if len(repoIDs) == 0 || perRepoLimit <= 0 {
		return nil, nil
	}
	where, args := buildSearchWhereIndexed(f, repoIDs)
	var assets []Asset
	err := r.db.SelectContext(ctx, &assets,
		`SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
			FROM (
				SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at,
					ROW_NUMBER() OVER (PARTITION BY repository_id ORDER BY path ASC) AS rn
				FROM asset WHERE `+where+`
			) WHERE rn <= ? ORDER BY repository_id ASC, path ASC`,
		append(args, perRepoLimit)...,
	)
	return assets, err
}

// SearchIndexReady 报告 asset_path_fts 的回填是否已完成
// （search_index_state.completed_at 非空，见迁移 0049 与规格 §3.1）。
//
// 由领域层在**每次搜索入口**调用一次，不做进程内缓存：全量重建会把 completed_at 打回 NULL，
// 而缓存里的 true 会让重建期间的搜索继续走已被清空的索引——结果是静默漏结果。这条查询是单行
// 主键点查，成本远低于缓存失效设计带来的正确性风险。
func (r *AssetRepo) SearchIndexReady() (bool, error) {
	var ready bool
	err := r.db.Get(&ready, `SELECT completed_at IS NOT NULL FROM search_index_state WHERE id = 1`)
	return ready, err
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
