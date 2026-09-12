package repository

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

func TestOperationsObservabilityRepoAggregatesAndRetainsCurrentNodeMetrics(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "operations-observability.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewOperationsObservabilityRepo(db)
	base := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	first := ProtocolMinute{BucketStart: formatMetricTime(base), RequestCount: 3, DownloadCount: 2, FailureCount: 1, CacheHitCount: 1, CacheMissCount: 1}
	second := ProtocolMinute{BucketStart: formatMetricTime(base), RequestCount: 4, DownloadCount: 3, CacheHitCount: 2}
	if err := repo.AddProtocolMinute(first); err != nil {
		t.Fatalf("写入第一批协议计数：%v", err)
	}
	if err := repo.AddProtocolMinute(second); err != nil {
		t.Fatalf("累加同一分钟协议计数：%v", err)
	}
	items, err := repo.ProtocolMinutes(base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("读取协议计数：%v", err)
	}
	if len(items) != 1 || items[0].RequestCount != 7 || items[0].DownloadCount != 5 || items[0].FailureCount != 1 || items[0].CacheHitCount != 3 || items[0].CacheMissCount != 1 {
		t.Fatalf("分钟聚合错误：%+v", items)
	}
	if err := repo.PutHostSample(HostMetricSample{BucketStart: formatMetricTime(base), HostState: MetricStateOK, NetworkState: MetricStateUnavailable, NetworkErrorCode: "network_unavailable", ProcessState: MetricStateOK, ReadinessState: MetricStateOK}); err != nil {
		t.Fatalf("写入主机样本：%v", err)
	}
	latest, err := repo.LatestHostSample()
	if err != nil {
		t.Fatalf("读取主机样本：%v", err)
	}
	if latest.NetworkState != MetricStateUnavailable || latest.NetworkErrorCode != "network_unavailable" {
		t.Fatalf("主机状态未持久化：%+v", latest)
	}
	if err := repo.DeleteBefore(base.Add(time.Second)); err != nil {
		t.Fatalf("清理过期数据：%v", err)
	}
	items, err = repo.ProtocolMinutes(base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("读取清理后协议计数：%v", err)
	}
	if len(items) != 0 {
		t.Fatalf("过期协议计数未清理：%+v", items)
	}
}

// TestOperationsObservabilityCurrentCapacityCountsDistinctRepositories 回归：
// CurrentCapacity 的 LEFT JOIN 资产后 COUNT(repository.id) 会按资产行放大仓库数
// （t1 测试站实机：3 仓库被计成 7），必须按 DISTINCT 仓库去重。
func TestOperationsObservabilityCurrentCapacityCountsDistinctRepositories(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "operations-capacity.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewOperationsObservabilityRepo(db)
	repoRepo := NewRepoRepo(db)
	assetRepo := NewAssetRepo(db)

	if _, err := repoRepo.Create("cap-rich", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建含资产仓库：%v", err)
	}
	rich, err := repoRepo.GetByName("cap-rich")
	if err != nil {
		t.Fatalf("读仓库：%v", err)
	}
	for i, p := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := assetRepo.Upsert(rich.ID, p, fmt.Sprintf("hash-%d", i), 10, "application/octet-stream", "", ""); err != nil {
			t.Fatalf("写资产 %s：%v", p, err)
		}
	}
	if _, err := repoRepo.Create("cap-empty", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建空仓库：%v", err)
	}

	snap, err := repo.CurrentCapacity()
	if err != nil {
		t.Fatalf("读取容量快照：%v", err)
	}
	if snap.RepositoryCount != 2 {
		t.Fatalf("仓库数=%d，期望 2（不得按资产行放大）", snap.RepositoryCount)
	}
	if snap.AssetCount != 3 {
		t.Fatalf("资产数=%d，期望 3", snap.AssetCount)
	}
}
