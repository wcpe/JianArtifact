package repository

import (
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ProtocolMinute 是当前节点制品协议在一个 UTC 分钟内的聚合，不保存路径、主体或凭据。
type ProtocolMinute struct {
	BucketStart    string `db:"bucket_start"`
	RequestCount   int64  `db:"request_count"`
	DownloadCount  int64  `db:"download_count"`
	FailureCount   int64  `db:"failure_count"`
	CacheHitCount  int64  `db:"cache_hit_count"`
	CacheMissCount int64  `db:"cache_miss_count"`
}

// CapacitySnapshot 是逻辑制品视角的小时快照，不计物理 blob 去重后的占用。
type CapacitySnapshot struct {
	BucketStart     string `db:"bucket_start"`
	RepositoryCount int64  `db:"repository_count"`
	AssetCount      int64  `db:"asset_count"`
	LogicalBytes    int64  `db:"logical_bytes"`
}

// MetricState 描述单个监控指标组的可用性。
type MetricState string

const (
	MetricStateOK          MetricState = "ok"
	MetricStateUnavailable MetricState = "unavailable"
	MetricStateUnsupported MetricState = "unsupported"
	MetricStateError       MetricState = "error"
)

// HostMetricSample 是当前进程所在主机的一条分钟样本；空数值必须由状态解释，不能表示为伪造的零。
type HostMetricSample struct {
	BucketStart          string      `db:"bucket_start"`
	HostState            MetricState `db:"host_state"`
	HostErrorCode        string      `db:"host_error_code"`
	CPUPercent           *float64    `db:"cpu_percent"`
	MemoryTotalBytes     *int64      `db:"memory_total_bytes"`
	MemoryAvailableBytes *int64      `db:"memory_available_bytes"`
	// MemoryUsedBytes 口径固定为同一采样点的 memory_total_bytes − memory_available_bytes，前端只展示不复算。
	MemoryUsedBytes    *int64 `db:"memory_used_bytes"`
	DiskAvailableBytes *int64 `db:"disk_available_bytes"`
	DiskTotalBytes     *int64 `db:"disk_total_bytes"`
	// DiskUsedBytes 口径固定为同一采样点的 disk_total_bytes − disk_available_bytes，与内存已用算法对称。
	DiskUsedBytes                 *int64      `db:"disk_used_bytes"`
	NetworkState                  MetricState `db:"network_state"`
	NetworkErrorCode              string      `db:"network_error_code"`
	NetworkReceiveBytesPerSecond  *float64    `db:"network_receive_bytes_per_sec"`
	NetworkTransmitBytesPerSecond *float64    `db:"network_transmit_bytes_per_sec"`
	// NetworkReceiveBytesTotal / NetworkTransmitBytesTotal 是自网卡启动以来的累计字节数
	// （全部非回环网卡的聚合）；采集失败时为 NULL，不伪造零。
	NetworkReceiveBytesTotal  *int64      `db:"network_receive_bytes_total"`
	NetworkTransmitBytesTotal *int64      `db:"network_transmit_bytes_total"`
	ProcessState              MetricState `db:"process_state"`
	ProcessErrorCode          string      `db:"process_error_code"`
	ProcessRSSBytes           *int64      `db:"process_rss_bytes"`
	ProcessCPUPercent         *float64    `db:"process_cpu_percent"`
	// ProcessUptimeSeconds 是当前进程运行时长（挂钟口径，仅本进程，不做系统进程枚举）。
	ProcessUptimeSeconds *int64      `db:"process_uptime_seconds"`
	GoroutineCount       *int64      `db:"goroutine_count"`
	OpenFileDescriptors  *int64      `db:"open_file_descriptors"`
	ReadinessState       MetricState `db:"readiness_state"`
	ReadinessErrorCode   string      `db:"readiness_error_code"`
}

// HostNetworkInterfaceSample 是单个网卡的一个分钟样本：累计总量为自网卡启动以来的字节数，
// 速率为与**同名网卡**上一份样本差分的结果；网卡刚出现（无上一份）或计数回退时速率列为 NULL，
// 不伪造零。选定网卡后速率图与总量都取该网卡的行，避免与全网卡聚合值混读。
type HostNetworkInterfaceSample struct {
	BucketStart            string      `db:"bucket_start"`
	Interface              string      `db:"interface"`
	State                  MetricState `db:"state"`
	ErrorCode              string      `db:"error_code"`
	ReceiveBytesTotal      *int64      `db:"receive_bytes_total"`
	TransmitBytesTotal     *int64      `db:"transmit_bytes_total"`
	ReceiveBytesPerSecond  *float64    `db:"receive_bytes_per_sec"`
	TransmitBytesPerSecond *float64    `db:"transmit_bytes_per_sec"`
}

// OperationsObservabilityRepo 保存当前节点的可视化聚合；它从不参与复制。
type OperationsObservabilityRepo struct {
	db *persistence.DB

	// 容量聚合的短期缓存。容量数字只是仪表盘上的规模展示，不属于授权判定，
	// 因此允许在短窗内复用上一次的聚合结果（见 CurrentCapacity 的说明）。
	capacityMu    sync.Mutex
	capacityValue CapacitySnapshot
	capacityUntil time.Time
	capacityValid bool
	// capacityTTL 是缓存有效期；<= 0 表示禁用缓存。由装配层经 SetCapacityCacheTTL 注入
	// （JIAN_CAPACITY_CACHE_TTL_SECONDS 的解析结果），未注入时取 defaultCapacityCacheTTL。
	capacityTTL time.Duration
	// capacityNow 是可注入的时钟，测试用来确定性地驱动 TTL 过期；生产为 time.Now。
	capacityNow func() time.Time
}

// defaultCapacityCacheTTL 是容量聚合结果缓存有效期的**兜底默认值**（装配层经
// SetCapacityCacheTTL 注入 JIAN_CAPACITY_CACHE_TTL_SECONDS 的解析结果）。
//
// 为什么可以缓存：容量统计不是授权数据——鉴权与 ACL 判定仍逐请求读库，组内成员变动、
// 仓库可见性变更都即时生效，与本缓存无关；这里缓存的只是「仓库数 / 资产数 / 逻辑字节数」
// 这类规模数字，几十秒的滞后对运维展示没有影响。反过来，代价却很大：仪表盘是常用落地页，
// 每次加载都会调用本查询，而它必须遍历整张 asset 表（线上约 8.6 GB）。
//
// 与 config 侧 defaultCapacityCacheTTL 保持同值：装配层始终注入解析结果，
// 本常量只在未装配（测试直接构造）时兜底。
const defaultCapacityCacheTTL = 30 * time.Second

func NewOperationsObservabilityRepo(db *persistence.DB) *OperationsObservabilityRepo {
	return &OperationsObservabilityRepo{db: db, capacityTTL: defaultCapacityCacheTTL, capacityNow: time.Now}
}

// SetCapacityCacheTTL 设定容量聚合结果的缓存有效期：<= 0 表示**禁用缓存**（每次调用都重新
// 聚合），> 0 为复用窗口。语义由装配层（JIAN_CAPACITY_CACHE_TTL_SECONDS）决定，仓储层不
// 再对取值做二次加工——这里若擅自夹取，会让运维显式配置的值与实际行为不符。
//
// 有效期变化后**必须让既有缓存立即失效**：现有缓存值的截止时间是按旧 TTL 算出来的，
// 若沿用它，「从 30s 改成 60s」在第一次重算前仍按旧窗口返回、改小则可能继续超期返回。
// 写入在锁内完成，与 cachedCapacity / storeCapacity 互斥，不存在撕裂读。
func (r *OperationsObservabilityRepo) SetCapacityCacheTTL(ttl time.Duration) {
	r.capacityMu.Lock()
	defer r.capacityMu.Unlock()
	r.capacityTTL = ttl
	r.capacityValue = CapacitySnapshot{}
	r.capacityUntil = time.Time{}
	r.capacityValid = false
}

func (r *OperationsObservabilityRepo) AddProtocolMinute(value ProtocolMinute) error {
	_, err := r.db.Exec(`INSERT INTO protocol_metric_minute
		(bucket_start, request_count, download_count, failure_count, cache_hit_count, cache_miss_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_start) DO UPDATE SET
		request_count = request_count + excluded.request_count,
		download_count = download_count + excluded.download_count,
		failure_count = failure_count + excluded.failure_count,
		cache_hit_count = cache_hit_count + excluded.cache_hit_count,
		cache_miss_count = cache_miss_count + excluded.cache_miss_count`,
		value.BucketStart, value.RequestCount, value.DownloadCount, value.FailureCount, value.CacheHitCount, value.CacheMissCount)
	return err
}

func (r *OperationsObservabilityRepo) PutCapacitySnapshot(value CapacitySnapshot) error {
	_, err := r.db.Exec(`INSERT INTO capacity_snapshot_hour (bucket_start, repository_count, asset_count, logical_bytes)
		VALUES (?, ?, ?, ?) ON CONFLICT(bucket_start) DO UPDATE SET
		repository_count = excluded.repository_count, asset_count = excluded.asset_count, logical_bytes = excluded.logical_bytes`,
		value.BucketStart, value.RepositoryCount, value.AssetCount, value.LogicalBytes)
	return err
}

// hostSampleExecer 抽象「连接或事务」，让同一段 SQL 既能直连执行、也能在事务内执行。
type hostSampleExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func putHostSample(exec hostSampleExecer, value HostMetricSample) error {
	_, err := exec.Exec(`INSERT INTO host_metric_minute
		(bucket_start, host_state, host_error_code, cpu_percent, memory_total_bytes, memory_available_bytes, memory_used_bytes,
		disk_available_bytes, disk_total_bytes, disk_used_bytes,
		network_state, network_error_code, network_receive_bytes_per_sec, network_transmit_bytes_per_sec,
		network_receive_bytes_total, network_transmit_bytes_total,
		process_state, process_error_code, process_rss_bytes, process_cpu_percent, process_uptime_seconds, goroutine_count, open_file_descriptors,
		readiness_state, readiness_error_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_start) DO UPDATE SET
		host_state=excluded.host_state, host_error_code=excluded.host_error_code, cpu_percent=excluded.cpu_percent,
		memory_total_bytes=excluded.memory_total_bytes, memory_available_bytes=excluded.memory_available_bytes,
		memory_used_bytes=excluded.memory_used_bytes, disk_available_bytes=excluded.disk_available_bytes,
		disk_total_bytes=excluded.disk_total_bytes, disk_used_bytes=excluded.disk_used_bytes,
		network_state=excluded.network_state,
		network_error_code=excluded.network_error_code, network_receive_bytes_per_sec=excluded.network_receive_bytes_per_sec,
		network_transmit_bytes_per_sec=excluded.network_transmit_bytes_per_sec,
		network_receive_bytes_total=excluded.network_receive_bytes_total, network_transmit_bytes_total=excluded.network_transmit_bytes_total,
		process_state=excluded.process_state,
		process_error_code=excluded.process_error_code, process_rss_bytes=excluded.process_rss_bytes,
		process_cpu_percent=excluded.process_cpu_percent, process_uptime_seconds=excluded.process_uptime_seconds,
		goroutine_count=excluded.goroutine_count,
		open_file_descriptors=excluded.open_file_descriptors, readiness_state=excluded.readiness_state,
		readiness_error_code=excluded.readiness_error_code`,
		value.BucketStart, value.HostState, value.HostErrorCode, value.CPUPercent, value.MemoryTotalBytes, value.MemoryAvailableBytes, value.MemoryUsedBytes,
		value.DiskAvailableBytes, value.DiskTotalBytes, value.DiskUsedBytes,
		value.NetworkState, value.NetworkErrorCode, value.NetworkReceiveBytesPerSecond, value.NetworkTransmitBytesPerSecond,
		value.NetworkReceiveBytesTotal, value.NetworkTransmitBytesTotal,
		value.ProcessState, value.ProcessErrorCode, value.ProcessRSSBytes, value.ProcessCPUPercent, value.ProcessUptimeSeconds, value.GoroutineCount, value.OpenFileDescriptors,
		value.ReadinessState, value.ReadinessErrorCode)
	return err
}

// putHostNetworkInterfaces 逐网卡写入同一分钟的样本（主键 bucket_start + interface）。
// 空切片直接返回，避免发起无意义的事务/语句。
func putHostNetworkInterfaces(exec hostSampleExecer, values []HostNetworkInterfaceSample) error {
	for _, value := range values {
		if _, err := exec.Exec(`INSERT INTO host_network_interface_minute
			(bucket_start, interface, state, error_code, receive_bytes_total, transmit_bytes_total,
			receive_bytes_per_sec, transmit_bytes_per_sec)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(bucket_start, interface) DO UPDATE SET
			state=excluded.state, error_code=excluded.error_code,
			receive_bytes_total=excluded.receive_bytes_total, transmit_bytes_total=excluded.transmit_bytes_total,
			receive_bytes_per_sec=excluded.receive_bytes_per_sec, transmit_bytes_per_sec=excluded.transmit_bytes_per_sec`,
			value.BucketStart, value.Interface, value.State, value.ErrorCode,
			value.ReceiveBytesTotal, value.TransmitBytesTotal, value.ReceiveBytesPerSecond, value.TransmitBytesPerSecond); err != nil {
			return err
		}
	}
	return nil
}

func (r *OperationsObservabilityRepo) PutHostSample(value HostMetricSample) error {
	return putHostSample(r.db, value)
}

func (r *OperationsObservabilityRepo) PutHostNetworkInterfaces(values []HostNetworkInterfaceSample) error {
	return putHostNetworkInterfaces(r.db, values)
}

// PutHostSampleWithInterfaces 在同一事务里写总体样本与逐网卡明细：两张表要么一起生效、
// 要么都不生效。此前采集侧是两次独立调用，进程若在两次写之间退出（崩溃 / 断电 / 磁盘报错），
// 就会留下「总账有、明细无」的缺口，只能靠下一次采样自愈。
// 采集侧应走这个入口；单表方法保留给只需要其一的场景。
func (r *OperationsObservabilityRepo) PutHostSampleWithInterfaces(value HostMetricSample, interfaces []HostNetworkInterfaceSample) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := putHostSample(tx, value); err != nil {
		return err
	}
	if err := putHostNetworkInterfaces(tx, interfaces); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestHostNetworkInterfaces 返回窗口 [from,to) 内**每个网卡各自最新**的一行，
// 供网卡选择器与「各网卡累计总量」展示（每网卡一行，按网卡名升序）。
func (r *OperationsObservabilityRepo) LatestHostNetworkInterfaces(from, to time.Time) ([]HostNetworkInterfaceSample, error) {
	items := []HostNetworkInterfaceSample{}
	fromValue, toValue := formatMetricTime(from), formatMetricTime(to)
	err := r.db.Select(&items, `SELECT bucket_start, interface, state, error_code, receive_bytes_total, transmit_bytes_total,
		receive_bytes_per_sec, transmit_bytes_per_sec FROM host_network_interface_minute AS current
		WHERE current.bucket_start >= ? AND current.bucket_start < ?
		AND current.bucket_start = (
			SELECT MAX(bucket_start) FROM host_network_interface_minute AS latest
			WHERE latest.interface = current.interface AND latest.bucket_start >= ? AND latest.bucket_start < ?)
		ORDER BY current.interface`, fromValue, toValue, fromValue, toValue)
	return items, err
}

// HostNetworkInterfaceSamples 返回窗口 [from,to) 内指定网卡的逐分钟样本（按时间升序），
// 供 handler 在选定网卡时用该网卡的速率与总量覆盖聚合口径。
func (r *OperationsObservabilityRepo) HostNetworkInterfaceSamples(from, to time.Time, iface string) ([]HostNetworkInterfaceSample, error) {
	items := []HostNetworkInterfaceSample{}
	err := r.db.Select(&items, `SELECT bucket_start, interface, state, error_code, receive_bytes_total, transmit_bytes_total,
		receive_bytes_per_sec, transmit_bytes_per_sec FROM host_network_interface_minute
		WHERE bucket_start >= ? AND bucket_start < ? AND interface = ? ORDER BY bucket_start`,
		formatMetricTime(from), formatMetricTime(to), iface)
	return items, err
}

// LatestHostNetworkInterface 返回指定网卡的最新一行（不限窗口，与 LatestHostSample 同一最新语义）；
// 没有该网卡的样本时返回 ErrNotFound。
func (r *OperationsObservabilityRepo) LatestHostNetworkInterface(iface string) (*HostNetworkInterfaceSample, error) {
	var item HostNetworkInterfaceSample
	err := r.db.Get(&item, `SELECT bucket_start, interface, state, error_code, receive_bytes_total, transmit_bytes_total,
		receive_bytes_per_sec, transmit_bytes_per_sec FROM host_network_interface_minute
		WHERE interface = ? ORDER BY bucket_start DESC LIMIT 1`, iface)
	if err != nil {
		return nil, mapMetricNotFound(err)
	}
	return &item, nil
}

func (r *OperationsObservabilityRepo) ProtocolMinutes(from, to time.Time) ([]ProtocolMinute, error) {
	items := []ProtocolMinute{}
	err := r.db.Select(&items, `SELECT bucket_start, request_count, download_count, failure_count, cache_hit_count, cache_miss_count
		FROM protocol_metric_minute WHERE bucket_start >= ? AND bucket_start < ? ORDER BY bucket_start`, formatMetricTime(from), formatMetricTime(to))
	return items, err
}

func (r *OperationsObservabilityRepo) CapacitySnapshots(from, to time.Time) ([]CapacitySnapshot, error) {
	items := []CapacitySnapshot{}
	err := r.db.Select(&items, `SELECT bucket_start, repository_count, asset_count, logical_bytes
		FROM capacity_snapshot_hour WHERE bucket_start >= ? AND bucket_start < ? ORDER BY bucket_start`, formatMetricTime(from), formatMetricTime(to))
	return items, err
}

func (r *OperationsObservabilityRepo) HostSamples(from, to time.Time) ([]HostMetricSample, error) {
	items := []HostMetricSample{}
	err := r.db.Select(&items, `SELECT bucket_start, host_state, host_error_code, cpu_percent, memory_total_bytes, memory_available_bytes, memory_used_bytes,
		disk_available_bytes, disk_total_bytes, disk_used_bytes,
		network_state, network_error_code, network_receive_bytes_per_sec, network_transmit_bytes_per_sec,
		network_receive_bytes_total, network_transmit_bytes_total,
		process_state, process_error_code, process_rss_bytes, process_cpu_percent, process_uptime_seconds, goroutine_count, open_file_descriptors,
		readiness_state, readiness_error_code FROM host_metric_minute WHERE bucket_start >= ? AND bucket_start < ? ORDER BY bucket_start`, formatMetricTime(from), formatMetricTime(to))
	return items, err
}

func (r *OperationsObservabilityRepo) LatestHostSample() (*HostMetricSample, error) {
	var item HostMetricSample
	err := r.db.Get(&item, `SELECT bucket_start, host_state, host_error_code, cpu_percent, memory_total_bytes, memory_available_bytes, memory_used_bytes,
		disk_available_bytes, disk_total_bytes, disk_used_bytes,
		network_state, network_error_code, network_receive_bytes_per_sec, network_transmit_bytes_per_sec,
		network_receive_bytes_total, network_transmit_bytes_total,
		process_state, process_error_code, process_rss_bytes, process_cpu_percent, process_uptime_seconds, goroutine_count, open_file_descriptors,
		readiness_state, readiness_error_code FROM host_metric_minute ORDER BY bucket_start DESC LIMIT 1`)
	if err != nil {
		return nil, mapMetricNotFound(err)
	}
	return &item, nil
}

// CurrentCapacity 返回当前逻辑容量：仓库数、资产数、逻辑字节数。
// 三个字段的口径与历史上的单条 JOIN 聚合完全一致（见 aggregateCapacity）。
//
// 结果按实例的 capacityTTL（来自 JIAN_CAPACITY_CACHE_TTL_SECONDS）缓存：容量数字非授权
// 数据，短窗滞后可接受，而每次重算都要遍历整张 asset 表。缓存未命中时在锁外执行聚合——
// 锁只保护缓存字段的读写，不用于串行化查询（项目红线：持锁执行磁盘 IO）。
// 并发未命中最多让每个请求各算一次，结果幂等，不会写坏缓存。
// TTL <= 0 表示禁用缓存：每次调用都走这条完整的「聚合 → 返回」路径。
func (r *OperationsObservabilityRepo) CurrentCapacity() (CapacitySnapshot, error) {
	if value, ok := r.cachedCapacity(); ok {
		return value, nil
	}
	value, err := r.aggregateCapacity()
	if err != nil {
		// 失败不写缓存：不把一次查询错误固化成 TTL 内的「有效值」。
		return CapacitySnapshot{}, err
	}
	r.storeCapacity(value)
	return value, nil
}

// cachedCapacity 返回仍在有效期内的缓存值；过期、禁用缓存或从未填充时返回 false。
func (r *OperationsObservabilityRepo) cachedCapacity() (CapacitySnapshot, bool) {
	r.capacityMu.Lock()
	defer r.capacityMu.Unlock()
	// TTL <= 0 = 禁用缓存：直接判定未命中，不必读时钟也不必看是否有旧值。
	if r.capacityTTL <= 0 {
		return CapacitySnapshot{}, false
	}
	if !r.capacityValid || !r.capacityNow().Before(r.capacityUntil) {
		return CapacitySnapshot{}, false
	}
	return r.capacityValue, true
}

// storeCapacity 写入缓存并顺延有效期；禁用缓存时直接跳过，不保留任何「有效值」。
func (r *OperationsObservabilityRepo) storeCapacity(value CapacitySnapshot) {
	r.capacityMu.Lock()
	defer r.capacityMu.Unlock()
	if r.capacityTTL <= 0 {
		return
	}
	r.capacityValue = value
	r.capacityUntil = r.capacityNow().Add(r.capacityTTL)
	r.capacityValid = true
}

// aggregateCapacity 聚合当前逻辑容量。
//
// 历史实现是单条 repository LEFT JOIN asset：每个仓库按 idx_asset_repository 索引进
// asset 表逐行取数（随机页读），外加 COUNT(DISTINCT) 的临时 B 树。50 万行合成资产
// （约 282 MB）实测中位数约 8.5 s、最小 6.4 s，而 EXPLAIN QUERY PLAN 显示
// "SCAN repository" + "SEARCH asset USING INDEX idx_asset_repository" +
// "USE TEMP B-TREE FOR count(DISTINCT)"。
//
// 拆成两条单表语句后口径不变：
//   - repository_count：LEFT JOIN 保留了没有任何资产的仓库，COUNT(DISTINCT repository.id)
//     去重后每个仓库恰好计一次，等价于 repository 表的行数。
//   - asset_count / logical_bytes：asset.repository_id 是 NOT NULL 外键，指向 repository
//     的主键（唯一），任一资产行最多匹配一个仓库行，JOIN 不会放大行数，COUNT(asset.id)
//     与 COUNT(*) 同值、SUM(size) 也不受 JOIN 影响。因此可直接在 asset 上聚合
//     （执行计划退化为一次顺序 SCAN asset，无临时 B 树）。
//   - 唯一的口径差异是「孤儿资产」（repository_id 指向不存在的仓库）：旧写法不计、
//     新写法会计。连接 DSN 启用了 foreign_keys(ON)，且有 ON DELETE CASCADE，
//     孤儿行无法存在，故两种写法等价。
func (r *OperationsObservabilityRepo) aggregateCapacity() (CapacitySnapshot, error) {
	// 资产侧：一次顺序扫描同时得到资产数与逻辑字节数。
	var assets struct {
		AssetCount   int64 `db:"asset_count"`
		LogicalBytes int64 `db:"logical_bytes"`
	}
	if err := r.db.Get(&assets, `SELECT COUNT(*) AS asset_count, COALESCE(SUM(size),0) AS logical_bytes FROM asset`); err != nil {
		return CapacitySnapshot{}, err
	}
	// 仓库侧：覆盖索引上的计数，含没有任何资产的仓库。
	var repositoryCount int64
	if err := r.db.Get(&repositoryCount, `SELECT COUNT(*) FROM repository`); err != nil {
		return CapacitySnapshot{}, err
	}
	return CapacitySnapshot{
		RepositoryCount: repositoryCount,
		AssetCount:      assets.AssetCount,
		LogicalBytes:    assets.LogicalBytes,
	}, nil
}

func (r *OperationsObservabilityRepo) DeleteBefore(cutoff time.Time) error {
	value := formatMetricTime(cutoff)
	for _, table := range []string{"protocol_metric_minute", "capacity_snapshot_hour", "host_metric_minute", "host_network_interface_minute"} {
		if _, err := r.db.Exec("DELETE FROM "+table+" WHERE bucket_start < ?", value); err != nil {
			return err
		}
	}
	return nil
}

func formatMetricTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func mapMetricNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
