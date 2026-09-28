package repository

import (
	"errors"
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
	memoryTotal := int64(16 << 30)
	memoryAvailable := int64(6 << 30)
	memoryUsed := memoryTotal - memoryAvailable
	diskTotal := int64(500 << 30)
	diskAvailable := int64(200 << 30)
	diskUsed := diskTotal - diskAvailable
	uptime := int64(3600)
	if err := repo.PutHostSample(HostMetricSample{
		BucketStart: formatMetricTime(base), HostState: MetricStateOK, NetworkState: MetricStateUnavailable,
		NetworkErrorCode: "network_unavailable", ProcessState: MetricStateOK, ReadinessState: MetricStateOK,
		MemoryTotalBytes: &memoryTotal, MemoryAvailableBytes: &memoryAvailable, MemoryUsedBytes: &memoryUsed,
		DiskTotalBytes: &diskTotal, DiskAvailableBytes: &diskAvailable, DiskUsedBytes: &diskUsed,
		ProcessUptimeSeconds: &uptime,
	}); err != nil {
		t.Fatalf("写入主机样本：%v", err)
	}
	latest, err := repo.LatestHostSample()
	if err != nil {
		t.Fatalf("读取主机样本：%v", err)
	}
	if latest.NetworkState != MetricStateUnavailable || latest.NetworkErrorCode != "network_unavailable" {
		t.Fatalf("主机状态未持久化：%+v", latest)
	}
	// 新容量与运行时长字段必须整列落库并原样读回（迁移 0039 的四列）。
	if latest.MemoryUsedBytes == nil || *latest.MemoryUsedBytes != memoryUsed {
		t.Fatalf("内存已用未持久化：%+v", latest.MemoryUsedBytes)
	}
	if latest.DiskTotalBytes == nil || *latest.DiskTotalBytes != diskTotal {
		t.Fatalf("磁盘总量未持久化：%+v", latest.DiskTotalBytes)
	}
	if latest.DiskUsedBytes == nil || *latest.DiskUsedBytes != diskUsed {
		t.Fatalf("磁盘已用未持久化：%+v", latest.DiskUsedBytes)
	}
	if latest.ProcessUptimeSeconds == nil || *latest.ProcessUptimeSeconds != uptime {
		t.Fatalf("进程运行时长未持久化：%+v", latest.ProcessUptimeSeconds)
	}
	rangeItems, err := repo.HostSamples(base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("范围读取主机样本：%v", err)
	}
	if len(rangeItems) != 1 || rangeItems[0].MemoryUsedBytes == nil || *rangeItems[0].MemoryUsedBytes != memoryUsed ||
		rangeItems[0].DiskTotalBytes == nil || *rangeItems[0].DiskTotalBytes != diskTotal ||
		rangeItems[0].DiskUsedBytes == nil || *rangeItems[0].DiskUsedBytes != diskUsed ||
		rangeItems[0].ProcessUptimeSeconds == nil || *rangeItems[0].ProcessUptimeSeconds != uptime {
		t.Fatalf("范围查询未返回新字段：%+v", rangeItems)
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

// TestOperationsObservabilityHostNetworkInterfaces 覆盖迁移 0042：
// host_metric_minute 的聚合累计总量整列往返，逐网卡新表的 upsert / 按网卡过滤 / 各网卡最新一行。
func TestOperationsObservabilityHostNetworkInterfaces(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "operations-host-network.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewOperationsObservabilityRepo(db)
	first := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)

	// 聚合累计总量（host_metric_minute 新增两列）必须整列落库并原样读回。
	receiveTotal, transmitTotal := int64(9_000_000), int64(4_500_000)
	if err := repo.PutHostSample(HostMetricSample{
		BucketStart: formatMetricTime(first), HostState: MetricStateOK, NetworkState: MetricStateOK,
		ProcessState: MetricStateOK, ReadinessState: MetricStateOK,
		NetworkReceiveBytesTotal: &receiveTotal, NetworkTransmitBytesTotal: &transmitTotal,
	}); err != nil {
		t.Fatalf("写入主机样本：%v", err)
	}
	latest, err := repo.LatestHostSample()
	if err != nil {
		t.Fatalf("读取主机样本：%v", err)
	}
	if latest.NetworkReceiveBytesTotal == nil || *latest.NetworkReceiveBytesTotal != receiveTotal ||
		latest.NetworkTransmitBytesTotal == nil || *latest.NetworkTransmitBytesTotal != transmitTotal {
		t.Fatalf("聚合累计总量未持久化：%+v", latest)
	}
	// 历史行（未写总量）必须保持 NULL，而不是被读成 0。
	if err := repo.PutHostSample(HostMetricSample{
		BucketStart: formatMetricTime(second), HostState: MetricStateOK, NetworkState: MetricStateUnavailable,
		NetworkErrorCode: "network_unavailable", ProcessState: MetricStateOK, ReadinessState: MetricStateOK,
	}); err != nil {
		t.Fatalf("写入无网络主机样本：%v", err)
	}
	samples, err := repo.HostSamples(first.Add(-time.Minute), second.Add(time.Minute))
	if err != nil {
		t.Fatalf("范围读取主机样本：%v", err)
	}
	if len(samples) != 2 || samples[1].NetworkReceiveBytesTotal != nil || samples[1].NetworkTransmitBytesTotal != nil {
		t.Fatalf("采集失败样本的总量必须为 NULL：%+v", samples)
	}

	// 逐网卡：同一分钟两网卡 + 下一分钟更新 eth0，验证 upsert、过滤与各网卡最新行。
	ethReceive, ethTransmit := int64(600), int64(900)
	ethRate := 8.0
	wlanReceive, wlanTransmit := int64(100), int64(200)
	if err := repo.PutHostNetworkInterfaces([]HostNetworkInterfaceSample{
		{BucketStart: formatMetricTime(first), Interface: "eth0", State: MetricStateOK,
			ReceiveBytesTotal: &ethReceive, TransmitBytesTotal: &ethTransmit, ReceiveBytesPerSecond: &ethRate},
		{BucketStart: formatMetricTime(first), Interface: "wlan0", State: MetricStateOK,
			ReceiveBytesTotal: &wlanReceive, TransmitBytesTotal: &wlanTransmit},
	}); err != nil {
		t.Fatalf("写入逐网卡样本：%v", err)
	}
	ethReceive2 := int64(1000)
	if err := repo.PutHostNetworkInterfaces([]HostNetworkInterfaceSample{
		{BucketStart: formatMetricTime(second), Interface: "eth0", State: MetricStateOK, ReceiveBytesTotal: &ethReceive2},
	}); err != nil {
		t.Fatalf("写入第二分钟逐网卡样本：%v", err)
	}
	// 重复写入同一 (bucket_start, interface) 必须覆盖而不是新增一行。
	ethReceive2Updated := int64(1500)
	if err := repo.PutHostNetworkInterfaces([]HostNetworkInterfaceSample{
		{BucketStart: formatMetricTime(second), Interface: "eth0", State: MetricStateOK, ReceiveBytesTotal: &ethReceive2Updated},
	}); err != nil {
		t.Fatalf("覆盖逐网卡样本：%v", err)
	}

	ethRows, err := repo.HostNetworkInterfaceSamples(first.Add(-time.Minute), second.Add(time.Minute), "eth0")
	if err != nil {
		t.Fatalf("按网卡读取样本：%v", err)
	}
	if len(ethRows) != 2 {
		t.Fatalf("eth0 行数 = %d，期望 2（ON CONFLICT 覆盖而非新增）：%+v", len(ethRows), ethRows)
	}
	if ethRows[0].ReceiveBytesTotal == nil || *ethRows[0].ReceiveBytesTotal != ethReceive ||
		ethRows[0].ReceiveBytesPerSecond == nil || *ethRows[0].ReceiveBytesPerSecond != ethRate {
		t.Fatalf("eth0 首行错误：%+v", ethRows[0])
	}
	if ethRows[1].ReceiveBytesTotal == nil || *ethRows[1].ReceiveBytesTotal != ethReceive2Updated {
		t.Fatalf("eth0 第二次写入未覆盖：%+v", ethRows[1])
	}
	if ethRows[1].TransmitBytesTotal != nil || ethRows[1].ReceiveBytesPerSecond != nil {
		t.Fatalf("未提供的可选列必须保持 NULL：%+v", ethRows[1])
	}

	latestInterfaces, err := repo.LatestHostNetworkInterfaces(first.Add(-time.Minute), second.Add(time.Minute))
	if err != nil {
		t.Fatalf("读取各网卡最新行：%v", err)
	}
	if len(latestInterfaces) != 2 {
		t.Fatalf("网卡数 = %d，期望 2：%+v", len(latestInterfaces), latestInterfaces)
	}
	byName := map[string]HostNetworkInterfaceSample{}
	for _, item := range latestInterfaces {
		byName[item.Interface] = item
	}
	if item, ok := byName["eth0"]; !ok || item.BucketStart != formatMetricTime(second) || item.ReceiveBytesTotal == nil || *item.ReceiveBytesTotal != ethReceive2Updated {
		t.Fatalf("eth0 最新行错误：%+v", byName["eth0"])
	}
	if item, ok := byName["wlan0"]; !ok || item.BucketStart != formatMetricTime(first) || item.ReceiveBytesTotal == nil || *item.ReceiveBytesTotal != wlanReceive {
		t.Fatalf("wlan0 最新行错误：%+v", byName["wlan0"])
	}

	latestEth, err := repo.LatestHostNetworkInterface("eth0")
	if err != nil {
		t.Fatalf("读取指定网卡最新行：%v", err)
	}
	if latestEth.BucketStart != formatMetricTime(second) || latestEth.ReceiveBytesTotal == nil || *latestEth.ReceiveBytesTotal != ethReceive2Updated {
		t.Fatalf("指定网卡最新行错误：%+v", latestEth)
	}
	if _, err := repo.LatestHostNetworkInterface("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的网卡应返回 ErrNotFound，得 %v", err)
	}

	// 留存清理必须同时覆盖新表。
	if err := repo.DeleteBefore(second.Add(time.Second)); err != nil {
		t.Fatalf("清理过期数据：%v", err)
	}
	remaining, err := repo.HostNetworkInterfaceSamples(first.Add(-time.Minute), second.Add(time.Minute), "eth0")
	if err != nil {
		t.Fatalf("清理后读取：%v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("过期逐网卡行未清理：%+v", remaining)
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
