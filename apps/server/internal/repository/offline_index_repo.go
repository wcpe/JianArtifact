package repository

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// OfflineDirIndex 索引任务元数据。
type OfflineDirIndex struct {
	RootPath     string         `db:"root_path"`
	Status       string         `db:"status"`
	Mode         string         `db:"mode"`
	TotalEntries int64          `db:"total_entries"`
	ScannedProps int64          `db:"scanned_props"`
	RepoCount    int64          `db:"repo_count"`
	Message      string         `db:"message"`
	ErrorMessage sql.NullString `db:"error_message"`
	StartedAt    sql.NullString `db:"started_at"`
	FinishedAt   sql.NullString `db:"finished_at"`
	UpdatedAt    string         `db:"updated_at"`
}

// OfflineDirIndexEntry 单条可迁移资产索引。
type OfflineDirIndexEntry struct {
	RootPath  string `db:"root_path"`
	Repo      string `db:"repo"`
	AssetPath string `db:"asset_path"`
	BytesPath string `db:"bytes_path"`
	PropPath  string `db:"prop_path"`
	PropMtime int64  `db:"prop_mtime"`
}

// OfflineIndexRepo 读写离线目录索引。
type OfflineIndexRepo struct{ db *persistence.DB }

// NewOfflineIndexRepo 构造。
func NewOfflineIndexRepo(db *persistence.DB) *OfflineIndexRepo {
	return &OfflineIndexRepo{db: db}
}

const (
	OfflineIndexIdle     = "idle"
	OfflineIndexScanning = "scanning"
	OfflineIndexReady    = "ready"
	OfflineIndexFailed   = "failed"
)

// Get 按绝对路径取索引元数据；不存在返回 ErrNotFound。
func (r *OfflineIndexRepo) Get(rootPath string) (*OfflineDirIndex, error) {
	var row OfflineDirIndex
	err := r.db.Get(&row, `SELECT * FROM offline_dir_index WHERE root_path = ?`, rootPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// UpsertMeta 插入或更新元数据。
func (r *OfflineIndexRepo) UpsertMeta(row OfflineDirIndex) error {
	_, err := r.db.Exec(`
INSERT INTO offline_dir_index (
  root_path, status, mode, total_entries, scanned_props, repo_count,
  message, error_message, started_at, finished_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(root_path) DO UPDATE SET
  status = excluded.status,
  mode = excluded.mode,
  total_entries = excluded.total_entries,
  scanned_props = excluded.scanned_props,
  repo_count = excluded.repo_count,
  message = excluded.message,
  error_message = excluded.error_message,
  started_at = excluded.started_at,
  finished_at = excluded.finished_at,
  updated_at = datetime('now')`,
		row.RootPath, row.Status, row.Mode, row.TotalEntries, row.ScannedProps, row.RepoCount,
		row.Message, row.ErrorMessage, row.StartedAt, row.FinishedAt,
	)
	return err
}

// ClearEntries 删除某 root 下全部条目。
func (r *OfflineIndexRepo) ClearEntries(rootPath string) error {
	_, err := r.db.Exec(`DELETE FROM offline_dir_index_entry WHERE root_path = ?`, rootPath)
	return err
}

// InsertEntries 批量插入条目（调用方保证事务外批量）。
func (r *OfflineIndexRepo) InsertEntries(entries []OfflineDirIndexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Preparex(`
INSERT INTO offline_dir_index_entry (root_path, repo, asset_path, bytes_path, prop_path, prop_mtime)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(root_path, repo, asset_path) DO UPDATE SET
  bytes_path = excluded.bytes_path,
  prop_path = excluded.prop_path,
  prop_mtime = excluded.prop_mtime`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range entries {
		if _, err := stmt.Exec(e.RootPath, e.Repo, e.AssetPath, e.BytesPath, e.PropPath, e.PropMtime); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteMissingProps 删除 prop_path 不在 keep 集合中的条目（update 扫描后清理）。
func (r *OfflineIndexRepo) DeleteMissingProps(rootPath string, keepPropPaths []string) (int64, error) {
	// 先取全部 prop，再差集删除，避免超大 IN
	var existing []string
	if err := r.db.Select(&existing, `SELECT prop_path FROM offline_dir_index_entry WHERE root_path = ?`, rootPath); err != nil {
		return 0, err
	}
	keep := make(map[string]bool, len(keepPropPaths))
	for _, p := range keepPropPaths {
		keep[p] = true
	}
	var removed int64
	for _, p := range existing {
		if keep[p] {
			continue
		}
		res, err := r.db.Exec(`DELETE FROM offline_dir_index_entry WHERE root_path = ? AND prop_path = ?`, rootPath, p)
		if err != nil {
			return removed, err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	return removed, nil
}

// CountByRepo 返回各仓资产数。
func (r *OfflineIndexRepo) CountByRepo(rootPath string) (map[string]int64, error) {
	type row struct {
		Repo  string `db:"repo"`
		Count int64  `db:"c"`
	}
	var rows []row
	err := r.db.Select(&rows, `
SELECT repo, COUNT(*) AS c FROM offline_dir_index_entry
WHERE root_path = ? GROUP BY repo ORDER BY repo`, rootPath)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, x := range rows {
		out[x.Repo] = x.Count
	}
	return out, nil
}

// ListRepos 返回索引中的仓库名列表。
func (r *OfflineIndexRepo) ListRepos(rootPath string) ([]string, error) {
	var names []string
	err := r.db.Select(&names, `
SELECT DISTINCT repo FROM offline_dir_index_entry WHERE root_path = ? ORDER BY repo`, rootPath)
	return names, err
}

// ListEntries 按仓库列出条目；repos 空则全部。
func (r *OfflineIndexRepo) ListEntries(rootPath string, repos []string) ([]OfflineDirIndexEntry, error) {
	if len(repos) == 0 {
		var all []OfflineDirIndexEntry
		err := r.db.Select(&all, `
SELECT root_path, repo, asset_path, bytes_path, prop_path, prop_mtime
FROM offline_dir_index_entry WHERE root_path = ? ORDER BY repo, asset_path`, rootPath)
		return all, err
	}
	// 逐仓查并合并（仓库数通常很少）
	var out []OfflineDirIndexEntry
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		var part []OfflineDirIndexEntry
		err := r.db.Select(&part, `
SELECT root_path, repo, asset_path, bytes_path, prop_path, prop_mtime
FROM offline_dir_index_entry WHERE root_path = ? AND repo = ? ORDER BY asset_path`, rootPath, repo)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

// TotalEntries 条目总数。
func (r *OfflineIndexRepo) TotalEntries(rootPath string) (int64, error) {
	var n int64
	err := r.db.Get(&n, `SELECT COUNT(*) FROM offline_dir_index_entry WHERE root_path = ?`, rootPath)
	return n, err
}
