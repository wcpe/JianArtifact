package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestSecurityCoverageDurableReplayAndFencing(t *testing.T) {
	db := openSecurityEventDB(t)
	r := NewSecurityCoverageRepo(db)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	first, second := "w1-first", "w2-second"
	initial, err := r.Open(ctx, first, start)
	if err != nil || !initial.CoveredFrom.Equal(start) || !initial.CoveredThrough.Equal(start) || initial.Coverage != "unknown" {
		t.Fatalf("首个检查点错误：%+v，%v", initial, err)
	}
	one := int64(1)
	b := SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: start.Add(time.Second), CoveredThrough: start.Add(500 * time.Millisecond),
		Observations: []SecurityAuditObservation{{Event: securityEventFixture("")}},
		Gaps:         []SecurityAuditGap{{Start: start, End: start.Add(time.Minute), Reason: "source_rate", ObservedCount: &one, SuppressedCount: &one}}}
	for range 2 {
		got, err := r.Write(ctx, first, b)
		if err != nil || got.Watermark != 1 || got.Coverage != "gapped" || got.LastGap.SuppressedCount == nil || *got.LastGap.SuppressedCount != 1 || !got.CoveredThrough.Equal(b.CoveredThrough) {
			t.Fatalf("同批回放重复或推进了未落库水位：%+v，%v", got, err)
		}
	}
	conflict := b
	conflict.ID = NewSecurityEventID()
	if _, err := r.Write(ctx, first, conflict); !errors.Is(err, ErrSecurityCoverageOrder) {
		t.Fatalf("同序号不同批次应拒绝：%v", err)
	}
	one = 2
	if _, err := r.Write(ctx, first, b); !errors.Is(err, ErrSecurityCoverageOrder) {
		t.Fatalf("同批号和标识但内容不同应拒绝：%v", err)
	}
	for range 2 {
		got, err := r.Open(ctx, second, start.Add(2*time.Minute))
		if err != nil || got.Coverage != "unknown" || got.LastGap.Reason != "unclean_restart" || got.LastGap.ObservedCount != nil || !got.LastGap.Start.Equal(b.CoveredThrough) || !got.CoveredFrom.Equal(start) || got.Watermark != 1 {
			t.Fatalf("重启没有从未覆盖水位起报告未知：%+v，%v", got, err)
		}
	}
	if _, err := r.Write(ctx, first, b); !errors.Is(err, ErrSecurityCoverageLease) {
		t.Fatalf("前一写者不能推进新租约水位：%v", err)
	}
	if _, err := r.Open(ctx, first, start); !errors.Is(err, ErrSecurityCoverageLease) {
		t.Fatalf("启动回执迟到也不能让前一写者重新夺取检查点：%v", err)
	}
	var events, gaps int
	if err := db.Get(&events, `SELECT count(*) FROM security_event`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&gaps, `SELECT count(*) FROM security_audit_gap`); err != nil {
		t.Fatal(err)
	}
	if events != 1 || gaps != 2 {
		t.Fatalf("幂等批次或启动重放产生重复：事件=%d，缺口=%d", events, gaps)
	}
}

func TestSecurityCoverageFirstBaselineExcludesExistingEvents(t *testing.T) {
	db := openSecurityEventDB(t)
	ctx, worker := context.Background(), "w1-worker"
	existing := securityEventFixture("")
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := NewSecurityEventRepo(db).InsertTx(ctx, tx, existing, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	repo := NewSecurityCoverageRepo(db)
	at := existing.OccurredAt.Add(time.Hour).Truncate(time.Millisecond)
	initial, err := repo.Open(ctx, worker, at)
	if err != nil || initial.Coverage != "unknown" || initial.Watermark != 0 || !initial.CoveredFrom.Equal(at) || !initial.CoveredThrough.Equal(at) {
		t.Fatalf("已有事件不能让首次空区间完整：%+v，%v", initial, err)
	}
	var lowerBound int64
	if err := db.Get(&lowerBound, `SELECT covered_from FROM security_audit_checkpoint`); err != nil || lowerBound != at.UnixMilli() {
		t.Fatalf("覆盖下界未持久化：%d，%v", lowerBound, err)
	}
	batch := SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: at.Add(time.Second), CoveredThrough: at.Add(time.Second)}
	got, err := repo.Write(ctx, worker, batch)
	if err != nil || got.Coverage != "complete" || got.Watermark != 1 || !got.CoveredFrom.Equal(at) ||
		!got.CoveredThrough.Equal(batch.CoveredThrough) || !existing.OccurredAt.Before(got.CoveredFrom) {
		t.Fatalf("水位可以包含历史事件，但覆盖仅从新基线起：%+v，%v", got, err)
	}
}

func TestSecurityCoverageCountOverflowStaysUnknown(t *testing.T) {
	db := openSecurityEventDB(t)
	repo, ctx, worker := NewSecurityCoverageRepo(db), context.Background(), "w1-worker"
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := repo.Open(ctx, worker, at); err != nil {
		t.Fatal(err)
	}
	full, one := int64(math.MaxInt64), int64(1)
	for i, n := range []*int64{&full, &one, &one} {
		batch := SecurityAuditBatch{Number: int64(i + 1), ID: NewSecurityEventID(), HeartbeatAt: at.Add(time.Second), CoveredThrough: at.Add(time.Second),
			Gaps: []SecurityAuditGap{{Start: at, End: at.Add(time.Minute), Reason: "queue_full", ObservedCount: n, SuppressedCount: n}}}
		for range 2 {
			got, err := repo.Write(ctx, worker, batch)
			if i == 0 {
				if err != nil || got.Coverage != "gapped" || got.LastGap.SuppressedCount == nil || *got.LastGap.SuppressedCount != full {
					t.Fatalf("上限以内应精确保存且重放不重复：%+v，%v", got, err)
				}
			} else if err != nil || got.Coverage != "unknown" || got.LastGap.Coverage != "unknown" || got.LastGap.ObservedCount != nil || got.LastGap.SuppressedCount != nil {
				t.Fatalf("计数超限必须持续未知并允许批次推进：%+v，%v", got, err)
			}
		}
	}
}

func TestSecurityCoverageLowPriorityDoesNotWaitForSQLiteWriter(t *testing.T) {
	db := openSecurityEventDB(t)
	r := NewSecurityCoverageRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lease := "w1-lease"
	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := r.Open(ctx, lease, at); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	batch := SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: at, CoveredThrough: at}
	if _, err := r.Write(ctx, lease, batch); !errors.Is(err, ErrSecurityCoverageBusy) || ctx.Err() != nil {
		t.Fatalf("低优先级写未及时让路：%v，上下文：%v", err, ctx.Err())
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write(ctx, lease, batch); err != nil {
		t.Fatalf("释放关键 SQLite 写事务后低优先级无法恢复：%v", err)
	}
	var timeout int
	if err := db.Get(&timeout, `PRAGMA busy_timeout`); err != nil || timeout != 30000 {
		t.Fatalf("低优先级写污染共享连接等待策略：%d，%v", timeout, err)
	}
}

func TestSecurityCoverageAuthenticFailureObservation(t *testing.T) {
	e := securityEventFixture("")
	e.Outcome, e.ErrorCode, e.AuthStrength = "failed", "internal_error", ""
	if err := ValidateSecurityAuditObservation(SecurityAuditObservation{Event: e}); err != nil {
		t.Fatalf("未知主体的认证服务故障必须通过异步观察校验：%v", err)
	}
	for _, outcome := range []string{"failed", "denied"} {
		e.Outcome = outcome
		e.AuthStrength = "mfa"
		if err := ValidateSecurityAuditObservation(SecurityAuditObservation{Event: e}); !errors.Is(err, ErrInvalidSecurityEvent) {
			t.Fatalf("%s 的认证尝试不得声称已验证认证强度：%v", outcome, err)
		}
	}
}
