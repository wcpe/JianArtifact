package repository

import (
	"database/sql"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 迁移任务状态常量（与 ADR-0012 对齐）。
const (
	MigrationStatusPlanned   = "planned"
	MigrationStatusRunning   = "running"
	MigrationStatusCompleted = "completed"
	MigrationStatusFailed    = "failed"
	MigrationStatusCancelled = "cancelled"
)

// 迁移来源类型。
const (
	MigrationSourceOnlineREST    = "online_rest"
	MigrationSourceOfflineDir    = "offline_dir"
	MigrationSourceOfflineBundle = "offline_bundle"
)

// 冲突策略。
const (
	MigrationConflictSkip      = "skip"
	MigrationConflictOverwrite = "overwrite"
	MigrationConflictFail      = "fail"
)

// MigrationTask 是 migration_task 表的行模型。
// 注意：无明文密钥字段；CredentialRef 仅为环境变量引用名。
type MigrationTask struct {
	ID             int64          `db:"id"`
	Status         string         `db:"status"`
	SourceType     string         `db:"source_type"`
	SourceConfig   string         `db:"source_config"`
	CredentialRef  sql.NullString `db:"credential_ref"`
	ConflictPolicy string         `db:"conflict_policy"`
	PlanJSON       string         `db:"plan_json"`
	CheckpointJSON string         `db:"checkpoint_json"`
	ReportJSON     string         `db:"report_json"`
	ErrorMessage   sql.NullString `db:"error_message"`
	CreatedAt      string         `db:"created_at"`
	UpdatedAt      string         `db:"updated_at"`
	StartedAt      sql.NullString `db:"started_at"`
	FinishedAt     sql.NullString `db:"finished_at"`
}

// MigrationTaskCreate 创建任务时的输入（状态固定由调用方传入，一般为 planned）。
type MigrationTaskCreate struct {
	Status         string
	SourceType     string
	SourceConfig   string // JSON
	CredentialRef  string // 可空
	ConflictPolicy string
	PlanJSON       string
}

// MigrationTaskRepo 读写 migration_task 表。
type MigrationTaskRepo struct{ db *persistence.DB }

// NewMigrationTaskRepo 构造 MigrationTaskRepo。
func NewMigrationTaskRepo(db *persistence.DB) *MigrationTaskRepo {
	return &MigrationTaskRepo{db: db}
}

const migrationTaskSelect = `SELECT id, status, source_type, source_config, credential_ref,
	conflict_policy, plan_json, checkpoint_json, report_json, error_message,
	created_at, updated_at, started_at, finished_at FROM migration_task`

// Create 插入任务，返回新 ID。
func (r *MigrationTaskRepo) Create(in MigrationTaskCreate) (int64, error) {
	if in.SourceConfig == "" {
		in.SourceConfig = "{}"
	}
	if in.PlanJSON == "" {
		in.PlanJSON = "{}"
	}
	if in.ConflictPolicy == "" {
		in.ConflictPolicy = MigrationConflictSkip
	}
	var cred any
	if in.CredentialRef != "" {
		cred = in.CredentialRef
	}
	res, err := r.db.Exec(
		`INSERT INTO migration_task (status, source_type, source_config, credential_ref, conflict_policy, plan_json)
			VALUES (?, ?, ?, ?, ?, ?)`,
		in.Status, in.SourceType, in.SourceConfig, cred, in.ConflictPolicy, in.PlanJSON,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetByID 按 ID 取任务；不存在返回 ErrNotFound。
func (r *MigrationTaskRepo) GetByID(id int64) (*MigrationTask, error) {
	var t MigrationTask
	err := r.db.Get(&t, migrationTaskSelect+` WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// List 返回分页任务（按 id 降序，新任务在前）。
func (r *MigrationTaskRepo) List(limit, offset int) ([]MigrationTask, error) {
	var items []MigrationTask
	err := r.db.Select(&items, migrationTaskSelect+` ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	return items, err
}

// Count 返回任务总数。
func (r *MigrationTaskRepo) Count() (int, error) {
	var n int
	err := r.db.Get(&n, `SELECT COUNT(*) FROM migration_task`)
	return n, err
}

// UpdateStatus 更新状态；可选 error_message、started_at、finished_at 标记。
// setStarted/setFinished 为 true 时写入对应时间戳（仅当原列为空时保留首次值的语义由调用方控制，此处直接覆盖写 datetime('now')）。
func (r *MigrationTaskRepo) UpdateStatus(id int64, status string, errMsg *string, setStarted, setFinished bool) error {
	// 分路径拼 SQL，避免动态拼接带来的注入风险（仅字面量片段）。
	if setStarted && setFinished {
		res, err := r.db.Exec(
			`UPDATE migration_task SET status = ?, error_message = ?,
				started_at = COALESCE(started_at, datetime('now')),
				finished_at = datetime('now'),
				updated_at = datetime('now') WHERE id = ?`,
			status, nullStr(errMsg), id,
		)
		return affected(res, err)
	}
	if setStarted {
		res, err := r.db.Exec(
			`UPDATE migration_task SET status = ?, error_message = ?,
				started_at = COALESCE(started_at, datetime('now')),
				updated_at = datetime('now') WHERE id = ?`,
			status, nullStr(errMsg), id,
		)
		return affected(res, err)
	}
	if setFinished {
		res, err := r.db.Exec(
			`UPDATE migration_task SET status = ?, error_message = ?,
				finished_at = datetime('now'),
				updated_at = datetime('now') WHERE id = ?`,
			status, nullStr(errMsg), id,
		)
		return affected(res, err)
	}
	res, err := r.db.Exec(
		`UPDATE migration_task SET status = ?, error_message = ?, updated_at = datetime('now') WHERE id = ?`,
		status, nullStr(errMsg), id,
	)
	return affected(res, err)
}

// SaveCheckpoint 覆盖写入 checkpoint_json。
func (r *MigrationTaskRepo) SaveCheckpoint(id int64, checkpointJSON string) error {
	if checkpointJSON == "" {
		checkpointJSON = "{}"
	}
	res, err := r.db.Exec(
		`UPDATE migration_task SET checkpoint_json = ?, updated_at = datetime('now') WHERE id = ?`,
		checkpointJSON, id,
	)
	return affected(res, err)
}

// SaveReportMeta 覆盖写入 report_json。
func (r *MigrationTaskRepo) SaveReportMeta(id int64, reportJSON string) error {
	if reportJSON == "" {
		reportJSON = "{}"
	}
	res, err := r.db.Exec(
		`UPDATE migration_task SET report_json = ?, updated_at = datetime('now') WHERE id = ?`,
		reportJSON, id,
	)
	return affected(res, err)
}

// SavePlan 覆盖写入 plan_json。
func (r *MigrationTaskRepo) SavePlan(id int64, planJSON string) error {
	if planJSON == "" {
		planJSON = "{}"
	}
	res, err := r.db.Exec(
		`UPDATE migration_task SET plan_json = ?, updated_at = datetime('now') WHERE id = ?`,
		planJSON, id,
	)
	return affected(res, err)
}

// FailInterruptedRunning 将所有 running 任务标为 failed（进程崩溃回收）。
// 返回受影响行数。
func (r *MigrationTaskRepo) FailInterruptedRunning(message string) (int64, error) {
	if message == "" {
		message = "进程中断，请 resume"
	}
	res, err := r.db.Exec(
		`UPDATE migration_task SET status = ?, error_message = ?,
			finished_at = datetime('now'), updated_at = datetime('now')
		WHERE status = ?`,
		MigrationStatusFailed, message, MigrationStatusRunning,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListByStatus 列出指定状态的任务（调试/回收用）。
func (r *MigrationTaskRepo) ListByStatus(status string) ([]MigrationTask, error) {
	var items []MigrationTask
	err := r.db.Select(&items, migrationTaskSelect+` WHERE status = ? ORDER BY id`, status)
	return items, err
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
