package domain

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestOperationsDashboardFlushCountsOnlyCompletedProtocolOutcomes(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "dashboard-metrics.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := repository.NewOperationsObservabilityRepo(db)
	service := NewOperationsDashboardService(repo)
	now := time.Date(2026, 8, 27, 10, 0, 20, 0, time.UTC)
	service.RecordProtocol(ProtocolMetric{CompletedAt: now, Method: "GET", Status: 200, CacheResult: "hit"})
	service.RecordProtocol(ProtocolMetric{CompletedAt: now, Method: "GET", Status: 404})
	service.RecordProtocol(ProtocolMetric{CompletedAt: now, Method: "GET", Status: 502, CacheResult: "miss"})
	if err := service.Flush(now); err != nil {
		t.Fatalf("落盘协议指标：%v", err)
	}
	items, err := repo.ProtocolMinutes(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("读取协议指标：%v", err)
	}
	if len(items) != 1 || items[0].RequestCount != 3 || items[0].DownloadCount != 1 || items[0].FailureCount != 1 || items[0].CacheHitCount != 1 || items[0].CacheMissCount != 1 {
		t.Fatalf("统计口径错误：%+v", items)
	}
}
