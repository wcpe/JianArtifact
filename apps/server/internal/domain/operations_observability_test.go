package domain

import (
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestOperationsDashboardFlushCountsOnlyCompletedProtocolOutcomes(t *testing.T) {
	t.Parallel()
	db := migratedTestDB(t, "dashboard-metrics.db")
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
	t.Parallel()
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

// TestHostSampleFromRawWritesAggregateNetworkTotals 验证网络聚合累计总量只在采集成功时落库；
// 网络不可用（采集失败）时保持空值，不伪造 0。
func TestHostSampleFromRawWritesAggregateNetworkTotals(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	raw := HostRawSample{At: at, NetworkState: repository.MetricStateOK, NetworkReceiveBytes: 1234, NetworkTransmitBytes: 5678}
	item := hostSampleFromRaw(raw)
	if item.NetworkReceiveBytesTotal == nil || *item.NetworkReceiveBytesTotal != 1234 {
		t.Fatalf("聚合接收总量未落库：%v", item.NetworkReceiveBytesTotal)
	}
	if item.NetworkTransmitBytesTotal == nil || *item.NetworkTransmitBytesTotal != 5678 {
		t.Fatalf("聚合发送总量未落库：%v", item.NetworkTransmitBytesTotal)
	}
	failed := hostSampleFromRaw(HostRawSample{At: at, NetworkState: repository.MetricStateUnavailable})
	if failed.NetworkReceiveBytesTotal != nil || failed.NetworkTransmitBytesTotal != nil {
		t.Fatalf("网络不可用时总量必须为空：%+v", failed)
	}
}

// TestHostInterfaceSamplesFromRawPairsByInterfaceName 验证逐网卡样本口径：
// 累计总量原样落库；速率按**同名网卡**与上一份样本差分；网卡新增（无上一份）速率留空、
// 消失的网卡不写行；网络不可用时不写任何逐网卡行。
func TestHostInterfaceSamplesFromRawPairsByInterfaceName(t *testing.T) {
	t.Parallel()
	const bucket = "2026-08-27T10:00:00Z"
	previous := &HostRawSample{NetworkState: repository.MetricStateOK, NetworkInterfaces: []HostNetworkInterface{
		{Name: "eth0", ReceiveBytes: 1000, TransmitBytes: 500},
		{Name: "wlan0", ReceiveBytes: 100, TransmitBytes: 50},
	}}
	// eth0 继续存在（可算速率）；wlan0 消失（不写行）；eth1 新增（无上一份，速率留空）。
	raw := HostRawSample{NetworkState: repository.MetricStateOK, NetworkInterfaces: []HostNetworkInterface{
		{Name: "eth0", ReceiveBytes: 3000, TransmitBytes: 2500},
		{Name: "eth1", ReceiveBytes: 70, TransmitBytes: 30},
	}}
	samples := hostInterfaceSamplesFromRaw(bucket, raw, previous, 10)
	if len(samples) != 2 {
		t.Fatalf("逐网卡样本数 = %d，期望 2（wlan0 已消失不写行）：%+v", len(samples), samples)
	}
	if samples[0].BucketStart != bucket || samples[0].Interface != "eth0" || samples[0].State != repository.MetricStateOK {
		t.Fatalf("eth0 行基本字段错误：%+v", samples[0])
	}
	if samples[0].ReceiveBytesTotal == nil || *samples[0].ReceiveBytesTotal != 3000 ||
		samples[0].TransmitBytesTotal == nil || *samples[0].TransmitBytesTotal != 2500 {
		t.Fatalf("eth0 累计总量未落库：%+v", samples[0])
	}
	if samples[0].ReceiveBytesPerSecond == nil || *samples[0].ReceiveBytesPerSecond != 200 ||
		samples[0].TransmitBytesPerSecond == nil || *samples[0].TransmitBytesPerSecond != 200 {
		t.Fatalf("eth0 同名网卡差分错误：%+v", samples[0])
	}
	if samples[1].Interface != "eth1" || samples[1].ReceiveBytesTotal == nil || *samples[1].ReceiveBytesTotal != 70 {
		t.Fatalf("新网卡总量未落库：%+v", samples[1])
	}
	if samples[1].ReceiveBytesPerSecond != nil || samples[1].TransmitBytesPerSecond != nil {
		t.Fatalf("新增网卡速率必须留空，不伪造 0：%+v", samples[1])
	}
	// 首份样本（无上一份）：所有网卡都只有总量、没有速率。
	first := hostInterfaceSamplesFromRaw(bucket, raw, nil, 0)
	if len(first) != 2 || first[0].ReceiveBytesPerSecond != nil || first[0].ReceiveBytesTotal == nil {
		t.Fatalf("首份样本应只写总量：%+v", first)
	}
	// 网络不可用：不写任何逐网卡行（由聚合样本的 networkState 解释）。
	if items := hostInterfaceSamplesFromRaw(bucket, HostRawSample{NetworkState: repository.MetricStateUnavailable}, previous, 10); len(items) != 0 {
		t.Fatalf("网络不可用时应无逐网卡行：%+v", items)
	}
	// 计数回退（网卡重置）：速率留空而不是负数。
	regressed := hostInterfaceSamplesFromRaw(bucket, HostRawSample{NetworkState: repository.MetricStateOK, NetworkInterfaces: []HostNetworkInterface{
		{Name: "eth0", ReceiveBytes: 10, TransmitBytes: 20},
	}}, previous, 10)
	if len(regressed) != 1 || regressed[0].ReceiveBytesPerSecond != nil || regressed[0].TransmitBytesPerSecond != nil {
		t.Fatalf("计数回退时速率必须留空：%+v", regressed)
	}
}
