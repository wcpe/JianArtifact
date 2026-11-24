package repository

import (
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 资产回收站条目状态。与 trash_entry 的三态同名同义，但**不共用表**：
// 本表记录的是「元数据事实」（asset 行快照），trash_entry 记录的是「文件系统事实」。
const (
	AssetTrashStaged   = "staged"
	AssetTrashRestored = "restored"
	AssetTrashPurged   = "purged"
)

// 资产回收站条目的删除范围，仅用于展示与审计追溯。
const (
	AssetTrashScopeVersion       = "version"
	AssetTrashScopeSnapshotBuild = "snapshot_build"
	AssetTrashScopeArtifact      = "artifact"
)

// AssetTrashEntry 是 asset_trash 的行模型：被软删除的 asset 完整快照。
//
// 为什么要把 asset 的全部展示相关字段都存下来：恢复必须还原到与删除前**完全一致**的状态，
// 包括时间戳与校验和。只存 path + blob_hash 会让恢复后的制品在列表排序、时间显示和
// 校验和字段上与原件不同——用户点的是「恢复」，不是「重新上传一份内容相同的文件」。
type AssetTrashEntry struct {
	ID           int64   `db:"id" json:"id"`
	RepositoryID int64   `db:"repository_id" json:"repositoryId"`
	Path         string  `db:"path" json:"path"`
	BlobHash     string  `db:"blob_hash" json:"blobHash"`
	Size         int64   `db:"size" json:"size"`
	ContentType  string  `db:"content_type" json:"contentType"`
	Sha1         string  `db:"sha1" json:"sha1"`
	Md5          string  `db:"md5" json:"md5"`
	CreatedAt    string  `db:"created_at" json:"createdAt"`
	UpdatedAt    string  `db:"updated_at" json:"updatedAt"`
	Scope        string  `db:"scope" json:"scope"`
	Actor        string  `db:"actor" json:"actor"`
	OperationID  string  `db:"operation_id" json:"operationId"`
	Status       string  `db:"status" json:"status"`
	TrashedAt    string  `db:"trashed_at" json:"trashedAt"`
	ResolvedAt   *string `db:"resolved_at" json:"resolvedAt,omitempty"`

	// RepositoryName 是非列字段：列表展示需要仓库名，但表里只存 id。
	// 由查询处 LEFT JOIN repository 填充，避免前端为每行再发一次请求。
	RepositoryName string `db:"repository_name" json:"repositoryName,omitempty"`
}

// AssetTrashStats 是回收站占用统计。
//
// 口径是「被回收站**保留**的字节」：这些 blob 在保留期内仍占磁盘，因此它同时也是
// 「回收站最多能为你省下多少」的上界，与存储清理回收站的 StagedTrashStats 同义。
type AssetTrashStats struct {
	Entries int   `db:"entries" json:"entries"`
	Bytes   int64 `db:"bytes" json:"bytes"`
}

// AssetTrashRepo 读写 asset_trash 表。
type AssetTrashRepo struct{ db *persistence.DB }

// NewAssetTrashRepo 构造 AssetTrashRepo。
func NewAssetTrashRepo(db *persistence.DB) *AssetTrashRepo { return &AssetTrashRepo{db: db} }

const assetTrashColumns = `t.id, t.repository_id, t.path, t.blob_hash, t.size, t.content_type,
	t.sha1, t.md5, t.created_at, t.updated_at, t.scope, t.actor, t.operation_id, t.status,
	t.trashed_at, t.resolved_at`

// InsertStaged 在调用方事务内把一批资产快照写入回收站。
//
// 必须由调用方传入事务：这保证了「从 asset 表删除」与「写入 asset_trash」是**同一个
// 事务**。分开提交会在中间崩溃时留下「元数据没了、回收站里也没有」的孤儿状态——
// 此时 blob 仍占磁盘，但既不可恢复也不可见，只能等保留期到期由 GC 兜底，属于最难排查的形态。
func InsertAssetTrashStaged(tx *sqlx.Tx, entries []AssetTrashEntry) error {
	if len(entries) == 0 {
		return nil
	}
	for i := range entries {
		status := entries[i].Status
		if status == "" {
			status = AssetTrashStaged
		}
		scope := entries[i].Scope
		if scope == "" {
			scope = AssetTrashScopeVersion
		}
		res, err := tx.Exec(
			`INSERT INTO asset_trash (repository_id, path, blob_hash, size, content_type, sha1, md5,
				created_at, updated_at, scope, actor, operation_id, status, trashed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
			entries[i].RepositoryID, entries[i].Path, entries[i].BlobHash, entries[i].Size,
			entries[i].ContentType, entries[i].Sha1, entries[i].Md5,
			entries[i].CreatedAt, entries[i].UpdatedAt, scope, entries[i].Actor,
			entries[i].OperationID, status)
		if err != nil {
			return err
		}
		// 回填自增 id：调用方在同一事务内失败回滚时要按 id 定位已写入的行，
		// 与 trash_entry.InsertTrashEntries 同一口径。
		id, idErr := res.LastInsertId()
		if idErr != nil {
			return idErr
		}
		entries[i].ID = id
		entries[i].Status = status
	}
	return nil
}

// List 分页列出回收站条目（默认只看 staged），并填充仓库名。
//
// 排序用 trashed_at DESC, id DESC：同一批量删除的条目 trashed_at 可能同秒，
// 用 id 兜底才能让分页稳定（否则翻页会重复或漏行）。
func (r *AssetTrashRepo) List(status string, limit, offset int) ([]AssetTrashEntry, int, error) {
	if status == "" {
		status = AssetTrashStaged
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM asset_trash WHERE status = ?`, status); err != nil {
		return nil, 0, err
	}
	var entries []AssetTrashEntry
	// LEFT JOIN 而非 JOIN：仓库被删时 asset_trash 通过 CASCADE 一并清理，因此理论上
	// 不会出现孤立行；但用 LEFT JOIN 可保证即使出现（如手工改库）也不会让整页查询失败。
	err := r.db.Select(&entries,
		`SELECT `+assetTrashColumns+`, COALESCE(r.name, '') AS repository_name
		 FROM asset_trash t LEFT JOIN repository r ON r.id = t.repository_id
		 WHERE t.status = ?
		 ORDER BY t.trashed_at DESC, t.id DESC LIMIT ? OFFSET ?`, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// Stats 返回回收站占用（staged 条目数与字节合计）。
func (r *AssetTrashRepo) Stats() (AssetTrashStats, error) {
	var stats AssetTrashStats
	err := r.db.Get(&stats,
		`SELECT COUNT(*) AS entries, COALESCE(SUM(size), 0) AS bytes
		 FROM asset_trash WHERE status = ?`, AssetTrashStaged)
	return stats, err
}

// Get 读取单条回收站条目；不存在返回 sql.ErrNoRows。
func (r *AssetTrashRepo) Get(id int64) (*AssetTrashEntry, error) {
	var entry AssetTrashEntry
	err := r.db.Get(&entry,
		`SELECT `+assetTrashColumns+`, COALESCE(r.name, '') AS repository_name
		 FROM asset_trash t LEFT JOIN repository r ON r.id = t.repository_id
		 WHERE t.id = ?`, id)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// GetStagedByPath 读取某一路径当前的 staged 条目；不存在返回 sql.ErrNoRows。
//
// 恢复前用它判断「该路径是否已有一条在回收站的记录」：同一路径重复删除时，
// 部分唯一索引 idx_asset_trash_active_path 会拒绝第二次写入（见迁移注释），
// 这里提供显式的前置判断，好让调用方给出可读的冲突原因而不是一个约束错误。
func (r *AssetTrashRepo) GetStagedByPath(repoID int64, path string) (*AssetTrashEntry, error) {
	var entry AssetTrashEntry
	err := r.db.Get(&entry,
		`SELECT `+assetTrashColumns+` FROM asset_trash t
		 WHERE t.repository_id = ? AND t.path = ? AND t.status = ?`,
		repoID, path, AssetTrashStaged)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// 恢复/彻底清理都必须能接受调用方事务，理由与插入相同：状态迁移与 asset 表写回
// 必须是同一个事务，否则会留下「已标 restored 但资产没写回」的死条目。
func markAssetTrashResolvedTx(tx *sqlx.Tx, id int64, status string) error {
	if status != AssetTrashRestored && status != AssetTrashPurged {
		return errors.New("非法的回收站终态")
	}
	_, err := tx.Exec(
		`UPDATE asset_trash SET status = ?, resolved_at = datetime('now')
		 WHERE id = ? AND status = ?`, status, id, AssetTrashStaged)
	return err
}

// MarkResolved 在调用方事务内把一条 staged 条目迁移到终态。
// 只有当前仍为 staged 才会生效（避免并发重复恢复把已处理的行再改一次）。
func MarkAssetTrashResolved(tx *sqlx.Tx, id int64, status string) error {
	return markAssetTrashResolvedTx(tx, id, status)
}

// ListStagedByIds 按 id 批量读取 staged 条目，按传入顺序返回命中的那些。
//
// 顺序敏感：恢复要按 id 顺序逐条处理并把结果对回请求，乱序会让「第 2 条冲突」
// 报成第 1 条。
func (r *AssetTrashRepo) ListStagedByIds(ids []int64) ([]AssetTrashEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query, args, err := sqlx.In(
		`SELECT `+assetTrashColumns+` FROM asset_trash t WHERE t.id IN (?) AND t.status = ?`,
		ids, AssetTrashStaged)
	if err != nil {
		return nil, err
	}
	var rows []AssetTrashEntry
	if err := r.db.Select(&rows, r.db.Rebind(query), args...); err != nil {
		return nil, err
	}
	byID := make(map[int64]AssetTrashEntry, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]AssetTrashEntry, 0, len(ids))
	for _, id := range ids {
		if row, ok := byID[id]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// ExpiredStaged 返回 trashed_at 早于 cutoff 的 staged 条目（保留期回收用）。
func (r *AssetTrashRepo) ExpiredStaged(cutoff time.Time, limit int) ([]AssetTrashEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	var rows []AssetTrashEntry
	err := r.db.Select(&rows,
		`SELECT `+assetTrashColumns+` FROM asset_trash t
		 WHERE t.status = ? AND t.trashed_at < ? ORDER BY t.trashed_at ASC, t.id ASC LIMIT ?`,
		AssetTrashStaged, cutoff.UTC().Format("2006-01-02 15:04:05"), limit)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// CountStagedByBlobHash 返回该 blob 仍被回收站引用的 staged 条目数。
//
// 这是 blob 存活的**第二类引用来源**，与 asset 表的引用同等重要：只保留在回收站里的
// blob 仍然是「可恢复的数据」，绝不能被孤立回收当成垃圾删掉。
// 若漏掉这一处，删除后立刻恢复会得到一个指向已消失 blob 的资产行——数据静默丢失。
func (r *AssetTrashRepo) CountStagedByBlobHash(hash string) (int, error) {
	var count int
	err := r.db.Get(&count,
		`SELECT COUNT(*) FROM asset_trash WHERE blob_hash = ? AND status = ?`,
		hash, AssetTrashStaged)
	return count, err
}

// HasStagedBlob 判断某 blob 是否仍被回收站引用（供 blob 存活判定使用）。
func (r *AssetTrashRepo) HasStagedBlob(hash string) (bool, error) {
	count, err := r.CountStagedByBlobHash(hash)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
