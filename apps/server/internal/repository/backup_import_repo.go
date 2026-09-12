package repository

import (
	"database/sql"
	"errors"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 导入状态机（FR-137）：queued → fetching → staging → pending_restart；任意失败置 failed。
const (
	ImportStatusQueued         = "queued"
	ImportStatusFetching       = "fetching"
	ImportStatusStaging        = "staging"
	ImportStatusPendingRestart = "pending_restart"
	ImportStatusDone           = "done"
	ImportStatusFailed         = "failed"
)

// 导入通道来源。
const (
	ImportOriginURL    = "url"
	ImportOriginUpload = "upload"
	ImportOriginCLI    = "cli"
)

// BackupImport 是一次备份包导入的记录（包体由调用方下载落盘，记录只存元数据与进度）。
type BackupImport struct {
	ImportID         string  `db:"import_id" json:"importId"`
	Origin           string  `db:"origin" json:"origin"`
	Status           string  `db:"status" json:"status"`
	SourceURL        string  `db:"source_url" json:"sourceUrl"`
	Operator         string  `db:"operator" json:"operator"`
	Overwrite        bool    `db:"overwrite" json:"overwrite"`
	Deep             bool    `db:"deep" json:"deep"`
	TotalBytes       int64   `db:"total_bytes" json:"totalBytes"`
	FetchedBytes     int64   `db:"fetched_bytes" json:"fetchedBytes"`
	BlobCount        int     `db:"blob_count" json:"blobCount"`
	PackageID        string  `db:"package_id" json:"packageId"`
	ErrorCode        string  `db:"error_code" json:"errorCode"`
	ErrorSummary     string  `db:"error_summary" json:"error"`
	CreatedAt        string  `db:"created_at" json:"createdAt"`
	UpdatedAt        string  `db:"updated_at" json:"updatedAt"`
	FinishedAt       *string `db:"finished_at" json:"finishedAt,omitempty"`
	RestorePendingAt *string `db:"restore_pending_at" json:"restorePendingAt,omitempty"`
}

// BackupImportRepo 保存和查询备份包导入记录。
type BackupImportRepo struct{ db *persistence.DB }

func NewBackupImportRepo(db *persistence.DB) *BackupImportRepo {
	return &BackupImportRepo{db: db}
}

const backupImportColumns = `import_id, origin, status, source_url, operator, overwrite, deep,
	total_bytes, fetched_bytes, blob_count, package_id, error_code, error_summary,
	created_at, updated_at, finished_at, restore_pending_at`

// Create 登记一条导入记录；缺省时间戳填当前时间（UTC）。
func (r *BackupImportRepo) Create(rec BackupImport) error {
	now := time.Now().UTC()
	if rec.CreatedAt == "" {
		rec.CreatedAt = now.Format(time.RFC3339Nano)
	}
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = rec.CreatedAt
	}
	_, err := r.db.Exec(`INSERT INTO backup_import
		(import_id, origin, status, source_url, operator, overwrite, deep,
		 total_bytes, fetched_bytes, blob_count, package_id, error_code, error_summary,
		 created_at, updated_at, finished_at, restore_pending_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ImportID, rec.Origin, rec.Status, rec.SourceURL, rec.Operator,
		boolToInt(rec.Overwrite), boolToInt(rec.Deep),
		rec.TotalBytes, rec.FetchedBytes, rec.BlobCount, rec.PackageID, rec.ErrorCode, rec.ErrorSummary,
		rec.CreatedAt, rec.UpdatedAt, rec.FinishedAt, rec.RestorePendingAt)
	return err
}

// Get 按导入 ID 读取；不存在返回 ErrNotFound。
func (r *BackupImportRepo) Get(importID string) (*BackupImport, error) {
	var rec BackupImport
	err := r.db.Get(&rec, `SELECT `+backupImportColumns+` FROM backup_import WHERE import_id=?`, importID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// List 按最近优先分页读取。
func (r *BackupImportRepo) List(limit, offset int) ([]BackupImport, error) {
	items := []BackupImport{}
	err := r.db.Select(&items, `SELECT `+backupImportColumns+`
		FROM backup_import ORDER BY created_at DESC, import_id DESC LIMIT ? OFFSET ?`, limit, offset)
	return items, err
}

// Count 返回导入记录总数。
func (r *BackupImportRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM backup_import`)
	return n, err
}

// UpdateProgress 推进下载中的导入：状态 + 已下载字节 + 声明总字节；updated_at 刷新。
func (r *BackupImportRepo) UpdateProgress(importID, status string, fetchedBytes, totalBytes int64) error {
	_, err := r.db.Exec(`UPDATE backup_import
		SET status=?, fetched_bytes=?, total_bytes=?, updated_at=? WHERE import_id=?`,
		status, fetchedBytes, totalBytes, time.Now().UTC().Format(time.RFC3339Nano), importID)
	return err
}

// Finish 收尾导入：置终态（pending_restart / failed），写错误码与摘要、包标识、blob 数、
// 待生效时间（可为空）与完成时间、刷新 updated_at。终态记录不再推进。
func (r *BackupImportRepo) Finish(importID, status, errorCode, errorSummary, packageID string, blobCount int, restorePendingAt *string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := r.db.Exec(`UPDATE backup_import
		SET status=?, error_code=?, error_summary=?, package_id=?, blob_count=?,
		    restore_pending_at=?, finished_at=?, updated_at=?
		WHERE import_id=?`,
		status, errorCode, errorSummary, packageID, blobCount,
		restorePendingAt, now, now, importID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
