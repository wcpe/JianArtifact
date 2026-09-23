package repository

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 覆盖 FR-142 持久层口径：分钟桶内同组合累加合并、维度分行、per-制品聚合不串仓库、
// 以及 30 天保留清理（PurgeBefore）。
func TestAssetDownloadRepoAccumulatesSumsAndPurges(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset-download.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewAssetDownloadRepo(db)

	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	bucket := formatMetricTime(base)

	// 同一分钟同一组合写两次 → 主键合并累加。
	if err := repo.AddMinutes([]AssetDownloadMinute{
		{BucketStart: bucket, Repo: "r1", AssetPath: "a/b.jar", ClientIP: "1.2.3.4", UAFamily: "maven", DownloadCount: 1},
	}); err != nil {
		t.Fatalf("写入首批：%v", err)
	}
	if err := repo.AddMinutes([]AssetDownloadMinute{
		{BucketStart: bucket, Repo: "r1", AssetPath: "a/b.jar", ClientIP: "1.2.3.4", UAFamily: "maven", DownloadCount: 2},
		// 不同 IP、不同 UA 族、不同制品：各自分行。
		{BucketStart: bucket, Repo: "r1", AssetPath: "a/b.jar", ClientIP: "5.6.7.8", UAFamily: "curl", DownloadCount: 1},
		{BucketStart: bucket, Repo: "r1", AssetPath: "a/c.jar", ClientIP: "1.2.3.4", UAFamily: "maven", DownloadCount: 5},
		// 其他仓库同名制品不得混入。
		{BucketStart: bucket, Repo: "r2", AssetPath: "a/b.jar", ClientIP: "1.2.3.4", UAFamily: "maven", DownloadCount: 9},
	}); err != nil {
		t.Fatalf("写入次批：%v", err)
	}

	sum, err := repo.SumByAsset("r1")
	if err != nil {
		t.Fatalf("聚合：%v", err)
	}
	if len(sum) != 2 || sum["a/b.jar"] != 4 || sum["a/c.jar"] != 5 {
		t.Fatalf("per-制品聚合错误：%+v", sum)
	}

	// 空输入是 no-op。
	if err := repo.AddMinutes(nil); err != nil {
		t.Fatalf("空批次应无副作用：%v", err)
	}

	// 清理：cutoff 之后无数据，之前的全部删除。
	purged, err := repo.PurgeBefore(base.Add(time.Hour))
	if err != nil {
		t.Fatalf("清理：%v", err)
	}
	if purged != 4 {
		t.Fatalf("应清理 4 行，实际 %d", purged)
	}
	after, err := repo.SumByAsset("r1")
	if err != nil {
		t.Fatalf("清理后聚合：%v", err)
	}
	if len(after) != 0 {
		t.Fatalf("清理后应为空：%+v", after)
	}
}

// SumPaths：批量路径只返回命中项、跨仓库隔离、空集合为 no-op（不发查询）。
func TestAssetDownloadRepoSumPaths(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset-download-paths.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewAssetDownloadRepo(db)

	base := time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC)
	bucket := formatMetricTime(base)
	if err := repo.AddMinutes([]AssetDownloadMinute{
		{BucketStart: bucket, Repo: "r1", AssetPath: "a/x.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 3},
		{BucketStart: bucket, Repo: "r1", AssetPath: "a/y.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 7},
		{BucketStart: bucket, Repo: "r2", AssetPath: "a/x.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 9},
	}); err != nil {
		t.Fatalf("写入：%v", err)
	}

	sum, err := repo.SumPaths("r1", []string{"a/x.jar", "a/y.jar", "a/missing.jar"})
	if err != nil {
		t.Fatalf("批量聚合：%v", err)
	}
	if len(sum) != 2 || sum["a/x.jar"] != 3 || sum["a/y.jar"] != 7 {
		t.Fatalf("命中项应只有两条且数值正确（r2 不混入、missing 不出现）：%+v", sum)
	}

	empty, err := repo.SumPaths("r1", nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空集合应为空 map：%+v, %v", empty, err)
	}
}

// DownloadTrend：按粒度分桶聚合（分钟 / 小时），原始累计不去重。
func TestAssetDownloadRepoDownloadTrend(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset-download-trend.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewAssetDownloadRepo(db)

	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if err := repo.AddMinutes([]AssetDownloadMinute{
		{BucketStart: formatMetricTime(base), Repo: "r1", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 2},
		{BucketStart: formatMetricTime(base.Add(time.Minute)), Repo: "r1", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 3},
		{BucketStart: formatMetricTime(base.Add(time.Minute)), Repo: "r1", AssetPath: "b.jar", ClientIP: "2.2.2.2", UAFamily: "curl", DownloadCount: 4},
	}); err != nil {
		t.Fatalf("写入：%v", err)
	}
	window := [2]time.Time{base.Add(-time.Minute), base.Add(time.Hour)}

	minuteTrend, err := repo.DownloadTrend(window[0], window[1], "minute")
	if err != nil {
		t.Fatalf("分钟趋势：%v", err)
	}
	if len(minuteTrend) != 2 || minuteTrend[0].Count != 2 || minuteTrend[1].Count != 7 {
		t.Fatalf("分钟桶应为 [2, 7]：%+v", minuteTrend)
	}

	hourTrend, err := repo.DownloadTrend(window[0], window[1], "hour")
	if err != nil {
		t.Fatalf("小时趋势：%v", err)
	}
	if len(hourTrend) != 1 || hourTrend[0].Count != 9 {
		t.Fatalf("小时桶应为 [9]：%+v", hourTrend)
	}
}

// newAssetDownloadTestRepo 打开临时库并迁移，返回待测仓储。
func newAssetDownloadTestRepo(t *testing.T, name string) *AssetDownloadRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	return NewAssetDownloadRepo(db)
}

// seedGroupedDownloads 写入跨分钟 / 跨 IP / 跨 UA 族 / 跨仓库的样本：
// base 分钟 r1(r2) maven@1.1.1.1、base+1 分钟 r1 curl@2.2.2.2、base+1 小时 r1 maven@3.3.3.3。
func seedGroupedDownloads(t *testing.T, repo *AssetDownloadRepo, base time.Time) {
	t.Helper()
	if err := repo.AddMinutes([]AssetDownloadMinute{
		{BucketStart: formatMetricTime(base), Repo: "r1", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 2},
		{BucketStart: formatMetricTime(base), Repo: "r1", AssetPath: "b.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 5},
		{BucketStart: formatMetricTime(base), Repo: "r2", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 9},
		{BucketStart: formatMetricTime(base.Add(time.Minute)), Repo: "r1", AssetPath: "a.jar", ClientIP: "2.2.2.2", UAFamily: "curl", DownloadCount: 4},
		{BucketStart: formatMetricTime(base.Add(time.Hour)), Repo: "r1", AssetPath: "a.jar", ClientIP: "3.3.3.3", UAFamily: "maven", DownloadCount: 6},
	}); err != nil {
		t.Fatalf("写入样本：%v", err)
	}
}

// DownloadTrendGrouped：按 ip / family 分组、可选 repo 过滤、分桶对齐、空区间与非法分组维度。
func TestAssetDownloadRepoDownloadTrendGrouped(t *testing.T) {
	repo := newAssetDownloadTestRepo(t, "asset-download-grouped.db")
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	seedGroupedDownloads(t, repo, base)
	from, to := base.Add(-time.Minute), base.Add(2*time.Hour)

	// 按 IP 分组（全局、分钟桶）：(bucket, group) 升序。
	ipRows, err := repo.DownloadTrendGrouped(from, to, "minute", DownloadGroupKeyIP, "")
	if err != nil {
		t.Fatalf("按 IP 分组：%v", err)
	}
	wantIP := []DownloadGroupedBucket{
		{Bucket: base.Format(time.RFC3339), Group: "1.1.1.1", Count: 16},
		{Bucket: base.Add(time.Minute).Format(time.RFC3339), Group: "2.2.2.2", Count: 4},
		{Bucket: base.Add(time.Hour).Format(time.RFC3339), Group: "3.3.3.3", Count: 6},
	}
	if len(ipRows) != len(wantIP) {
		t.Fatalf("按 IP 分组应 %d 行，得 %d：%+v", len(wantIP), len(ipRows), ipRows)
	}
	for i := range wantIP {
		if ipRows[i] != wantIP[i] {
			t.Fatalf("第 %d 行应 %+v，得 %+v", i, wantIP[i], ipRows[i])
		}
	}

	// 按 UA 族分组 + repo 过滤（r2 不得混入）。
	familyRows, err := repo.DownloadTrendGrouped(from, to, "minute", DownloadGroupKeyFamily, "r1")
	if err != nil {
		t.Fatalf("按族分组：%v", err)
	}
	if len(familyRows) != 3 {
		t.Fatalf("r1 按族应 3 行（三个分钟桶），得 %d：%+v", len(familyRows), familyRows)
	}
	if familyRows[0].Bucket != base.Format(time.RFC3339) || familyRows[0].Group != "maven" || familyRows[0].Count != 7 {
		t.Fatalf("首个族桶应 (10:00, maven, 7)，得 %+v", familyRows[0])
	}
	if familyRows[1].Group != "curl" || familyRows[1].Count != 4 {
		t.Fatalf("第二个族桶应 (10:01, curl, 4)，得 %+v", familyRows[1])
	}
	if familyRows[2].Bucket != base.Add(time.Hour).Format(time.RFC3339) || familyRows[2].Group != "maven" || familyRows[2].Count != 6 {
		t.Fatalf("第三个族桶应 (11:00, maven, 6)，得 %+v", familyRows[2])
	}

	// 小时桶对齐：10:00 与 10:01 合并进 10:00 桶；11:00 独立成桶。
	hourRows, err := repo.DownloadTrendGrouped(from, to, "hour", DownloadGroupKeyIP, "")
	if err != nil {
		t.Fatalf("小时桶分组：%v", err)
	}
	if len(hourRows) != 3 {
		t.Fatalf("小时桶应 3 行（10:00 两个 IP + 11:00 一个 IP），得 %d：%+v", len(hourRows), hourRows)
	}
	if hourRows[0].Bucket != base.Format(time.RFC3339) || hourRows[0].Count != 16 ||
		hourRows[1].Bucket != base.Format(time.RFC3339) || hourRows[1].Count != 4 ||
		hourRows[2].Bucket != base.Add(time.Hour).Format(time.RFC3339) || hourRows[2].Count != 6 {
		t.Fatalf("小时桶分组值错误：%+v", hourRows)
	}

	// 空区间返回空切片（非 nil），不报错。
	empty, err := repo.DownloadTrendGrouped(base.Add(3*time.Hour), base.Add(4*time.Hour), "minute", DownloadGroupKeyIP, "")
	if err != nil {
		t.Fatalf("空区间不应报错：%v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空区间应返回空切片：%+v", empty)
	}

	// 不存在的仓库过滤 → 空。
	otherRepo, err := repo.DownloadTrendGrouped(from, to, "minute", DownloadGroupKeyIP, "missing")
	if err != nil || len(otherRepo) != 0 {
		t.Fatalf("未知仓库应返回空：%+v, %v", otherRepo, err)
	}

	// 非法分组维度直接报错（白名单外的列名不得下推 SQL）。
	if _, err := repo.DownloadTrendGrouped(from, to, "minute", DownloadGroupKey("asset_path"), ""); err == nil {
		t.Fatal("非法 groupBy 应报错")
	}
}

// DownloadTrendForRepo / SumByRepo：按仓隔离、分桶对齐、空区间与总下载存读。
func TestAssetDownloadRepoDownloadTrendForRepoAndSumByRepo(t *testing.T) {
	repo := newAssetDownloadTestRepo(t, "asset-download-repo-trend.db")
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	seedGroupedDownloads(t, repo, base)
	from, to := base.Add(-time.Minute), base.Add(2*time.Hour)

	// r1 分钟趋势：10:00=7、10:01=4、11:00=6。
	minuteTrend, err := repo.DownloadTrendForRepo("r1", from, to, "minute")
	if err != nil {
		t.Fatalf("r1 分钟趋势：%v", err)
	}
	wantMinute := []DownloadTrendBucket{
		{Bucket: base.Format(time.RFC3339), Count: 7},
		{Bucket: base.Add(time.Minute).Format(time.RFC3339), Count: 4},
		{Bucket: base.Add(time.Hour).Format(time.RFC3339), Count: 6},
	}
	if len(minuteTrend) != len(wantMinute) {
		t.Fatalf("r1 分钟趋势应 %d 桶，得 %d：%+v", len(wantMinute), len(minuteTrend), minuteTrend)
	}
	for i := range wantMinute {
		if minuteTrend[i] != wantMinute[i] {
			t.Fatalf("第 %d 桶应 %+v，得 %+v", i, wantMinute[i], minuteTrend[i])
		}
	}

	// 小时桶对齐：10:00 与 10:01 合并为 10:00 桶（7+4=11），11:00 独立。
	hourTrend, err := repo.DownloadTrendForRepo("r1", from, to, "hour")
	if err != nil {
		t.Fatalf("r1 小时趋势：%v", err)
	}
	if len(hourTrend) != 2 || hourTrend[0].Count != 11 || hourTrend[1].Count != 6 {
		t.Fatalf("r1 小时桶应 [11, 6]：%+v", hourTrend)
	}

	// r2 隔离：只有自己的 9。
	r2Trend, err := repo.DownloadTrendForRepo("r2", from, to, "minute")
	if err != nil {
		t.Fatalf("r2 趋势：%v", err)
	}
	if len(r2Trend) != 1 || r2Trend[0].Count != 9 {
		t.Fatalf("r2 应单桶 9：%+v", r2Trend)
	}

	// 空区间返回空切片。
	emptyTrend, err := repo.DownloadTrendForRepo("r1", base.Add(3*time.Hour), base.Add(4*time.Hour), "minute")
	if err != nil || emptyTrend == nil || len(emptyTrend) != 0 {
		t.Fatalf("空区间应返回空切片：%+v, %v", emptyTrend, err)
	}

	// 总下载：r1 = 7+4+6 = 17，r2 = 9，未知仓库 = 0。
	if total, err := repo.SumByRepo("r1"); err != nil || total != 17 {
		t.Fatalf("r1 总下载应 17，得 %d（%v）", total, err)
	}
	if total, err := repo.SumByRepo("r2"); err != nil || total != 9 {
		t.Fatalf("r2 总下载应 9，得 %d（%v）", total, err)
	}
	if total, err := repo.SumByRepo("missing"); err != nil || total != 0 {
		t.Fatalf("未知仓库总下载应 0，得 %d（%v）", total, err)
	}
}

// 下载趋势遵循 [from,to)：边界上的分钟桶 to 不得进入分组、仓库、全局或客户端排行结果。
func TestAssetDownloadRepoDownloadRangesExcludeUpperBoundary(t *testing.T) {
	repo := newAssetDownloadTestRepo(t, "asset-download-half-open.db")
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	seedGroupedDownloads(t, repo, base)
	to := base.Add(time.Hour)

	grouped, err := repo.DownloadTrendGrouped(base, to, "hour", DownloadGroupKeyIP, "")
	if err != nil {
		t.Fatalf("分组趋势：%v", err)
	}
	if len(grouped) != 2 || grouped[0].Bucket != base.Format(time.RFC3339) || grouped[1].Bucket != base.Format(time.RFC3339) {
		t.Fatalf("to 边界的 11:00 桶不得进入分组趋势：%+v", grouped)
	}

	repoTrend, err := repo.DownloadTrendForRepo("r1", base, to, "hour")
	if err != nil || len(repoTrend) != 1 || repoTrend[0].Bucket != base.Format(time.RFC3339) || repoTrend[0].Count != 11 {
		t.Fatalf("仓库趋势仅应含 [10:00,11:00) 的 11 次下载，得 %+v（%v）", repoTrend, err)
	}

	globalTrend, err := repo.DownloadTrend(base, to, "hour")
	if err != nil || len(globalTrend) != 1 || globalTrend[0].Bucket != base.Format(time.RFC3339) || globalTrend[0].Count != 20 {
		t.Fatalf("全局趋势仅应含 [10:00,11:00) 的 20 次下载，得 %+v（%v）", globalTrend, err)
	}

	ips, _, err := repo.DownloadClientRanking(base, to, 10)
	if err != nil {
		t.Fatalf("客户端排行：%v", err)
	}
	for _, item := range ips {
		if item.IP == "3.3.3.3" {
			t.Fatalf("to 边界的 11:00 来源不得进入排行：%+v", ips)
		}
	}
}

// DownloadClientRanking：独立来源口径——同一小时同 IP 同制品多分钟只计一次贡献。
func TestAssetDownloadRepoDownloadClientRanking(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset-download-ranking.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewAssetDownloadRepo(db)

	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if err := repo.AddMinutes([]AssetDownloadMinute{
		// 同一小时内 1.1.1.1 对 a.jar 的两次（不同分钟）→ 去重后算一次贡献。
		{BucketStart: formatMetricTime(base), Repo: "r1", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 5},
		{BucketStart: formatMetricTime(base.Add(time.Minute)), Repo: "r1", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 9},
		// 同 IP 同小时另有 b.jar → 第二次贡献。
		{BucketStart: formatMetricTime(base.Add(2 * time.Minute)), Repo: "r1", AssetPath: "b.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 1},
		// 另一小时的 2.2.2.2 → 独立贡献一次。
		{BucketStart: formatMetricTime(base.Add(time.Hour)), Repo: "r1", AssetPath: "a.jar", ClientIP: "2.2.2.2", UAFamily: "curl", DownloadCount: 1},
	}); err != nil {
		t.Fatalf("写入：%v", err)
	}

	ips, families, err := repo.DownloadClientRanking(base.Add(-time.Minute), base.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("来源排名：%v", err)
	}
	if len(ips) != 2 || ips[0].IP != "1.1.1.1" || ips[0].Count != 2 || ips[1].IP != "2.2.2.2" || ips[1].Count != 1 {
		t.Fatalf("IP 排名应去重且按贡献降序：%+v", ips)
	}
	if len(families) != 2 || families[0].Family != "maven" || families[0].Count != 2 || families[1].Family != "curl" || families[1].Count != 1 {
		t.Fatalf("族分布应同口径去重：%+v", families)
	}
}
