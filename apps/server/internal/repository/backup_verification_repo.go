package repository

import (
	"database/sql"
	"errors"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 演练级别：shallow 只校验结构摘要，deep 额外逐 blob 比对内容摘要。
const (
	VerificationLevelShallow = "shallow"
	VerificationLevelDeep    = "deep"
)

// BackupVerification 是一次恢复演练的报告。
//
// 报告随包级联删除（外键 ON DELETE CASCADE）：包没了，针对它的报告没有意义。
type BackupVerification struct {
	ID         int64  `db:"id" json:"id"`
	PackageID  string `db:"package_id" json:"packageId"`
	Level      string `db:"level" json:"level"`
	OK         bool   `db:"ok" json:"ok"`
	StartedAt  string `db:"started_at" json:"startedAt"`
	FinishedAt string `db:"finished_at" json:"finishedAt"`
	DurationMS int64  `db:"duration_ms" json:"durationMs"`
	// ChecksJSON 是逐项结论（包完整性 / 解包 / SQLite 一致性 / 行数比对 / blob 摘要）。
	// 存 JSON 而不是给每项单开一列：项数会随演练强度演进，加项不该每次都动 schema。
	ChecksJSON string `db:"checks_json" json:"-"`
	// ErrorSummary 为失败原因；成功时为空。
	ErrorSummary string `db:"error_summary" json:"errorSummary"`
}

// BackupVerificationRepo 保存和查询恢复演练报告。
type BackupVerificationRepo struct{ db *persistence.DB }

func NewBackupVerificationRepo(db *persistence.DB) *BackupVerificationRepo {
	return &BackupVerificationRepo{db: db}
}

const backupVerificationColumns = `id, package_id, level, ok, started_at, finished_at,
	duration_ms, checks_json, error_summary`

// Create 写入一条演练报告并回填自增 id；StartedAt / FinishedAt 为空时填当前时间。
func (r *BackupVerificationRepo) Create(rec BackupVerification) (BackupVerification, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if rec.StartedAt == "" {
		rec.StartedAt = now
	}
	if rec.FinishedAt == "" {
		rec.FinishedAt = now
	}
	if rec.ChecksJSON == "" {
		rec.ChecksJSON = "{}"
	}
	res, err := r.db.Exec(`INSERT INTO backup_verification
		(package_id, level, ok, started_at, finished_at, duration_ms, checks_json, error_summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.PackageID, rec.Level, boolToInt(rec.OK), rec.StartedAt, rec.FinishedAt,
		rec.DurationMS, rec.ChecksJSON, rec.ErrorSummary)
	if err != nil {
		return rec, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return rec, err
	}
	rec.ID = id
	return rec, nil
}

// ListByPackage 按最近优先分页返回某包的历史演练报告。
func (r *BackupVerificationRepo) ListByPackage(packageID string, limit, offset int) ([]BackupVerification, error) {
	items := []BackupVerification{}
	err := r.db.Select(&items, `SELECT `+backupVerificationColumns+`
		FROM backup_verification WHERE package_id=?
		ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`, packageID, limit, offset)
	return items, err
}

// CountByPackage 返回某包的历史演练报告总数（供分页展示总数）。
func (r *BackupVerificationRepo) CountByPackage(packageID string) (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM backup_verification WHERE package_id=?`, packageID)
	return n, err
}

// Get 按 id 读取单条报告；不存在返回 ErrNotFound。
func (r *BackupVerificationRepo) Get(id int64) (*BackupVerification, error) {
	var rec BackupVerification
	err := r.db.Get(&rec, `SELECT `+backupVerificationColumns+` FROM backup_verification WHERE id=?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}
