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

// TestHostSampleFromRawDerivesUsedBytesAtSamplePoint 验证已用量在采样点
// 按 total − available 直接给值（与总量同源），缺任一端时保持空值而不是伪造零。
func TestHostSampleFromRawDerivesUsedBytesAtSamplePoint(t *testing.T) {
	memoryTotal, memoryAvailable := int64(100), int64(40)
	diskTotal, diskAvailable := int64(1000), int64(250)
	uptime := int64(120)
	raw := HostRawSample{At: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
		MemoryTotalBytes: &memoryTotal, MemoryAvailableBytes: &memoryAvailable,
		DiskTotalBytes: &diskTotal, DiskAvailableBytes: &diskAvailable,
		ProcessUptimeSeconds: &uptime}
	item := hostSampleFromRaw(raw)
	if item.MemoryUsedBytes == nil || *item.MemoryUsedBytes != 60 {
		t.Fatalf("内存已用 = %v，期望 60", item.MemoryUsedBytes)
	}
	if item.DiskUsedBytes == nil || *item.DiskUsedBytes != 750 {
		t.Fatalf("磁盘已用 = %v，期望 750", item.DiskUsedBytes)
	}
	if item.DiskTotalBytes == nil || *item.DiskTotalBytes != diskTotal {
		t.Fatalf("磁盘总量未透传：%v", item.DiskTotalBytes)
	}
	if item.ProcessUptimeSeconds == nil || *item.ProcessUptimeSeconds != uptime {
		t.Fatalf("进程运行时长未透传：%v", item.ProcessUptimeSeconds)
	}
	// 只有总量、缺可用量时，已用量必须保持空值。
	partial := hostSampleFromRaw(HostRawSample{At: raw.At, MemoryTotalBytes: &memoryTotal})
	if partial.MemoryUsedBytes != nil || partial.DiskUsedBytes != nil {
		t.Fatalf("缺可用量时已用量应为空：%+v", partial)
	}
}
