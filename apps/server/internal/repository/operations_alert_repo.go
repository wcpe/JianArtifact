package repository

import (
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// OperationsAlertRow 是运维告警去重持久化行：同一 (code, source) 仅存一份。
// first_observed_at 保留首次发现时间，last_observed_at 随每次观察到滚动更新；
// blocked_until 仅 upstream_auto_blocked 携带（自动阻止窗口截止，可为空）。
type OperationsAlertRow struct {
	Code            string  `db:"code"`
	Source          string  `db:"source"`
	Severity        string  `db:"severity"`
	FirstObservedAt string  `db:"first_observed_at"`
	LastObservedAt  string  `db:"last_observed_at"`
	BlockedUntil    *string `db:"blocked_until"`
}

// OperationsAlertRepo 管理当前节点运维告警的去重持久化；它从不参与复制。
type OperationsAlertRepo struct{ db *persistence.DB }

func NewOperationsAlertRepo(db *persistence.DB) *OperationsAlertRepo {
	return &OperationsAlertRepo{db: db}
}

// UpsertActive 以 (code, source) 为指纹合并当前告警：
// 存在则保留 first_observed_at 并滚动 last_observed_at / blocked_until，否则插入新行。
func (r *OperationsAlertRepo) UpsertActive(rows []OperationsAlertRow) error {
	for _, row := range rows {
		_, err := r.db.Exec(`INSERT INTO operations_alert
			(code, source, severity, first_observed_at, last_observed_at, blocked_until)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(code, source) DO UPDATE SET
			severity = excluded.severity,
			last_observed_at = excluded.last_observed_at,
			blocked_until = excluded.blocked_until`,
			row.Code, row.Source, row.Severity, row.FirstObservedAt, row.LastObservedAt, row.BlockedUntil)
		if err != nil {
			return err
		}
	}
	return nil
}

// RemoveRecovered 删除当前不再活跃的告警（健康恢复后清理，避免历史告警残留）。
func (r *OperationsAlertRepo) RemoveRecovered(activeKeys map[string]bool) error {
	rows, err := r.List()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !activeKeys[alertRowKey(row.Code, row.Source)] {
			if _, err := r.db.Exec(`DELETE FROM operations_alert WHERE code = ? AND source = ?`,
				row.Code, row.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

// List 返回当前全部持久化告警，按 last_observed_at 倒序（最新优先）。
func (r *OperationsAlertRepo) List() ([]OperationsAlertRow, error) {
	rows := []OperationsAlertRow{}
	err := r.db.Select(&rows, `SELECT code, source, severity, first_observed_at, last_observed_at, blocked_until
		FROM operations_alert ORDER BY last_observed_at DESC`)
	return rows, err
}

// 以下时间工具供 api 层复用，避免重复格式化。
func FormatMetricTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func alertRowKey(code, source string) string { return code + "\x00" + source }
