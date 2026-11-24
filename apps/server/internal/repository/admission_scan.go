package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

// AdmissionScanJobRow 是 admission_scan_job 的持久投影。
type AdmissionScanJobRow struct {
	JobID                string `db:"job_id"`
	OperationID          string `db:"operation_id"`
	ManifestVersion      int64  `db:"manifest_version"`
	Engine               string `db:"engine"`
	EngineVersion        string `db:"engine_version"`
	RulesVersion         string `db:"rules_version"`
	ParserProfileVersion string `db:"parser_profile_version"`
	State                string `db:"state"`
	LeaseGeneration      int64  `db:"lease_generation"`
	LeaseUntilUnix       int64  `db:"lease_until_unix"`
	WorkerID             string `db:"worker_id"`
	Attempts             int64  `db:"attempts"`
	MaxAttempts          int64  `db:"max_attempts"`
	NextRunAtUnix        int64  `db:"next_run_at_unix"`
	Verdict              string `db:"verdict"`
	ErrorCode            string `db:"error_code"`
	CreatedUnix          int64  `db:"created_unix"`
	UpdatedUnix          int64  `db:"updated_unix"`
}

// AdmissionScanFactRow 是 admission_scan_fact 的有界事实行：不含原始字节或密钥。
type AdmissionScanFactRow struct {
	JobID             string `db:"job_id"`
	Ordinal           int64  `db:"ordinal"`
	LeaseGeneration   int64  `db:"lease_generation"`
	ObjectDigest      string `db:"object_digest"`
	CoverageRequired  int64  `db:"coverage_required"`
	CoverageCompleted int64  `db:"coverage_completed"`
	CoverageMissing   string `db:"coverage_missing"`
	Integrity         string `db:"integrity"`
	Verdict           string `db:"verdict"`
	Findings          string `db:"findings"`
	WorkerID          string `db:"worker_id"`
	ObservedAtUnix    int64  `db:"observed_at_unix"`
}

// AdmissionScanJobRepo 只承载事务级操作；调用方用候选仓储的 Read/Write 传事务。
type AdmissionScanJobRepo struct{}

func NewAdmissionScanJobRepo() *AdmissionScanJobRepo { return &AdmissionScanJobRepo{} }

// EnqueueTx 登记或取回当前清单版本的扫描任务；同 (operation,manifest) 去重。
func (r *AdmissionScanJobRepo) EnqueueTx(ctx context.Context, tx *sqlx.Tx, job AdmissionScanJobRow) (AdmissionScanJobRow, error) {
	_, err := tx.NamedExecContext(ctx, `INSERT OR IGNORE INTO admission_scan_job
 (job_id,operation_id,manifest_version,engine,engine_version,rules_version,parser_profile_version,state,next_run_at_unix,created_unix,updated_unix)
 VALUES (:job_id,:operation_id,:manifest_version,:engine,:engine_version,:rules_version,:parser_profile_version,'queued',:next_run_at_unix,:created_unix,:updated_unix)`, job)
	if err != nil {
		return AdmissionScanJobRow{}, err
	}
	return r.GetByOperationTx(ctx, tx, job.OperationID, job.ManifestVersion)
}

// GetByOperationTx 读取某候选当前清单版本的扫描任务。
func (r *AdmissionScanJobRepo) GetByOperationTx(ctx context.Context, tx *sqlx.Tx, operationID string, manifestVersion int64) (AdmissionScanJobRow, error) {
	var row AdmissionScanJobRow
	err := tx.GetContext(ctx, &row, `SELECT * FROM admission_scan_job WHERE operation_id=? AND manifest_version=?`, operationID, manifestVersion)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return row, err
}

// ClaimTx 认领可运行任务：queued 到期，或 scanning 租约已过期（崩溃重领）。
// 返回 (job, claimed)；claimed=false 表示他人持有有效租约。
func (r *AdmissionScanJobRepo) ClaimTx(ctx context.Context, tx *sqlx.Tx, operationID string, manifestVersion int64, workerID string, lease time.Duration, now time.Time) (AdmissionScanJobRow, bool, error) {
	row, err := r.GetByOperationTx(ctx, tx, operationID, manifestVersion)
	if err != nil {
		return row, false, err
	}
	nowUnix := now.Unix()
	switch row.State {
	case "queued":
		if row.NextRunAtUnix > nowUnix {
			return row, false, nil
		}
	case "scanning":
		if row.LeaseUntilUnix > nowUnix {
			return row, false, nil
		}
		if row.Attempts >= row.MaxAttempts {
			return row, false, nil
		}
	default:
		return row, false, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE admission_scan_job
 SET state='scanning', lease_generation=lease_generation+1, lease_until_unix=?, worker_id=?, attempts=attempts+1, updated_unix=?
 WHERE job_id=? AND state=? AND lease_generation=? AND attempts=?`,
		now.Add(lease).Unix(), workerID, nowUnix, row.JobID, row.State, row.LeaseGeneration, row.Attempts)
	if err != nil {
		return row, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return row, false, err
	}
	claimed, err := r.GetByOperationTx(ctx, tx, operationID, manifestVersion)
	return claimed, err == nil, err
}

// CompleteTx 在租约代次一致时完成裁决：写 job 终态；代次不匹配返回 false（迟到结果不生效）。
func (r *AdmissionScanJobRepo) CompleteTx(ctx context.Context, tx *sqlx.Tx, jobID string, generation int64, verdict, errCode string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE admission_scan_job
 SET state='completed', verdict=?, error_code=?, updated_unix=?
 WHERE job_id=? AND state='scanning' AND lease_generation=?`,
		verdict, errCode, now.Unix(), jobID, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RequeueTx 在租约代次一致时把任务退回 queued（可重试故障、未到预算）。
func (r *AdmissionScanJobRepo) RequeueTx(ctx context.Context, tx *sqlx.Tx, jobID string, generation int64, nextRunAt time.Time, errCode string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE admission_scan_job
 SET state='queued', next_run_at_unix=?, error_code=?, updated_unix=?
 WHERE job_id=? AND state='scanning' AND lease_generation=?`,
		nextRunAt.Unix(), errCode, now.Unix(), jobID, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InsertFactTx 追加一条检测事实（事实只增不改；ordinal 代次内递增）。
func (r *AdmissionScanJobRepo) InsertFactTx(ctx context.Context, tx *sqlx.Tx, fact AdmissionScanFactRow) error {
	_, err := tx.NamedExecContext(ctx, `INSERT INTO admission_scan_fact
 (job_id,ordinal,lease_generation,object_digest,coverage_required,coverage_completed,coverage_missing,integrity,verdict,findings,worker_id,observed_at_unix)
 VALUES (:job_id,:ordinal,:lease_generation,:object_digest,:coverage_required,:coverage_completed,:coverage_missing,:integrity,:verdict,:findings,:worker_id,:observed_at_unix)`, fact)
	return err
}

// FactsTx 按 job 读全部事实（供裁决预览/复核）。
func (r *AdmissionScanJobRepo) FactsTx(ctx context.Context, tx *sqlx.Tx, jobID string) ([]AdmissionScanFactRow, error) {
	var out []AdmissionScanFactRow
	if err := tx.SelectContext(ctx, &out, `SELECT * FROM admission_scan_fact WHERE job_id=? ORDER BY ordinal`, jobID); err != nil {
		return nil, err
	}
	return out, nil
}

// CountFactsTx 返回某 job 已持久事实数（供 ordinal 分配）。
func (r *AdmissionScanJobRepo) CountFactsTx(ctx context.Context, tx *sqlx.Tx, jobID string) (int64, error) {
	var n int64
	return n, tx.GetContext(ctx, &n, `SELECT COUNT(*) FROM admission_scan_fact WHERE job_id=?`, jobID)
}

// ConsumeConfirmationTx 把确认令牌登记为已用；重复消费返回 false（不重放）。
func (r *AdmissionScanJobRepo) ConsumeConfirmationTx(ctx context.Context, tx *sqlx.Tx, tokenID string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO admission_confirmation_token (token_id, used_unix) VALUES (?, ?)`, tokenID, now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
