package domain

import (
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 FR-159 的保留策略（规格 §2.2 与 §5.1）：
//   - 上游健康历史保留 30 天，由 storage-cleanup 作业**顺带清理**（不新增作业）；
//   - 清理只针对 upstream_health_minute 一张表，不误删 host_metric_minute 等其它表。
//
// 这里走完整的 StorageCleanupService.Run（而不是单独调 PruneUpstreamHealth），
// 因为「顺带」这件事本身就是需求：作业装配漏掉这一步时，单表用例仍然会全绿。

// 30 天前/后的采样：只清超期那一行，未超期保留，且 host_metric_minute 一行不少。
func TestStorageCleanupPrunesUpstreamHealthOnly(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{})
	healthRepo := repository.NewUpstreamHealthRepo(fixture.db)
	fixture.clean.SetUpstreamHealthRepo(healthRepo)

	repoID, err := fixture.repos.Create("cleanup-proxy", "raw", "proxy", "private", "")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	expired := repository.UpstreamHealthBucketStart(fixture.now.Add(-31 * 24 * time.Hour))
	fresh := repository.UpstreamHealthBucketStart(fixture.now.Add(-29 * 24 * time.Hour))
	for _, bucket := range []string{expired, fresh} {
		if err := healthRepo.AddSamples(repository.UpstreamHealthDelta{
			BucketStart: bucket, RepositoryID: repoID, Source: repository.UpstreamHealthSourceRequest,
			Samples: 1, OK: 1, LatencySumMs: 10, LatencyCountMs: 1, MaxLatencyMs: 10,
		}); err != nil {
			t.Fatalf("写入采样：%v", err)
		}
	}
	// 另造一行同样超期的 host_metric_minute：清理不得波及它。
	if _, err := fixture.db.Exec(`INSERT INTO host_metric_minute (bucket_start, host_state, network_state, process_state, readiness_state)
		VALUES (?, 'ok', 'ok', 'ok', 'ok')`, expired); err != nil {
		t.Fatalf("插入主机样本：%v", err)
	}

	result, err := fixture.clean.Run()
	if err != nil {
		t.Fatalf("执行清理：%v", err)
	}
	if result.UpstreamHealth != 1 {
		t.Fatalf("应清理 1 行超期上游健康采样，实际 %d", result.UpstreamHealth)
	}
	// 未超期的那一行必须还在。
	items, err := healthRepo.UpstreamHealthBuckets(repoID, fixture.now.Add(-40*24*time.Hour), fixture.now, "")
	if err != nil {
		t.Fatalf("清理后读取采样：%v", err)
	}
	if len(items) != 1 || items[0].BucketStart != fresh {
		t.Fatalf("未超期的采样必须保留：%+v", items)
	}
	// 其它分钟表不得被误删。
	var hostRows int
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM host_metric_minute`).Scan(&hostRows); err != nil {
		t.Fatalf("统计主机样本：%v", err)
	}
	if hostRows != 1 {
		t.Fatalf("清理不得误删 host_metric_minute，现存 %d 行", hostRows)
	}
	// 未接线清理入口时该步骤是空操作（不 panic、不报错）。
	unwired := NewStorageCleanupService(fixture.meta, fixture.svc, fixture.blobs, StorageCleanupOptions{})
	unwired.now = func() time.Time { return fixture.now }
	plain, err := unwired.Run()
	if err != nil {
		t.Fatalf("未接线上游健康清理时应无错误：%v", err)
	}
	if plain.UpstreamHealth != 0 {
		t.Fatalf("未接线时不应清理任何行，实际 %d", plain.UpstreamHealth)
	}
}
