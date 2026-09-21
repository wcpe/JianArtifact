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
