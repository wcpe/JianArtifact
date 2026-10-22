package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
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

// legacyCapacitySQL 是优化前那一条 repository LEFT JOIN asset 的单语句聚合。
// 保留为口径 oracle：拆解后的新实现必须与它返回**完全相同**的三个值。
const legacyCapacitySQL = `SELECT COUNT(DISTINCT repository.id) AS repository_count, COALESCE(SUM(size),0) AS logical_bytes, COUNT(asset.id) AS asset_count
	FROM repository LEFT JOIN asset ON asset.repository_id = repository.id`

// capacityOracle 直接用旧 SQL 取一次容量，作为新实现的口径基准。
func capacityOracle(t *testing.T, db *persistence.DB) CapacitySnapshot {
	t.Helper()
	var item CapacitySnapshot
	if err := db.Get(&item, legacyCapacitySQL); err != nil {
		t.Fatalf("读取口径 oracle：%v", err)
	}
	return item
}

// openCapacityTestDB 建一个迁移完成的空库，供容量口径测试使用。
func openCapacityTestDB(t *testing.T, name string) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	return db
}

// TestOperationsObservabilityCurrentCapacityMatchesLegacyJoin 锁定拆解实现的三个字段口径
// 与旧的单条 JOIN 聚合逐一相等：空库、只有仓库没有资产、有资产与无资产仓库混合三种形态。
func TestOperationsObservabilityCurrentCapacityMatchesLegacyJoin(t *testing.T) {
	t.Run("空库", func(t *testing.T) {
		db := openCapacityTestDB(t, "capacity-empty-db.db")
		metrics := NewOperationsObservabilityRepo(db)

		got, err := metrics.CurrentCapacity()
		if err != nil {
			t.Fatalf("读取容量：%v", err)
		}
		if want := (CapacitySnapshot{}); got != want {
			t.Fatalf("空库容量=%+v，期望全零 %+v", got, want)
		}
		if oracle := capacityOracle(t, db); oracle != got {
			t.Fatalf("空库口径不一致：新=%+v 旧=%+v", got, oracle)
		}
	})

	t.Run("只有仓库没有资产", func(t *testing.T) {
		db := openCapacityTestDB(t, "capacity-no-assets.db")
		repoRepo := NewRepoRepo(db)
		metrics := NewOperationsObservabilityRepo(db)
		for _, name := range []string{"bare-a", "bare-b", "bare-c"} {
			if _, err := repoRepo.Create(name, "raw", "hosted", "private", ""); err != nil {
				t.Fatalf("建仓库 %s：%v", name, err)
			}
		}

		got, err := metrics.CurrentCapacity()
		if err != nil {
			t.Fatalf("读取容量：%v", err)
		}
		// 无资产的仓库必须计入仓库数：这是 LEFT JOIN 侧保留下来的语义。
		want := CapacitySnapshot{RepositoryCount: 3, AssetCount: 0, LogicalBytes: 0}
		if got != want {
			t.Fatalf("容量=%+v，期望 %+v（空仓库也要计入仓库数）", got, want)
		}
		if oracle := capacityOracle(t, db); oracle != got {
			t.Fatalf("口径不一致：新=%+v 旧=%+v", got, oracle)
		}
	})

	t.Run("有资产与无资产仓库混合", func(t *testing.T) {
		db := openCapacityTestDB(t, "capacity-mixed.db")
		repoRepo := NewRepoRepo(db)
		assetRepo := NewAssetRepo(db)
		metrics := NewOperationsObservabilityRepo(db)

		// 仓库一：3 个资产；仓库二：1 个资产；仓库三、四：无资产。
		sizes := map[string][]int64{"mix-rich": {10, 20, 30}, "mix-single": {7}}
		for name, assetSizes := range sizes {
			if _, err := repoRepo.Create(name, "raw", "hosted", "private", ""); err != nil {
				t.Fatalf("建仓库 %s：%v", name, err)
			}
			repo, err := repoRepo.GetByName(name)
			if err != nil {
				t.Fatalf("读仓库 %s：%v", name, err)
			}
			for i, size := range assetSizes {
				if err := assetRepo.Upsert(repo.ID, fmt.Sprintf("f-%d.bin", i), fmt.Sprintf("hash-%s-%d", name, i),
					size, "application/octet-stream", "", ""); err != nil {
					t.Fatalf("写资产 %s/%d：%v", name, i, err)
				}
			}
		}
		for _, name := range []string{"mix-empty-a", "mix-empty-b"} {
			if _, err := repoRepo.Create(name, "raw", "hosted", "private", ""); err != nil {
				t.Fatalf("建空仓库 %s：%v", name, err)
			}
		}

		got, err := metrics.CurrentCapacity()
		if err != nil {
			t.Fatalf("读取容量：%v", err)
		}
		want := CapacitySnapshot{RepositoryCount: 4, AssetCount: 4, LogicalBytes: 67}
		if got != want {
			t.Fatalf("容量=%+v，期望 %+v", got, want)
		}
		if oracle := capacityOracle(t, db); oracle != got {
			t.Fatalf("口径不一致：新=%+v 旧=%+v", got, oracle)
		}
	})
}

// TestOperationsObservabilityCurrentCapacityCacheReusesAndExpires 锁定缓存语义：
// 命中不重复查库、TTL 内数据变化不外泄、过期后重算并反映最新数据。
func TestOperationsObservabilityCurrentCapacityCacheReusesAndExpires(t *testing.T) {
	db, counter, err := persistence.OpenCountingForTest(filepath.Join(t.TempDir(), "capacity-cache.db"))
	if err != nil {
		t.Fatalf("打开计数数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repoRepo := NewRepoRepo(db)
	assetRepo := NewAssetRepo(db)
	metrics := NewOperationsObservabilityRepo(db)
	// 注入可控时钟，确定性地驱动 TTL 过期（不靠 sleep）。
	clock := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	metrics.capacityNow = func() time.Time { return clock }

	if _, err := repoRepo.Create("cache-repo", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	repo, err := repoRepo.GetByName("cache-repo")
	if err != nil {
		t.Fatalf("读仓库：%v", err)
	}
	writeAsset := func(name string, size int64) {
		t.Helper()
		if err := assetRepo.Upsert(repo.ID, name, "hash-"+name, size, "application/octet-stream", "", ""); err != nil {
			t.Fatalf("写资产 %s：%v", name, err)
		}
	}
	writeAsset("one.bin", 100)
	writeAsset("two.bin", 200)

	// 首次调用未命中：应发出两条单表查询（资产侧 + 仓库侧），不含 JOIN。
	atomic.StoreInt64(counter, 0)
	first, err := metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("首次读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(counter); statements != 1 {
		t.Fatalf("未命中时语句数=%d，期望 1（资产与仓库合并在同一条语句里）", statements)
	}
	if want := (CapacitySnapshot{RepositoryCount: 1, AssetCount: 2, LogicalBytes: 300}); first != want {
		t.Fatalf("首次容量=%+v，期望 %+v", first, want)
	}

	// 命中：一条语句都不应发出，且返回值与首次相同。
	atomic.StoreInt64(counter, 0)
	second, err := metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("再次读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(counter); statements != 0 {
		t.Fatalf("缓存命中时语句数=%d，期望 0（不得重复查库）", statements)
	}
	if second != first {
		t.Fatalf("缓存命中返回值=%+v，期望与首次一致 %+v", second, first)
	}

	// TTL 内新增资产：缓存仍在有效期，返回的仍是旧值（这是缓存的既定代价）。
	writeAsset("three.bin", 400)
	atomic.StoreInt64(counter, 0)
	fresh, err := metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("TTL 内读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(counter); statements != 0 {
		t.Fatalf("TTL 内语句数=%d，期望 0", statements)
	}
	if fresh != first {
		t.Fatalf("TTL 内返回值=%+v，期望仍为 %+v", fresh, first)
	}

	// 过期后重算：重新查库，且反映新增的资产（未注入 TTL 时用兜底默认值）。
	clock = clock.Add(defaultCapacityCacheTTL)
	atomic.StoreInt64(counter, 0)
	expired, err := metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("过期后读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(counter); statements != 1 {
		t.Fatalf("过期后语句数=%d，期望 2（重新聚合）", statements)
	}
	if want := (CapacitySnapshot{RepositoryCount: 1, AssetCount: 3, LogicalBytes: 700}); expired != want {
		t.Fatalf("过期后容量=%+v，期望 %+v", expired, want)
	}
}

// TestOperationsObservabilityCurrentCapacityCacheIsPerInstance 锁定缓存不跨实例共享：
// 每个 repo 实例各自缓存，因此不同测试/不同服务实例之间不会互相污染。
func TestOperationsObservabilityCurrentCapacityCacheIsPerInstance(t *testing.T) {
	db := openCapacityTestDB(t, "capacity-cache-instances.db")
	repoRepo := NewRepoRepo(db)
	assetRepo := NewAssetRepo(db)
	first := NewOperationsObservabilityRepo(db)

	if _, err := repoRepo.Create("iso-repo", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	repo, err := repoRepo.GetByName("iso-repo")
	if err != nil {
		t.Fatalf("读仓库：%v", err)
	}
	if err := assetRepo.Upsert(repo.ID, "a.bin", "hash-a", 10, "application/octet-stream", "", ""); err != nil {
		t.Fatalf("写资产：%v", err)
	}
	warm, err := first.CurrentCapacity()
	if err != nil {
		t.Fatalf("首次实例读取容量：%v", err)
	}
	if warm.AssetCount != 1 {
		t.Fatalf("首次实例资产数=%d，期望 1", warm.AssetCount)
	}

	// 数据库在缓存填充之后变化。
	if err := assetRepo.Upsert(repo.ID, "b.bin", "hash-b", 20, "application/octet-stream", "", ""); err != nil {
		t.Fatalf("写第二个资产：%v", err)
	}

	// 新实例没有任何缓存，必须看到最新数据。
	second := NewOperationsObservabilityRepo(db)
	fresh, err := second.CurrentCapacity()
	if err != nil {
		t.Fatalf("第二个实例读取容量：%v", err)
	}
	if fresh.AssetCount != 2 || fresh.LogicalBytes != 30 {
		t.Fatalf("第二个实例容量=%+v，期望资产数 2、字节数 30（不得复用别的实例缓存）", fresh)
	}

	// 原实例仍在 TTL 内，继续返回缓存值（行为明确记录在案）。
	again, err := first.CurrentCapacity()
	if err != nil {
		t.Fatalf("原实例再次读取容量：%v", err)
	}
	if again != warm {
		t.Fatalf("原实例 TTL 内返回=%+v，期望仍为 %+v", again, warm)
	}
}

// TestOperationsObservabilityCurrentCapacityDoesNotCacheErrors 锁定失败不写缓存：
// 查询失败后不能把零值当成有效结果固化在 TTL 内。
func TestOperationsObservabilityCurrentCapacityDoesNotCacheErrors(t *testing.T) {
	db := openCapacityTestDB(t, "capacity-error.db")
	metrics := NewOperationsObservabilityRepo(db)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库：%v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := metrics.CurrentCapacity(); err == nil {
			t.Fatalf("第 %d 次调用：数据库已关闭，期望返回错误", i+1)
		}
	}
}

// capacityTestFixture 建一个带可控时钟的容量测试环境：迁移完成的计数数据库、
// 注入的时钟与两个仓储。返回值里的 advance 用于确定性地推进时间（不靠 sleep）。
type capacityTestFixture struct {
	db      *persistence.DB
	counter *int64
	metrics *OperationsObservabilityRepo
	repo    *Repository
	assets  *AssetRepo
	advance func(time.Duration)
}

// newCapacityCacheFixture 准备容量缓存测试夹具；countStatements 由调用方通过 fixture.counter 读取。
func newCapacityCacheFixture(t *testing.T, name string) *capacityTestFixture {
	t.Helper()
	db, counter, err := persistence.OpenCountingForTest(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("打开计数数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repoRepo := NewRepoRepo(db)
	if _, err := repoRepo.Create("ttl-repo", "raw", "hosted", "private", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	created, err := repoRepo.GetByName("ttl-repo")
	if err != nil {
		t.Fatalf("读仓库：%v", err)
	}
	clock := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	metrics := NewOperationsObservabilityRepo(db)
	metrics.capacityNow = func() time.Time { return clock }
	return &capacityTestFixture{
		db:      db,
		counter: counter,
		metrics: metrics,
		repo:    created,
		assets:  NewAssetRepo(db),
		advance: func(d time.Duration) { clock = clock.Add(d) },
	}
}

// writeAsset 向夹具仓库写入一个资产。
func (f *capacityTestFixture) writeAsset(t *testing.T, name string, size int64) {
	t.Helper()
	if err := f.assets.Upsert(f.repo.ID, name, "hash-"+name, size, "application/octet-stream", "", ""); err != nil {
		t.Fatalf("写资产 %s：%v", name, err)
	}
}

// TestOperationsObservabilityCurrentCapacityCacheTTLIsConfigurable 锁定注入的 TTL 真正生效：
// 未注入时沿用兜底默认值；注入后过期边界由注入值决定，而不是写死的 30s。
func TestOperationsObservabilityCurrentCapacityCacheTTLIsConfigurable(t *testing.T) {
	fixture := newCapacityCacheFixture(t, "capacity-ttl-configured.db")
	// 未注入时取兜底默认值（装配层正常情况下会用 JIAN_CAPACITY_CACHE_TTL_SECONDS 的解析结果覆盖）。
	if fixture.metrics.capacityTTL != defaultCapacityCacheTTL {
		t.Fatalf("未注入时 TTL=%s，期望兜底默认 %s", fixture.metrics.capacityTTL, defaultCapacityCacheTTL)
	}
	// 注入一个明显不同于默认值的短 TTL：过期边界必须跟着它走。
	const ttl = 5 * time.Second
	fixture.metrics.SetCapacityCacheTTL(ttl)
	fixture.writeAsset(t, "one.bin", 100)

	atomic.StoreInt64(fixture.counter, 0)
	first, err := fixture.metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("首次读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(fixture.counter); statements != 1 {
		t.Fatalf("未命中时语句数=%d，期望 2", statements)
	}
	if want := (CapacitySnapshot{RepositoryCount: 1, AssetCount: 1, LogicalBytes: 100}); first != want {
		t.Fatalf("首次容量=%+v，期望 %+v", first, want)
	}

	// TTL 内（4s）命中：一条语句都不发。
	fixture.advance(ttl - time.Second)
	atomic.StoreInt64(fixture.counter, 0)
	if _, err := fixture.metrics.CurrentCapacity(); err != nil {
		t.Fatalf("TTL 内读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(fixture.counter); statements != 0 {
		t.Fatalf("TTL 内语句数=%d，期望 0（命中缓存）", statements)
	}

	// 到达截止时刻（正好 ttl）即视为过期：重算一次，并反映期间新增的资产。
	fixture.writeAsset(t, "two.bin", 200)
	fixture.advance(time.Second)
	atomic.StoreInt64(fixture.counter, 0)
	expired, err := fixture.metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("过期后读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(fixture.counter); statements != 1 {
		t.Fatalf("过期后语句数=%d，期望 2（重新聚合）", statements)
	}
	if want := (CapacitySnapshot{RepositoryCount: 1, AssetCount: 2, LogicalBytes: 300}); expired != want {
		t.Fatalf("过期后容量=%+v，期望 %+v", expired, want)
	}

	// 反向确认窗口不是写死的 30s：再走 4s 仍在新的 5s 窗口内，仍是命中。
	fixture.advance(ttl - time.Second)
	atomic.StoreInt64(fixture.counter, 0)
	if _, err := fixture.metrics.CurrentCapacity(); err != nil {
		t.Fatalf("第二个窗口内读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(fixture.counter); statements != 0 {
		t.Fatalf("第二个窗口内语句数=%d，期望 0（窗口按注入值= %s 而不是默认值）", statements, ttl)
	}
}

// TestOperationsObservabilityCurrentCapacityCacheDisabledWhenTTLZero 锁定 TTL <= 0 的禁用语义：
// 缓存永不命中，每次调用都重新聚合，因此数据变化立刻可见。
//
// 语义依据见 config.capacityCacheTTL 的注释：容量数字只是展示口径，禁用缓存只增加查询成本，
// 不放开任何无界操作，所以它是正当的运维配置（需要实时数字时使用）。
func TestOperationsObservabilityCurrentCapacityCacheDisabledWhenTTLZero(t *testing.T) {
	fixture := newCapacityCacheFixture(t, "capacity-ttl-disabled.db")
	fixture.metrics.SetCapacityCacheTTL(0)
	fixture.writeAsset(t, "one.bin", 100)

	// 连续三次调用：每次都必须是两条查询，且每次的值都取自最新数据。
	wantSizes := []int64{100, 400, 900}
	for index, want := range wantSizes {
		if index > 0 {
			fixture.writeAsset(t, fmt.Sprintf("extra-%d.bin", index), want-wantSizes[index-1])
		}
		// 时钟完全不推进：缓存若仍生效，第二次起就会命中并返回旧值。
		atomic.StoreInt64(fixture.counter, 0)
		got, err := fixture.metrics.CurrentCapacity()
		if err != nil {
			t.Fatalf("第 %d 次读取容量：%v", index+1, err)
		}
		if statements := atomic.LoadInt64(fixture.counter); statements != 1 {
			t.Fatalf("第 %d 次语句数=%d，期望 2（禁用缓存后每次都重算）", index+1, statements)
		}
		if got.LogicalBytes != want || got.AssetCount != int64(index+1) {
			t.Fatalf("第 %d 次容量=%+v，期望资产数 %d、字节数 %d（禁用缓存必须实时反映）", index+1, got, index+1, want)
		}
	}
	// 禁用期间不得留下「有效值」：内部标记必须保持 false。
	if fixture.metrics.capacityValid {
		t.Fatalf("TTL=0 时不得写入缓存，capacityValid 却为 true")
	}
}

// TestOperationsObservabilitySetCapacityCacheTTLInvalidatesWarmCache 锁定改档语义：
// 注入新 TTL 后既有缓存立即失效——否则「把 30s 改成 60s」在第一次重算前仍按旧窗口返回，
// 改成 0（禁用）时更会继续返回缓存值，与运维的配置意图相反。
func TestOperationsObservabilitySetCapacityCacheTTLInvalidatesWarmCache(t *testing.T) {
	fixture := newCapacityCacheFixture(t, "capacity-ttl-switch.db")
	fixture.writeAsset(t, "one.bin", 100)
	if _, err := fixture.metrics.CurrentCapacity(); err != nil {
		t.Fatalf("预热缓存：%v", err)
	}

	// 缓存已预热，数据随后变化；改档必须让旧值立刻作废。
	fixture.writeAsset(t, "two.bin", 200)
	fixture.metrics.SetCapacityCacheTTL(time.Hour)
	atomic.StoreInt64(fixture.counter, 0)
	afterSwitch, err := fixture.metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("改档后读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(fixture.counter); statements != 1 {
		t.Fatalf("改档后语句数=%d，期望 2（旧缓存必须失效并重算）", statements)
	}
	if afterSwitch.AssetCount != 2 || afterSwitch.LogicalBytes != 300 {
		t.Fatalf("改档后容量=%+v，期望资产数 2、字节数 300", afterSwitch)
	}

	// 从正值改成 0（禁用）：下一次调用同样必须重算，而不是继续用 1 小时的旧缓存。
	fixture.writeAsset(t, "three.bin", 400)
	fixture.metrics.SetCapacityCacheTTL(0)
	atomic.StoreInt64(fixture.counter, 0)
	afterDisable, err := fixture.metrics.CurrentCapacity()
	if err != nil {
		t.Fatalf("改成禁用后读取容量：%v", err)
	}
	if statements := atomic.LoadInt64(fixture.counter); statements != 1 {
		t.Fatalf("改成禁用后语句数=%d，期望 2（立即重算）", statements)
	}
	if afterDisable.AssetCount != 3 || afterDisable.LogicalBytes != 700 {
		t.Fatalf("改成禁用后容量=%+v，期望资产数 3、字节数 700", afterDisable)
	}
}

// failSecondExec 把真实事务包装一层，在第二次 Exec 时注入失败：
// 用于模拟「总体样本已写、网卡明细写入失败」这一崩溃/报错点。
type failSecondExec struct {
	tx    *sql.Tx
	calls int
}

func (f *failSecondExec) Exec(query string, args ...any) (sql.Result, error) {
	f.calls++
	if f.calls == 2 {
		return nil, errors.New("注入失败：第二次写入")
	}
	return f.tx.Exec(query, args...)
}

// TestOperationsObservabilityRepoWritesHostSampleAndInterfacesInOneTransaction 锁定两表同事务：
// 合并入口必须同时写入两张表；中途失败回滚后，两张表都不留痕（不再出现「总账有、明细无」）。
func TestOperationsObservabilityRepoWritesHostSampleAndInterfacesInOneTransaction(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "host-sample-transaction.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewOperationsObservabilityRepo(db)
	base := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	sample := HostMetricSample{BucketStart: formatMetricTime(base), HostState: "ok", NetworkState: "ok"}
	interfaces := []HostNetworkInterfaceSample{
		{BucketStart: formatMetricTime(base), Interface: "eth0", State: "ok"},
		{BucketStart: formatMetricTime(base), Interface: "lo", State: "ok"},
	}

	// 正向：合并入口一次写入总体样本与两条网卡明细。
	if err := repo.PutHostSampleWithInterfaces(sample, interfaces); err != nil {
		t.Fatalf("合并写入：%v", err)
	}
	if got := countRows(t, db, "host_metric_minute"); got != 1 {
		t.Fatalf("总体样本行数：期望 1，得 %d", got)
	}
	if got := countRows(t, db, "host_network_interface_minute"); got != 2 {
		t.Fatalf("网卡明细行数：期望 2，得 %d", got)
	}

	// 反向：先清空，再用注入失败的事务走同一段写入，回滚后两张表都必须为空。
	if _, err := db.Exec("DELETE FROM host_metric_minute"); err != nil {
		t.Fatalf("清空总体样本：%v", err)
	}
	if _, err := db.Exec("DELETE FROM host_network_interface_minute"); err != nil {
		t.Fatalf("清空网卡明细：%v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	exec := &failSecondExec{tx: tx}
	if err := putHostSample(exec, sample); err != nil {
		t.Fatalf("第一段写入本应成功：%v", err)
	}
	if err := putHostNetworkInterfaces(exec, interfaces); err == nil {
		t.Fatalf("第二段写入本应失败（注入点）")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚：%v", err)
	}
	if got := countRows(t, db, "host_metric_minute"); got != 0 {
		t.Fatalf("回滚后总体样本应无痕，实得 %d 行", got)
	}
	if got := countRows(t, db, "host_network_interface_minute"); got != 0 {
		t.Fatalf("回滚后网卡明细应无痕，实得 %d 行", got)
	}
}

func countRows(t *testing.T, db *persistence.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("统计 %s：%v", table, err)
	}
	return count
}
