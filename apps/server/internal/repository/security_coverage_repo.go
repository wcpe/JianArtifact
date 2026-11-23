package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// SecurityCoverageRepo 原子持久化观察批次、覆盖检查点与固定词表的缺口。
type SecurityCoverageRepo struct {
	db     *persistence.DB
	events *SecurityEventRepo
}

// NewSecurityCoverageRepo 构造安全采集仓储。
func NewSecurityCoverageRepo(db *persistence.DB) *SecurityCoverageRepo {
	return &SecurityCoverageRepo{db: db, events: NewSecurityEventRepo(db)}
}

type securityAuditCheckpointRow struct {
	Worker      string `db:"worker_id"`
	LastBatch   int64  `db:"last_batch"`
	BatchDigest []byte `db:"batch_digest"`
	Heartbeat   int64  `db:"heartbeat_at"`
	CoveredFrom int64  `db:"covered_from"`
	Covered     int64  `db:"covered_through"`
	Watermark   int64  `db:"event_watermark"`
	CleanStop   bool   `db:"clean_stop"`
	Coverage    string `db:"coverage"`
	LeaseEpoch  int64  `db:"lease_epoch"`
}

// Open 建立写者检查点；旧水位之后未证明覆盖的时间不能恢复计数。
// at 为零时，在持有写事务的连接上采样服务端时间，不以进程开始或首次尝试的时间倒填。
// worker 为空时发放新一代写者标识 w<epoch>-<rand> 并接管；显式标识必须是
// w<epoch>-<rand> 形态且代次高于已发放代次才能接管，旧代一律按失效写者拒绝。
func (r *SecurityCoverageRepo) Open(ctx context.Context, worker string, at time.Time) (result SecurityAuditCheckpoint, err error) {
	if !validSecurityEventRef(worker) || (!at.IsZero() && !validSecurityEventTime(at)) {
		return result, ErrSecurityCoverageOrder
	}
	err = r.writeLowPriority(ctx, func(tx *sqlx.Tx) error {
		row, err := readSecurityAuditCheckpoint(ctx, tx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return securityAuditSQLiteError(err)
		}
		if row.Worker == worker && worker != "" {
			result, err = currentSecurityAuditCheckpoint(ctx, tx, row)
			return err
		}
		var epoch int64
		if worker == "" {
			epoch = row.LeaseEpoch + 1
			worker = mintSecurityAuditWorkerID(epoch)
		} else if epoch, err = securityAuditWorkerEpoch(worker); err != nil {
			return err
		} else if epoch <= row.LeaseEpoch {
			// 旧代写者即使墓碑行已被逐出也永远失效，不能靠逐出窗口重放为写者。
			return ErrSecurityCoverageLease
		}
		if err := activateSecurityAuditWorker(ctx, tx, worker, epoch); err != nil {
			return err
		}
		baseline := at
		if baseline.IsZero() {
			baseline = time.Now()
		}
		now := baseline.UnixMilli()
		if row.Worker == "" {
			// 只建立起点；需有后续持久水位才能证明非空覆盖区间。
			row.CoveredFrom, row.Coverage = now, "pending"
		} else {
			// 重叠或墙钟回拨时也不抹掉旧写者留下的未知尾段。
			now = max(now, row.Heartbeat, row.Covered+1)
			reason := "unclean_restart"
			if row.CleanStop {
				reason = "restart"
			}
			gap := SecurityAuditGap{Start: auditSQLTime(row.Covered), End: auditSQLTime(now), Reason: reason, Coverage: "unknown"}
			if _, err := upsertSecurityAuditGap(ctx, tx, gap); err != nil {
				return err
			}
			row.Coverage = "unknown"
		}
		row.Worker, row.LastBatch, row.BatchDigest = worker, 0, nil
		row.Heartbeat, row.Covered, row.CleanStop = now, now, false
		row.LeaseEpoch = max(row.LeaseEpoch, epoch)
		if err := writeSecurityAuditCheckpoint(ctx, tx, row); err != nil {
			return err
		}
		result, err = currentSecurityAuditCheckpoint(ctx, tx, row)
		return err
	})
	if err != nil {
		return SecurityAuditCheckpoint{}, err
	}
	return result, nil
}

// mintSecurityAuditWorkerID 发放内嵌代次的写者标识；代次由检查点单调发放，
// 写者不能自选更高代次抢占，也不能让旧代次重放。
func mintSecurityAuditWorkerID(epoch int64) string {
	return fmt.Sprintf("w%d-%s", epoch, NewSecurityEventID())
}

// securityAuditWorkerEpoch 从写者标识解析代次；非发放形态按无效输入拒绝。
func securityAuditWorkerEpoch(worker string) (int64, error) {
	rest, ok := strings.CutPrefix(worker, "w")
	if !ok {
		return 0, ErrSecurityCoverageOrder
	}
	digits, rand, ok := strings.Cut(rest, "-")
	if !ok || rand == "" {
		return 0, ErrSecurityCoverageOrder
	}
	epoch, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || epoch <= 0 || epoch == math.MaxInt64 {
		return 0, ErrSecurityCoverageOrder
	}
	return epoch, nil
}

func activateSecurityAuditWorker(ctx context.Context, tx *sqlx.Tx, worker string, epoch int64) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO security_audit_worker(worker_id,epoch) VALUES (?,?) ON CONFLICT DO NOTHING`, worker, epoch)
	if err != nil {
		return securityAuditSQLiteError(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return securityAuditSQLiteError(err)
	}
	if n != 1 {
		return ErrSecurityCoverageLease
	}
	// 租约表只保留最近 SecurityAuditLeaseRetain 代作审计环；
	// 更早代次靠检查点的单调 lease_epoch 继续拒绝，不依赖行存续。
	if _, err := tx.ExecContext(ctx, `DELETE FROM security_audit_worker WHERE epoch <= ?`, epoch-SecurityAuditLeaseRetain); err != nil {
		return securityAuditSQLiteError(err)
	}
	return nil
}

// Write 原子写入有序观察、窗口缺口和检查点；同批仅可按原内容重试。
func (r *SecurityCoverageRepo) Write(ctx context.Context, worker string, batch SecurityAuditBatch) (result SecurityAuditCheckpoint, err error) {
	digest, err := securityAuditBatchDigest(batch)
	if err != nil {
		return result, err
	}
	err = r.writeLowPriority(ctx, func(tx *sqlx.Tx) error {
		row, err := readSecurityAuditCheckpoint(ctx, tx)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSecurityCoverageLease
		}
		if err != nil {
			return securityAuditSQLiteError(err)
		}
		if row.Worker != worker {
			return ErrSecurityCoverageLease
		}
		switch {
		case batch.Number == row.LastBatch && bytes.Equal(digest, row.BatchDigest):
		case batch.Number == row.LastBatch+1 && !row.CleanStop:
			if err := r.applySecurityAuditBatch(ctx, tx, &row, batch, digest); err != nil {
				return err
			}
		default:
			return ErrSecurityCoverageOrder
		}
		result, err = currentSecurityAuditCheckpoint(ctx, tx, row)
		return err
	})
	if err != nil {
		return SecurityAuditCheckpoint{}, err
	}
	return result, nil
}

func (r *SecurityCoverageRepo) applySecurityAuditBatch(ctx context.Context, tx *sqlx.Tx, row *securityAuditCheckpointRow, batch SecurityAuditBatch, digest []byte) error {
	for _, observation := range batch.Observations {
		if err := r.events.InsertTx(ctx, tx, observation.Event, observation.objects()); err != nil {
			return err
		}
	}
	for _, gap := range batch.Gaps {
		level, err := upsertSecurityAuditGap(ctx, tx, gap)
		if err != nil {
			return err
		}
		if level == "unknown" || row.Coverage == "complete" || row.Coverage == "pending" {
			row.Coverage = level
		}
	}
	var latest int64
	if err := tx.GetContext(ctx, &latest, `SELECT COALESCE(MAX(sequence),0) FROM security_event`); err != nil {
		return securityAuditSQLiteError(err)
	}
	row.Watermark = max(row.Watermark, latest)
	row.LastBatch, row.BatchDigest, row.CleanStop = batch.Number, digest, batch.CleanStop
	row.Heartbeat = max(row.Heartbeat, batch.HeartbeatAt.UnixMilli())
	row.Covered = max(row.Covered, batch.CoveredThrough.UnixMilli())
	if row.Coverage == "pending" && row.Covered > row.CoveredFrom {
		row.Coverage = "complete"
	}
	return writeSecurityAuditCheckpoint(ctx, tx, *row)
}

func readSecurityAuditCheckpoint(ctx context.Context, tx *sqlx.Tx) (securityAuditCheckpointRow, error) {
	var row securityAuditCheckpointRow
	err := tx.GetContext(ctx, &row, `SELECT worker_id,last_batch,batch_digest,heartbeat_at,covered_from,covered_through,event_watermark,clean_stop,coverage,lease_epoch FROM security_audit_checkpoint WHERE singleton=1`)
	return row, err
}

func writeSecurityAuditCheckpoint(ctx context.Context, tx *sqlx.Tx, row securityAuditCheckpointRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO security_audit_checkpoint
		(singleton,worker_id,last_batch,batch_digest,heartbeat_at,covered_from,covered_through,event_watermark,clean_stop,coverage,lease_epoch)
		VALUES (1,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET worker_id=excluded.worker_id,
		last_batch=excluded.last_batch,batch_digest=excluded.batch_digest,heartbeat_at=excluded.heartbeat_at,
		covered_through=excluded.covered_through,event_watermark=excluded.event_watermark,
		clean_stop=excluded.clean_stop,coverage=excluded.coverage,lease_epoch=excluded.lease_epoch`, row.Worker, row.LastBatch, row.BatchDigest,
		row.Heartbeat, row.CoveredFrom, row.Covered, row.Watermark, row.CleanStop, row.Coverage, row.LeaseEpoch)
	return securityAuditSQLiteError(err)
}

func upsertSecurityAuditGap(ctx context.Context, tx *sqlx.Tx, gap SecurityAuditGap) (string, error) {
	// observed_count 始终不小于 suppressed_count，前者的加法上界也保护后者。
	// 任一已知计数无法精确累加后，将整个原因窗口保守标为未知，而不是阻塞批次。
	const lost = `(security_audit_gap.observed_count IS NULL OR excluded.observed_count IS NULL OR
		security_audit_gap.observed_count > 9223372036854775807 - excluded.observed_count)`
	var level string
	err := tx.GetContext(ctx, &level, `INSERT INTO security_audit_gap
		(window_start,window_end,reason,coverage,observed_count,suppressed_count) VALUES (?,?,?,?,?,?)
		ON CONFLICT(window_end,window_start,reason) DO UPDATE SET
		coverage=CASE WHEN `+lost+` THEN 'unknown' ELSE 'gapped' END,
		observed_count=CASE WHEN NOT `+lost+` THEN security_audit_gap.observed_count+excluded.observed_count END,
		suppressed_count=CASE WHEN NOT `+lost+` THEN security_audit_gap.suppressed_count+excluded.suppressed_count END
		RETURNING coverage`, gap.Start.UnixMilli(), gap.End.UnixMilli(), gap.Reason, securityAuditGapCoverage(gap), gap.ObservedCount, gap.SuppressedCount)
	return level, securityAuditSQLiteError(err)
}

func currentSecurityAuditCheckpoint(ctx context.Context, tx *sqlx.Tx, row securityAuditCheckpointRow) (SecurityAuditCheckpoint, error) {
	result := SecurityAuditCheckpoint{Worker: row.Worker, HeartbeatAt: auditSQLTime(row.Heartbeat), CoveredFrom: auditSQLTime(row.CoveredFrom),
		CoveredThrough: auditSQLTime(row.Covered), Watermark: row.Watermark, Coverage: row.Coverage}
	if row.Covered == row.CoveredFrom {
		result.Coverage = "unknown"
	}
	if row.Coverage == "complete" || row.Coverage == "pending" {
		return result, nil
	}
	var start, end int64
	g := &result.LastGap
	err := tx.QueryRowContext(ctx, `SELECT window_start,window_end,reason,coverage,observed_count,suppressed_count
		FROM security_audit_gap ORDER BY window_start DESC,coverage DESC,window_end DESC,reason LIMIT 1`).
		Scan(&start, &end, &g.Reason, &g.Coverage, &g.ObservedCount, &g.SuppressedCount)
	if err != nil {
		return SecurityAuditCheckpoint{}, securityAuditSQLiteError(err)
	}
	g.Start, g.End = auditSQLTime(start), auditSQLTime(end)
	return result, nil
}

func auditSQLTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
