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
