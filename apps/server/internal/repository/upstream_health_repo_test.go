package repository

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件覆盖规格 §5.1 中属于持久化层的验收项：分钟桶累加、跨分钟分行、
// 两源不混、延迟口径（无成功样本为 NULL）、按仓库隔离、30 天保留清理且不误删其它表。

// openUpstreamHealthTestDB 打开一个已迁移的临时库，测试结束自动关闭。
func openUpstreamHealthTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "upstream-health.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	return db
}

// insertUpstreamHealthRepo 造一个仓库行，返回其 ID（本表有外键，必须先有仓库）。
func insertUpstreamHealthRepo(t *testing.T, db *persistence.DB, name string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO repository (name, format, type, visibility, description, config, online, created_at)
		VALUES (?, 'raw', 'proxy', 'private', '', '{}', 1, '2026-08-27 10:00:00')`, name)
	if err != nil {
		t.Fatalf("插入仓库 %s：%v", name, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 ID：%v", err)
	}
	return id
}

// okDelta 返回一条成功采样的增量（默认落在同一分钟内）。
func okDelta(bucket string, repoID int64, source string, latencyMs int64) UpstreamHealthDelta {
	return UpstreamHealthDelta{
		BucketStart: bucket, RepositoryID: repoID, Source: source,
		Samples: 1, OK: 1,
		LatencySumMs: latencyMs, LatencyCountMs: 1, MaxLatencyMs: latencyMs,
	}
}

// failureDelta 返回一条指定失败类别的增量。
func failureDelta(bucket string, repoID int64, source, failure string) UpstreamHealthDelta {
	delta := UpstreamHealthDelta{BucketStart: bucket, RepositoryID: repoID, Source: source, Samples: 1}
	switch failure {
	case UpstreamHealthFailureTimeout:
		delta.Timeout = 1
	case UpstreamHealthFailureUnreachable:
		delta.Unreachable = 1
	case UpstreamHealthFailureSlow:
		delta.Slow = 1
	case UpstreamHealthFailureAborted:
		delta.Aborted = 1
	}
	return delta
}

// 同一分钟多次同源采样累加为一行（samples / ok / 各失败列 / 平均与最大延迟都累加），跨分钟分行。
func TestUpstreamHealthAddSamplesAccumulatesWithinMinuteAndSplitsAcrossMinutes(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	minute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC))
	nextMinute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 1, 0, 0, time.UTC))

	// 同一分钟：两次成功（100ms / 300ms）+ 一次超时 + 一次不可达。
	for _, delta := range []UpstreamHealthDelta{
		okDelta(minute, repoID, UpstreamHealthSourceRequest, 100),
		okDelta(minute, repoID, UpstreamHealthSourceRequest, 300),
		failureDelta(minute, repoID, UpstreamHealthSourceRequest, UpstreamHealthFailureTimeout),
		failureDelta(minute, repoID, UpstreamHealthSourceRequest, UpstreamHealthFailureUnreachable),
	} {
		if err := repo.AddSamples(delta); err != nil {
			t.Fatalf("累加采样：%v", err)
		}
	}
	// 下一分钟：一次成功。
	if err := repo.AddSamples(okDelta(nextMinute, repoID, UpstreamHealthSourceRequest, 50)); err != nil {
		t.Fatalf("累加跨分钟采样：%v", err)
	}

	items, err := repo.UpstreamHealthBuckets(repoID,
		time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC), time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("读取时间序列：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("跨分钟应分两行，得 %d 行：%+v", len(items), items)
	}
	first := items[0]
	if first.BucketStart != minute {
		t.Fatalf("第一行桶起点：期望 %s，得 %s", minute, first.BucketStart)
	}
	if first.Samples != 4 || first.OK != 2 || first.Timeout != 1 || first.Unreachable != 1 || first.Slow != 0 || first.Aborted != 0 {
		t.Fatalf("同分钟累加错误：%+v", first)
	}
	// 平均延迟只统计成功样本：(100+300)/2 = 200；最大值 300。
	if first.AvgLatencyMs == nil || *first.AvgLatencyMs != 200 {
		t.Fatalf("平均延迟：期望 200，得 %v", first.AvgLatencyMs)
	}
	if first.MaxLatencyMs == nil || *first.MaxLatencyMs != 300 {
		t.Fatalf("最大延迟：期望 300，得 %v", first.MaxLatencyMs)
	}
	second := items[1]
	if second.BucketStart != nextMinute || second.Samples != 1 || second.OK != 1 {
		t.Fatalf("跨分钟分行错误：%+v", second)
	}
}

// 同仓库同分钟的 request 与 probe 各占一行，互不覆盖——这是「两源不混」的核心断言。
func TestUpstreamHealthKeepsRequestAndProbeSeparate(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	minute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC))

	// 真实请求 2 次成功 + 1 次慢；探测 1 次成功。
	_ = repo.AddSamples(okDelta(minute, repoID, UpstreamHealthSourceRequest, 100))
	_ = repo.AddSamples(okDelta(minute, repoID, UpstreamHealthSourceRequest, 100))
	_ = repo.AddSamples(failureDelta(minute, repoID, UpstreamHealthSourceRequest, UpstreamHealthFailureSlow))
	_ = repo.AddSamples(okDelta(minute, repoID, UpstreamHealthSourceProbe, 20))

	all, err := repo.UpstreamHealthBuckets(repoID, time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("读取两源序列：%v", err)
	}
	if len(all) != 2 {
		t.Fatalf("两源应各占一行，得 %d 行：%+v", len(all), all)
	}
	bySource := map[string]UpstreamHealthBucket{}
	for _, item := range all {
		bySource[item.Source] = item
	}
	request := bySource[UpstreamHealthSourceRequest]
	if request.Samples != 3 || request.OK != 2 || request.Slow != 1 {
		t.Fatalf("request 序列被覆盖或串列：%+v", request)
	}
	probe := bySource[UpstreamHealthSourceProbe]
	if probe.Samples != 1 || probe.OK != 1 || probe.Slow != 0 {
		t.Fatalf("probe 序列被覆盖或串列：%+v", probe)
	}

	// 按来源过滤：只取 probe 时不应看到 request 的行。
	onlyProbe, err := repo.UpstreamHealthBuckets(repoID, time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC), UpstreamHealthSourceProbe)
	if err != nil {
		t.Fatalf("按来源过滤：%v", err)
	}
	if len(onlyProbe) != 1 || onlyProbe[0].Source != UpstreamHealthSourceProbe {
		t.Fatalf("按来源过滤结果错误：%+v", onlyProbe)
	}
}

// 无成功样本时 avg_latency_ms / max_latency_ms 为 NULL（不是 0）。
func TestUpstreamHealthLatencyIsNullWithoutSuccessfulSamples(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	minute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC))

	_ = repo.AddSamples(failureDelta(minute, repoID, UpstreamHealthSourceRequest, UpstreamHealthFailureUnreachable))
	_ = repo.AddSamples(failureDelta(minute, repoID, UpstreamHealthSourceRequest, UpstreamHealthFailureAborted))

	items, err := repo.UpstreamHealthBuckets(repoID, time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("读取序列：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应有一行，得 %d 行", len(items))
	}
	if items[0].AvgLatencyMs != nil {
		t.Fatalf("无成功样本时平均延迟必须为 NULL，得 %d", *items[0].AvgLatencyMs)
	}
	if items[0].MaxLatencyMs != nil {
		t.Fatalf("无成功样本时最大延迟必须为 NULL，得 %d", *items[0].MaxLatencyMs)
	}

	// 之后补一次成功样本：NULL 必须被正确衔接为「本次延迟」，而不是被当作 0 参与平均。
	_ = repo.AddSamples(okDelta(minute, repoID, UpstreamHealthSourceRequest, 400))
	items, err = repo.UpstreamHealthBuckets(repoID, time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("补成功样本后读取序列：%v", err)
	}
	if items[0].AvgLatencyMs == nil || *items[0].AvgLatencyMs != 400 {
		t.Fatalf("首个成功样本的平均延迟应为 400，得 %v", items[0].AvgLatencyMs)
	}
	if items[0].Samples != 3 || items[0].OK != 1 || items[0].Failed() != 2 {
		t.Fatalf("补成功样本后累计错误：%+v", items[0])
	}
}

// 按仓库隔离：A 仓库的采样不出现在 B 仓库的查询结果里。
func TestUpstreamHealthIsolatesRepositories(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoA := insertUpstreamHealthRepo(t, db, "proxy-a")
	repoB := insertUpstreamHealthRepo(t, db, "proxy-b")
	minute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC))
	from := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC)

	_ = repo.AddSamples(okDelta(minute, repoA, UpstreamHealthSourceRequest, 100))
	_ = repo.AddSamples(okDelta(minute, repoA, UpstreamHealthSourceRequest, 100))
	_ = repo.AddSamples(okDelta(minute, repoB, UpstreamHealthSourceRequest, 900))

	itemsA, err := repo.UpstreamHealthBuckets(repoA, from, to, "")
	if err != nil {
		t.Fatalf("读取 A 仓库：%v", err)
	}
	itemsB, err := repo.UpstreamHealthBuckets(repoB, from, to, "")
	if err != nil {
		t.Fatalf("读取 B 仓库：%v", err)
	}
	if len(itemsA) != 1 || itemsA[0].Samples != 2 {
		t.Fatalf("A 仓库序列错误：%+v", itemsA)
	}
	if len(itemsB) != 1 || itemsB[0].Samples != 1 {
		t.Fatalf("B 仓库序列错误：%+v", itemsB)
	}
	if itemsA[0].RepositoryID != repoA || itemsB[0].RepositoryID != repoB {
		t.Fatalf("仓库归属串号：A=%d B=%d", itemsA[0].RepositoryID, itemsB[0].RepositoryID)
	}
}

// 保留清理：只删超过 30 天的行，未超期保留；且绝不误删 host_metric_minute。
func TestPruneUpstreamHealthOnlyRemovesExpiredRowsAndLeavesOtherTables(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	expired := UpstreamHealthBucketStart(now.Add(-31 * 24 * time.Hour))
	fresh := UpstreamHealthBucketStart(now.Add(-29 * 24 * time.Hour))
	_ = repo.AddSamples(okDelta(expired, repoID, UpstreamHealthSourceRequest, 100))
	_ = repo.AddSamples(okDelta(fresh, repoID, UpstreamHealthSourceRequest, 100))

	// 另造一行 host_metric_minute（同样超期），用来验证清理只针对本表。
	if _, err := db.Exec(`INSERT INTO host_metric_minute (bucket_start, host_state, network_state, process_state, readiness_state)
		VALUES (?, 'ok', 'ok', 'ok', 'ok')`, expired); err != nil {
		t.Fatalf("插入主机样本：%v", err)
	}

	removed, err := repo.PruneUpstreamHealth(UpstreamHealthBucketStart(now.Add(-UpstreamHealthRetention)))
	if err != nil {
		t.Fatalf("清理过期行：%v", err)
	}
	if removed != 1 {
		t.Fatalf("应清理 1 行过期采样，得 %d", removed)
	}
	items, err := repo.UpstreamHealthBuckets(repoID, now.Add(-40*24*time.Hour), now, "")
	if err != nil {
		t.Fatalf("清理后读取序列：%v", err)
	}
	if len(items) != 1 || items[0].BucketStart != fresh {
		t.Fatalf("未超期的行必须保留：%+v", items)
	}
	if got := countRows(t, db, "host_metric_minute"); got != 1 {
		t.Fatalf("清理不得误删 host_metric_minute，现存 %d 行", got)
	}
}

// 增量分类不自洽（分类合计 ≠ samples）时拒绝落库，避免连通率分子分母互相矛盾。
func TestUpstreamHealthAddSamplesRejectsInconsistentDelta(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	minute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC))
	if err := repo.AddSamples(UpstreamHealthDelta{
		BucketStart: minute, RepositoryID: repoID, Source: UpstreamHealthSourceRequest, Samples: 3, OK: 1,
	}); err == nil {
		t.Fatalf("分类合计与样本数不符时应报错")
	}
	if got := countRows(t, db, "upstream_health_minute"); got != 0 {
		t.Fatalf("坏数据不得落库，现存 %d 行", got)
	}
}

// 仓库删除时本表采样随外键级联清理，不留孤儿行。
func TestUpstreamHealthCascadesOnRepositoryDelete(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	minute := UpstreamHealthBucketStart(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC))
	_ = repo.AddSamples(okDelta(minute, repoID, UpstreamHealthSourceRequest, 100))
	if got := countRows(t, db, "upstream_health_minute"); got != 1 {
		t.Fatalf("采样未写入，现存 %d 行", got)
	}
	if _, err := db.Exec(`DELETE FROM repository WHERE id = ?`, repoID); err != nil {
		t.Fatalf("删除仓库：%v", err)
	}
	if got := countRows(t, db, "upstream_health_minute"); got != 0 {
		t.Fatalf("仓库删除后采样应级联清理，现存 %d 行", got)
	}
}

// UpstreamHealthRecent 按时间倒序返回最近 N 个桶，两路来源都含。
func TestUpstreamHealthRecentReturnsNewestBuckets(t *testing.T) {
	db := openUpstreamHealthTestDB(t)
	repo := NewUpstreamHealthRepo(db)
	repoID := insertUpstreamHealthRepo(t, db, "proxy-a")
	base := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		bucket := UpstreamHealthBucketStart(base.Add(time.Duration(i) * time.Minute))
		_ = repo.AddSamples(okDelta(bucket, repoID, UpstreamHealthSourceRequest, int64(10*(i+1))))
	}
	items, err := repo.UpstreamHealthRecent(repoID, 2)
	if err != nil {
		t.Fatalf("读取最近明细：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应返回 2 个桶，得 %d", len(items))
	}
	if items[0].BucketStart != UpstreamHealthBucketStart(base.Add(2*time.Minute)) {
		t.Fatalf("首行应是最新桶，得 %s", items[0].BucketStart)
	}
	if items[1].BucketStart != UpstreamHealthBucketStart(base.Add(time.Minute)) {
		t.Fatalf("次行应是次新桶，得 %s", items[1].BucketStart)
	}
}
