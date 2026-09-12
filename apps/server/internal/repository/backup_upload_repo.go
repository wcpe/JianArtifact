package repository

import (
	"database/sql"
	"errors"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 分片上传会话状态机（FR-137 第三通道）：
// initialized → receiving → completed；中途取消置 aborted。
const (
	UploadStatusInitialized = "initialized"
	UploadStatusReceiving   = "receiving"
	UploadStatusCompleted   = "completed"
	UploadStatusAborted     = "aborted"
)

// BackupUpload 是一次 Web 分片上传会话的元数据记录（包体落盘在 ${JIAN_DATA_DIR}/backup-uploads/<uploadId>/）。
type BackupUpload struct {
	UploadID   string `db:"upload_id" json:"uploadId"`
	FileName   string `db:"file_name" json:"fileName"`
	TotalBytes int64  `db:"total_bytes" json:"totalBytes"`
	ChunkSize  int64  `db:"chunk_size" json:"chunkSize"`
	Status     string `db:"status" json:"status"`
	SHA256     string `db:"sha256" json:"sha256"`
	Operator   string `db:"operator" json:"operator"`
	CreatedAt  string `db:"created_at" json:"createdAt"`
	UpdatedAt  string `db:"updated_at" json:"updatedAt"`
	ExpiresAt  string `db:"expires_at" json:"expiresAt"`
}

// BackupUploadChunk 是一个已落盘分片的元数据（同 (upload_id, chunk_index) 幂等覆盖）。
type BackupUploadChunk struct {
	UploadID   string `db:"upload_id" json:"uploadId"`
	ChunkIndex int    `db:"chunk_index" json:"chunkIndex"`
	Size       int64  `db:"size" json:"size"`
	SHA256     string `db:"sha256" json:"sha256"`
}

// BackupUploadRepo 保存和查询分片上传会话及其分片。
type BackupUploadRepo struct{ db *persistence.DB }

func NewBackupUploadRepo(db *persistence.DB) *BackupUploadRepo {
	return &BackupUploadRepo{db: db}
}

const backupUploadColumns = `upload_id, file_name, total_bytes, chunk_size, status, sha256, operator, created_at, updated_at, expires_at`

// Create 登记一条上传会话；缺省时间戳填当前时间（UTC）。
func (r *BackupUploadRepo) Create(rec BackupUpload) error {
	now := time.Now().UTC()
	if rec.CreatedAt == "" {
		rec.CreatedAt = now.Format(time.RFC3339Nano)
	}
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = rec.CreatedAt
	}
	_, err := r.db.Exec(`INSERT INTO backup_upload
		(upload_id, file_name, total_bytes, chunk_size, status, sha256, operator, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.UploadID, rec.FileName, rec.TotalBytes, rec.ChunkSize, rec.Status, rec.SHA256, rec.Operator,
		rec.CreatedAt, rec.UpdatedAt, rec.ExpiresAt)
	return err
}

// Get 按会话 ID 读取；不存在返回 ErrNotFound。
func (r *BackupUploadRepo) Get(uploadID string) (*BackupUpload, error) {
	var rec BackupUpload
	err := r.db.Get(&rec, `SELECT `+backupUploadColumns+` FROM backup_upload WHERE upload_id=?`, uploadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// UpdateStatus 推进会话状态并刷新 updated_at。
func (r *BackupUploadRepo) UpdateStatus(uploadID, status string) error {
	_, err := r.db.Exec(`UPDATE backup_upload SET status=?, updated_at=? WHERE upload_id=?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), uploadID)
	return err
}

// AddChunk 记录一个分片；同 (upload_id, chunk_index) 覆盖（续传/重传幂等）。
func (r *BackupUploadRepo) AddChunk(chunk BackupUploadChunk) error {
	_, err := r.db.Exec(`INSERT INTO backup_upload_chunk (upload_id, chunk_index, size, sha256)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(upload_id, chunk_index) DO UPDATE SET size=excluded.size, sha256=excluded.sha256`,
		chunk.UploadID, chunk.ChunkIndex, chunk.Size, chunk.SHA256)
	return err
}

// ListChunks 按分片序号升序返回某会话的全部分片。
func (r *BackupUploadRepo) ListChunks(uploadID string) ([]BackupUploadChunk, error) {
	items := []BackupUploadChunk{}
	err := r.db.Select(&items, `SELECT upload_id, chunk_index, size, sha256
		FROM backup_upload_chunk WHERE upload_id=? ORDER BY chunk_index ASC`, uploadID)
	return items, err
}

// Count 返回上传会话总数。
func (r *BackupUploadRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM backup_upload`)
	return n, err
}

// ListExpired 返回所有 expires_at <= now 的会话（供启动期清理磁盘与记录）。
func (r *BackupUploadRepo) ListExpired(now string) ([]BackupUpload, error) {
	items := []BackupUpload{}
	err := r.db.Select(&items, `SELECT `+backupUploadColumns+` FROM backup_upload WHERE expires_at <= ?`, now)
	return items, err
}

// DeleteExpired 删除所有 expires_at <= now 的会话（级联清掉分片元数据），返回删除条数。
func (r *BackupUploadRepo) DeleteExpired(now string) (int, error) {
	res, err := r.db.Exec(`DELETE FROM backup_upload WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Delete 删除单条会话（级联清掉分片元数据）。
func (r *BackupUploadRepo) Delete(uploadID string) error {
	_, err := r.db.Exec(`DELETE FROM backup_upload WHERE upload_id=?`, uploadID)
	return err
}
