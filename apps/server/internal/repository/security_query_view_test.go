package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func securityQueryFixture(t *testing.T) (*persistence.DB, *SecurityEventRepo, time.Time) {
	t.Helper()
	db := openSecurityEventDB(t)
	return db, NewSecurityEventRepo(db), time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
}

func TestSecurityQueryViewCoverageWindows(t *testing.T) {
	db, repo, at := securityQueryFixture(t)
	ctx := context.Background()
	read := func(from, to time.Time, coverage SecurityQueryCoverage) {
		t.Helper()
		page, err := repo.ListView(ctx, SecurityEventFilter{From: from, To: to}, nil, 0)
		if err != nil || page.Coverage != coverage || page.SnapshotWatermark != 0 || len(page.Items) != 0 {
			t.Fatalf("空页的区间 [%v, %v) 覆盖应为 %q，得到 %+v, %v", from, to, coverage, page, err)
		}
	}
	read(at, at.Add(time.Minute), SecurityQueryCoverageUnknown)
	collection, worker := NewSecurityCoverageRepo(db), "w1-worker"
	if _, err := collection.Open(ctx, worker, at); err != nil {
		t.Fatal(err)
	}
	read(at, at.Add(time.Minute), SecurityQueryCoverageUnknown)
	minute := func(i int) time.Time { return at.Add(time.Duration(i) * time.Minute) }
	one := int64(1)
	batch := SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: minute(4), CoveredThrough: minute(4), Gaps: []SecurityAuditGap{
		{Start: minute(1), End: minute(2), Reason: "queue_full", ObservedCount: &one, SuppressedCount: &one},
		{Start: minute(2), End: minute(3), Reason: "counter_overflow", Coverage: "unknown"},
	}}
	if _, err := collection.Write(ctx, worker, batch); err != nil {
		t.Fatal(err)
	}
	read(minute(-1), minute(1), SecurityQueryCoverageUnknown)
	read(minute(1), minute(5), SecurityQueryCoverageUnknown)
	read(minute(0), minute(1), SecurityQueryCoverageComplete)
	read(minute(1), minute(2), SecurityQueryCoverageGapped)
	read(minute(1), minute(3), SecurityQueryCoverageUnknown)
	read(minute(3), minute(4), SecurityQueryCoverageComplete)
	read(minute(4), minute(5), SecurityQueryCoverageUnknown)

	// 维护后若持久检查点的水位超出当前可读事件快照，不宣称已证覆盖。
	if _, err := db.Exec(`UPDATE security_audit_checkpoint SET event_watermark=1 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	read(minute(0), minute(1), SecurityQueryCoverageUnknown)
}

func TestSecurityQueryViewRetentionFence(t *testing.T) {
	db, repo, at := securityQueryFixture(t)
	ctx := context.Background()
	collection, worker := NewSecurityCoverageRepo(db), "w1-worker"
	if _, err := collection.Open(ctx, worker, at); err != nil {
		t.Fatal(err)
	}
	events := make([]SecurityEvent, 4)
	for i := range events {
		events[i] = securityEventFixture("")
		events[i].OccurredAt = at.Add(time.Duration(i+1) * time.Minute)
		writeSecurityEvent(t, db, repo, events[i], nil)
	}
	through := at.Add(5 * time.Minute)
	if _, err := collection.Write(ctx, worker, SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: through, CoveredThrough: through}); err != nil {
		t.Fatal(err)
	}
	filter := SecurityEventFilter{From: at, To: through, Limit: 2}
	first, err := repo.ListView(ctx, filter, nil, 0)
	if err != nil || first.Next == nil || first.Coverage != SecurityQueryCoverageComplete || first.SnapshotWatermark != 4 || first.Next.BeforeSequence != 3 {
		t.Fatalf("首屏应固定快照与覆盖：%+v, %v", first, err)
	}
	late := securityEventFixture("")
	late.OccurredAt = at.Add(time.Second)
	writeSecurityEvent(t, db, repo, late, nil)
	one := int64(1)
	if _, err := collection.Write(ctx, worker, SecurityAuditBatch{Number: 2, ID: NewSecurityEventID(), HeartbeatAt: through, CoveredThrough: through,
		Gaps: []SecurityAuditGap{
			{Start: at, End: at.Add(time.Minute), Reason: "queue_full", ObservedCount: &one, SuppressedCount: &one},
			{Start: at.Add(3 * time.Minute), End: at.Add(4 * time.Minute), Reason: "queue_full", ObservedCount: &one, SuppressedCount: &one},
		},
	}); err != nil {
		t.Fatal(err)
	}
	readContinued := func() {
		t.Helper()
		page, err := repo.ListView(ctx, filter, first.Next, first.RetentionGeneration)
		if err != nil || page.RetentionGeneration != first.RetentionGeneration || page.SnapshotWatermark != 4 || page.Coverage != "" || len(page.Items) != 2 || page.Items[0].EventID != events[1].EventID {
			t.Fatalf("续页不得混入晚写记录或重新评估覆盖：%+v, %v", page, err)
		}
	}
	readContinued()
	// 只删除快照中间行；分页锚点和快照水位行均应保留。
	const deleteEvent = `DELETE FROM security_event WHERE event_id=?`
	tx := securityEventTx(t, db)
	if _, err := tx.Exec(deleteEvent, events[1].EventID); err != nil {
		t.Fatal(err)
	}
	readContinued() // 未提交删除不阻塞只读请求，也不得使游标失效。
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	readContinued()
	if _, err := db.Exec(deleteEvent, events[1].EventID); err != nil {
		t.Fatal(err)
	}
	if page, err := repo.ListView(ctx, filter, first.Next, first.RetentionGeneration); !errors.Is(err, ErrSecurityEventSnapshotExpired) || len(page.Items) != 0 {
		t.Fatalf("只删除中间行也应使快照过期：%+v, %v", page, err)
	}
	for _, tc := range []struct {
		from, to time.Time
		want     SecurityQueryCoverage
	}{
		{at, events[1].OccurredAt, SecurityQueryCoverageGapped},
		{events[1].OccurredAt, through, SecurityQueryCoverageUnknown},
		{events[1].OccurredAt.Add(time.Millisecond), through, SecurityQueryCoverageGapped},
	} {
		newPage, err := repo.ListView(ctx, SecurityEventFilter{From: tc.from, To: tc.to}, nil, 0)
		if err != nil || newPage.Coverage != tc.want || newPage.RetentionGeneration <= first.RetentionGeneration {
			t.Fatalf("新快照应以半开区间保守表示已删历史：%+v, %v", newPage, err)
		}
	}
	key := *first.Next
	key.BeforeSequence = 2
	if _, err := repo.ListView(ctx, filter, &key, first.RetentionGeneration+1); !errors.Is(err, ErrSecurityEventSnapshotExpired) {
		t.Fatalf("即使代次相同，也不接受不存在的锚点：%v", err)
	}
	key = *first.Next
	key.SnapshotWatermark = 6
	if _, err := repo.ListView(ctx, filter, &key, first.RetentionGeneration+1); !errors.Is(err, ErrSecurityEventSnapshotExpired) {
		t.Fatalf("即使代次相同，也不接受不存在的水位行：%v", err)
	}
}

func TestSecurityQueryViewObjectFence(t *testing.T) {
	db, repo, _ := securityQueryFixture(t)
	ctx := context.Background()
	e, objects := securityEventAssetFixture()
	for _, path := range []string{"pkg/second.tar", "pkg/third.tar"} {
		objects = append(objects, SecurityEventObject{ObjectKind: "asset", CanonicalPath: path})
	}
	writeSecurityEvent(t, db, repo, e, objects)
	collection, worker := NewSecurityCoverageRepo(db), "w1-worker"
	from, through := e.OccurredAt.Add(-time.Second), e.OccurredAt.Add(time.Second)
	if _, err := collection.Open(ctx, worker, from); err != nil {
		t.Fatal(err)
	}
	if _, err := collection.Write(ctx, worker, SecurityAuditBatch{Number: 1, ID: NewSecurityEventID(), HeartbeatAt: through, CoveredThrough: through}); err != nil {
		t.Fatal(err)
	}
	filter := SecurityEventFilter{From: from, To: through, CanonicalPath: objects[1].CanonicalPath}
	if page, err := repo.ListView(ctx, filter, nil, 0); err != nil || page.Coverage != SecurityQueryCoverageComplete || len(page.Items) != 1 {
		t.Fatalf("删除前应找到对象匹配事件：%+v, %v", page, err)
	}
	first, err := repo.ListObjectsView(ctx, e.EventID, 1, nil)
	if err != nil || first.EventSequence < 1 || first.NextOrdinal != 1 || !first.HasMore {
		t.Fatalf("对象首屏应有有效锚点：%+v, %v", first, err)
	}
	key := &SecurityEventObjectViewKey{EventSequence: first.EventSequence, AfterOrdinal: first.NextOrdinal, RetentionGeneration: first.RetentionGeneration}
	if page, err := repo.ListObjectsView(ctx, e.EventID, 2, key); err != nil || page.HasMore || len(page.Items) != 2 || page.Items[0].Ordinal != 2 {
		t.Fatalf("对象续页应按序号读取：%+v, %v", page, err)
	}
	unknown := NewSecurityEventID()
	if _, err := repo.ListObjectsView(ctx, unknown, 1, nil); !errors.Is(err, ErrSecurityEventNotFound) {
		t.Fatalf("对象首屏父事件不存在应为未找到：%v", err)
	}
	wrongKey := *key
	wrongKey.AfterOrdinal = 4
	if _, err := repo.ListObjectsView(ctx, e.EventID, 1, &wrongKey); !errors.Is(err, ErrSecurityEventSnapshotExpired) {
		t.Fatalf("对象锚点不存在应为失效：%v", err)
	}
	wrongKey = *key
	wrongKey.EventSequence++
	if _, err := repo.ListObjectsView(ctx, e.EventID, 1, &wrongKey); !errors.Is(err, ErrSecurityEventSnapshotExpired) {
		t.Fatalf("对象父事件序号不一致应为失效：%v", err)
	}
	if _, err := db.Exec(`DELETE FROM security_event_object WHERE event_sequence=? AND ordinal=2`, first.EventSequence); err != nil {
		t.Fatal(err)
	}
	if page, err := repo.ListObjectsView(ctx, e.EventID, 1, key); !errors.Is(err, ErrSecurityEventSnapshotExpired) || len(page.Items) != 0 {
		t.Fatalf("只删除独立明细也应使快照失效：%+v, %v", page, err)
	}
	fresh, err := repo.ListObjectsView(ctx, e.EventID, 1, nil)
	if err != nil || fresh.RetentionGeneration <= first.RetentionGeneration || fresh.NextOrdinal != 1 {
		t.Fatalf("删除不改变对象可读性与稳定序号：%+v, %v", fresh, err)
	}
	if page, err := repo.ListView(ctx, filter, nil, 0); err != nil || page.Coverage != SecurityQueryCoverageUnknown || len(page.Items) != 0 {
		t.Fatalf("删掉唯一匹配明细后，零行不能被标为已证完整：%+v, %v", page, err)
	}
	tx := securityEventTx(t, db)
	if _, err := tx.Exec(`DELETE FROM security_event_object WHERE event_sequence=?`, fresh.EventSequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM security_event WHERE sequence=?`, fresh.EventSequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ListObjectsView(ctx, e.EventID, 1, nil); !errors.Is(err, ErrSecurityEventNotFound) {
		t.Fatalf("已删父事件的首屏应为未找到：%v", err)
	}
	key.RetentionGeneration = fresh.RetentionGeneration
	if _, err := repo.ListObjectsView(ctx, e.EventID, 1, key); !errors.Is(err, ErrSecurityEventSnapshotExpired) {
		t.Fatalf("已删父事件的续页应为失效：%v", err)
	}
	later := securityEventFixture("")
	later.OccurredAt = through.Add(time.Minute)
	writeSecurityEvent(t, db, repo, later, nil)
	// 新水位已高于原检查点，但后者的物理锚仍已删除；即使查询不碰删除时间也不能宣称完整。
	if page, err := repo.ListView(ctx, SecurityEventFilter{From: from, To: from.Add(500 * time.Millisecond)}, nil, 0); err != nil ||
		page.SnapshotWatermark <= first.EventSequence || page.Coverage != SecurityQueryCoverageUnknown {
		t.Fatalf("新写入不能掩盖检查点的缺失锚：%+v, %v", page, err)
	}
}

func TestSecurityQueryViewBudgetAndPlan(t *testing.T) {
	db, repo, at := securityQueryFixture(t)
	ctx := context.Background()
	var plan []struct {
		ID     int    `db:"id"`
		Parent int    `db:"parent"`
		Unused int    `db:"notused"`
		Detail string `db:"detail"`
	}
	if err := db.Select(&plan, `EXPLAIN QUERY PLAN `+securityQueryCoverageSQL, at.UnixMilli(), at.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	searches := 0
	for _, row := range plan {
		t.Log(row.Detail)
		if strings.Contains(row.Detail, "SEARCH") && strings.Contains(row.Detail, "security_audit_gap_query_overlap") {
			searches++
		}
	}
	if searches != 2 {
		t.Fatalf("两种覆盖等级都应做有界索引查找，得到 %d 次", searches)
	}

	db.SetMaxOpenConns(1)
	if _, err := repo.ListView(ctx, SecurityEventFilter{From: at, To: at.Add(time.Minute)}, nil, 0); err != nil {
		t.Fatal(err)
	}
	var timeout int
	if err := db.Get(&timeout, `PRAGMA busy_timeout`); err != nil || timeout != 30000 {
		t.Fatalf("查询连接归还后必须保留主库忙等待设置：%d, %v", timeout, err)
	}
	conn, err := db.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if page, err := repo.ListView(ctx, SecurityEventFilter{From: at, To: at.Add(time.Minute)}, nil, 0); !errors.Is(err, ErrSecurityEventUnavailable) || page.Coverage == SecurityQueryCoverageComplete {
		t.Fatalf("池耗尽时应遵循短截止时间并返回脱敏错误：%+v, %v", page, err)
	}
	if _, err := repo.ListObjectsView(ctx, NewSecurityEventID(), 1, nil); !errors.Is(err, ErrSecurityEventUnavailable) {
		t.Fatalf("取消应返回脱敏错误：%v", err)
	}
}
