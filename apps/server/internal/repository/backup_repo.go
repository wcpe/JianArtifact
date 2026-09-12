package repository

import (
	"database/sql"
	"errors"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 备份包状态机（FR-132）：queued → snapshotting → packing → done / failed。
const (
	BackupStatusQueued       = "queued"
	BackupStatusSnapshotting = "snapshotting"
	BackupStatusPacking      = "packing"
	BackupStatusDone         = "done"
	BackupStatusFailed       = "failed"
)

// 备份包生成模式。
const (
	BackupModeHot    = "hot"
	BackupModeFrozen = "frozen"
)

// BackupPackage 是备份包的登记元数据（包体不在库内）。
type BackupPackage struct {
	PackageID       string  `db:"package_id" json:"packageId"`
	Mode            string  `db:"mode" json:"mode"`
	BasePackageID   string  `db:"base_package_id" json:"basePackageId"`
	Status          string  `db:"status" json:"status"`
	Label           string  `db:"label" json:"label"`
	SizeBytes       int64   `db:"size_bytes" json:"sizeBytes"`
	CountsJSON      string  `db:"counts_json" json:"-"`
	NodeID          string  `db:"node_id" json:"nodeId"`
	AppVersion      string  `db:"app_version" json:"appVersion"`
	DBSchemaVersion int     `db:"db_schema_version" json:"dbSchemaVersion"`
	CreatedAt       string  `db:"created_at" json:"createdAt"`
	FinishedAt      *string `db:"finished_at" json:"finishedAt,omitempty"`
	ErrorSummary    string  `db:"error_summary" json:"errorSummary"`
}

// IsIncremental 表示该包是在某个基线包之上生成的差包。
func (p BackupPackage) IsIncremental() bool { return p.BasePackageID != "" }

// BackupPackageRepo 保存和查询备份包登记。
type BackupPackageRepo struct{ db *persistence.DB }

func NewBackupPackageRepo(db *persistence.DB) *BackupPackageRepo {
	return &BackupPackageRepo{db: db}
}

const backupPackageColumns = `package_id, mode, base_package_id, status, label, size_bytes,
	counts_json, node_id, app_version, db_schema_version, created_at, finished_at, error_summary`

// Create 登记一个新包；CreatedAt 为空时填当前时间。
func (r *BackupPackageRepo) Create(p BackupPackage) error {
	if p.CreatedAt == "" {
		p.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := r.db.Exec(`INSERT INTO backup_package
		(package_id, mode, base_package_id, status, label, size_bytes, counts_json,
		 node_id, app_version, db_schema_version, created_at, finished_at, error_summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PackageID, p.Mode, p.BasePackageID, p.Status, p.Label, p.SizeBytes, p.CountsJSON,
		p.NodeID, p.AppVersion, p.DBSchemaVersion, p.CreatedAt, p.FinishedAt, p.ErrorSummary)
	return err
}

// Get 按包 ID 读取；不存在返回 ErrNotFound。
func (r *BackupPackageRepo) Get(packageID string) (*BackupPackage, error) {
	var p BackupPackage
	err := r.db.Get(&p, `SELECT `+backupPackageColumns+` FROM backup_package WHERE package_id=?`, packageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// List 按最近优先分页读取。
func (r *BackupPackageRepo) List(limit, offset int) ([]BackupPackage, error) {
	items := []BackupPackage{}
	err := r.db.Select(&items, `SELECT `+backupPackageColumns+`
		FROM backup_package ORDER BY created_at DESC, package_id DESC LIMIT ? OFFSET ?`, limit, offset)
	return items, err
}

// Count 返回登记总数。
func (r *BackupPackageRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM backup_package`)
	return n, err
}

// UpdateProgress 更新生成中包的状态与规模（未完成时 finished_at 保持为空）。
func (r *BackupPackageRepo) UpdateProgress(packageID, status string, sizeBytes int64, countsJSON string) error {
	_, err := r.db.Exec(`UPDATE backup_package SET status=?, size_bytes=?, counts_json=? WHERE package_id=?`,
		status, sizeBytes, countsJSON, packageID)
	return err
}

// Finish 收尾生成：标记 done 或 failed 并写完成时间与失败摘要。
func (r *BackupPackageRepo) Finish(packageID string, success bool, sizeBytes int64, countsJSON, errorSummary string) error {
	status := BackupStatusDone
	if !success {
		status = BackupStatusFailed
	}
	finished := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := r.db.Exec(`UPDATE backup_package
		SET status=?, size_bytes=?, counts_json=?, finished_at=?, error_summary=?
		WHERE package_id=?`, status, sizeBytes, countsJSON, finished, errorSummary, packageID)
	return err
}

// Delete 删除登记行。
func (r *BackupPackageRepo) Delete(packageID string) error {
	_, err := r.db.Exec(`DELETE FROM backup_package WHERE package_id=?`, packageID)
	return err
}

// HasDerived 判断是否仍有增量差包以该包为基线；删除基线前必须为 false，
// 否则派生包将永远无法在目标端还原。
func (r *BackupPackageRepo) HasDerived(basePackageID string) (bool, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM backup_package WHERE base_package_id=?`, basePackageID)
	return n > 0, err
}

// MarkInterrupted 把进程重启遗留的生成中包标记为失败（与复制轮次孤儿清理同理）。
// 返回受影响的行数。
func (r *BackupPackageRepo) MarkInterrupted(summary string) (int64, error) {
	res, err := r.db.Exec(`UPDATE backup_package
		SET status=?, finished_at=?, error_summary=?
		WHERE status IN (?, ?, ?)`,
		BackupStatusFailed, time.Now().UTC().Format(time.RFC3339Nano), summary,
		BackupStatusQueued, BackupStatusSnapshotting, BackupStatusPacking)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
