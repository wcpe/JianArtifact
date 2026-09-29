package domain

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const operationsRetention = 30 * 24 * time.Hour

// ProtocolMetric 是 HTTP 边界记录的、已完成的制品协议请求结果。
// 它不携带路径、主体、地址或任何凭据。
type ProtocolMetric struct {
	CompletedAt time.Time
	Method      string
	Status      int
	CacheResult string
}

// OperationsDashboardService 负责把低开销协议计数器定期持久化为当前节点分钟聚合。
type OperationsDashboardService struct {
	repo    *repository.OperationsObservabilityRepo
	mu      sync.Mutex
	pending map[string]repository.ProtocolMinute
}

func NewOperationsDashboardService(repo *repository.OperationsObservabilityRepo) *OperationsDashboardService {
	return &OperationsDashboardService{repo: repo, pending: make(map[string]repository.ProtocolMinute)}
}

// RecordProtocol 只累计已完成协议请求；实际写入在分钟任务中完成，避免下载路径逐请求写 SQLite。
func (s *OperationsDashboardService) RecordProtocol(metric ProtocolMetric) {
	if s == nil || s.repo == nil {
		return
	}
	bucket := metric.CompletedAt.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	s.mu.Lock()
	item := s.pending[bucket]
	item.BucketStart = bucket
	item.RequestCount++
	if metric.Method == "GET" && (metric.Status == 200 || metric.Status == 206) {
		item.DownloadCount++
	}
	if metric.Status >= 500 && metric.Status <= 599 {
		item.FailureCount++
	}
	switch metric.CacheResult {
	case "hit":
		item.CacheHitCount++
	case "miss":
		item.CacheMissCount++
	}
	s.pending[bucket] = item
	s.mu.Unlock()
}

// Flush 将不晚于当前分钟的内存累计原子加到 SQLite；失败时保留计数等待下一次重试。
func (s *OperationsDashboardService) Flush(now time.Time) error {
	if s == nil || s.repo == nil {
		return nil
	}
	cutoff := now.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	s.mu.Lock()
	items := make([]repository.ProtocolMinute, 0, len(s.pending))
	for key, item := range s.pending {
		if key <= cutoff {
			items = append(items, item)
			delete(s.pending, key)
		}
	}
	s.mu.Unlock()
	for index, item := range items {
		if err := s.repo.AddProtocolMinute(item); err != nil {
			s.mu.Lock()
			for _, remaining := range items[index:] {
				current := s.pending[remaining.BucketStart]
				current.BucketStart = remaining.BucketStart
				current.RequestCount += remaining.RequestCount
				current.DownloadCount += remaining.DownloadCount
				current.FailureCount += remaining.FailureCount
				current.CacheHitCount += remaining.CacheHitCount
				current.CacheMissCount += remaining.CacheMissCount
				s.pending[remaining.BucketStart] = current
			}
			s.mu.Unlock()
			return err
		}
	}
	return nil
}

func (s *OperationsDashboardService) Snapshot(now time.Time) (repository.CapacitySnapshot, error) {
	if s == nil || s.repo == nil {
		return repository.CapacitySnapshot{}, repository.ErrNotFound
	}
	value, err := s.repo.CurrentCapacity()
	if err != nil {
		return repository.CapacitySnapshot{}, err
	}
	value.BucketStart = now.UTC().Truncate(time.Hour).Format(time.RFC3339Nano)
	return value, s.repo.PutCapacitySnapshot(value)
}

// Start 启动分钟落盘、小时容量快照与每日保留清理；启动本身不扫描或清理历史数据。
func (s *OperationsDashboardService) Start(ctx context.Context, now func() time.Time) {
	if s == nil || s.repo == nil {
		return
	}
	go func() {
		minute := time.NewTicker(time.Minute)
		hour := time.NewTicker(time.Hour)
		day := time.NewTicker(24 * time.Hour)
		defer minute.Stop()
		defer hour.Stop()
		defer day.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-minute.C:
				_ = s.Flush(now())
			case <-hour.C:
				_, _ = s.Snapshot(now())
			case <-day.C:
				_ = s.repo.DeleteBefore(now().UTC().Add(-operationsRetention))
			}
		}
	}()
}

// HostNetworkInterface 是单个网卡的累计计数（自网卡启动以来的字节数）。
// 它不含速率：速率由服务层按同名网卡的相邻样本差分得出。
type HostNetworkInterface struct {
	Name          string
	ReceiveBytes  uint64
	TransmitBytes uint64
}

// HostRawSample 是平台适配器返回的原始计数与当前值；速率由服务按相邻样本计算。
type HostRawSample struct {
	At                   time.Time
	HostState            repository.MetricState
	HostErrorCode        string
	CPUIdleTicks         uint64
	CPUTotalTicks        uint64
	MemoryTotalBytes     *int64
	MemoryAvailableBytes *int64
	DiskAvailableBytes   *int64
	DiskTotalBytes       *int64
	NetworkState         repository.MetricState
	NetworkErrorCode     string
	// NetworkInterfaces 是本次采样读到的逐网卡累计计数（跳过回环）。
	NetworkInterfaces []HostNetworkInterface
	// NetworkReceiveBytes / NetworkTransmitBytes 是 NetworkInterfaces 的求和，
	// 语义保持不变（全网卡聚合累计），向后兼容既有聚合速率逻辑。
	NetworkReceiveBytes  uint64
	NetworkTransmitBytes uint64
	ProcessState         repository.MetricState
	ProcessErrorCode     string
	ProcessRSSBytes      *int64
	ProcessCPUTicks      uint64
	ProcessUptimeSeconds *int64
	GoroutineCount       *int64
	OpenFileDescriptors  *int64
	ReadinessState       repository.MetricState
	ReadinessErrorCode   string
}

// HostCollector 仅采集当前主机和当前进程，不读取远程节点或环境变量。
type HostCollector interface{ Collect(time.Time) HostRawSample }

// HostMonitoringService 持久化当前节点主机采样并从相邻成功计数得出速率。
type HostMonitoringService struct {
	repo      *repository.OperationsObservabilityRepo
	collector HostCollector
	mu        sync.Mutex
	previous  *HostRawSample
}

func NewHostMonitoringService(repo *repository.OperationsObservabilityRepo, collector HostCollector) *HostMonitoringService {
	return &HostMonitoringService{repo: repo, collector: collector}
}

func (s *HostMonitoringService) Sample(now time.Time) (repository.HostMetricSample, error) {
	if s == nil || s.repo == nil || s.collector == nil {
		return repository.HostMetricSample{}, errors.New("主机监控未就绪")
	}
	raw := s.collector.Collect(now.UTC())
	item := hostSampleFromRaw(raw)
	s.mu.Lock()
	previous := s.previous
	var elapsed float64
	if previous != nil {
		elapsed = raw.At.Sub(previous.At).Seconds()
		if elapsed > 0 {
			item.CPUPercent = cpuPercent(raw.CPUIdleTicks, raw.CPUTotalTicks, previous.CPUIdleTicks, previous.CPUTotalTicks)
			if raw.NetworkState == repository.MetricStateOK && previous.NetworkState == repository.MetricStateOK {
				item.NetworkReceiveBytesPerSecond = bytesPerSecond(raw.NetworkReceiveBytes, previous.NetworkReceiveBytes, elapsed)
				item.NetworkTransmitBytesPerSecond = bytesPerSecond(raw.NetworkTransmitBytes, previous.NetworkTransmitBytes, elapsed)
			}
			if raw.ProcessState == repository.MetricStateOK && previous.ProcessState == repository.MetricStateOK {
				item.ProcessCPUPercent = processCPUPercent(raw.ProcessCPUTicks, previous.ProcessCPUTicks, elapsed)
			}
		}
	}
	interfaces := hostInterfaceSamplesFromRaw(item.BucketStart, raw, previous, elapsed)
	s.previous = &raw
	s.mu.Unlock()
	// 逐网卡行与聚合样本必须在同一事务内写入：两次独立写入之间若进程退出，
	// 会留下「总账有、明细无」的缺口，只能等下一次采样自愈。失败沿用同一错误语义
	// （调用方按分钟任务忽略并重试）。
	if err := s.repo.PutHostSampleWithInterfaces(item, interfaces); err != nil {
		return repository.HostMetricSample{}, err
	}
	return item, nil
}

func (s *HostMonitoringService) Start(ctx context.Context, now func() time.Time) {
	if s == nil || s.repo == nil || s.collector == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		cleanup := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		defer cleanup.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.Sample(now())
			case <-cleanup.C:
				_ = s.repo.DeleteBefore(now().UTC().Add(-operationsRetention))
			}
		}
	}()
}

func hostSampleFromRaw(raw HostRawSample) repository.HostMetricSample {
	item := repository.HostMetricSample{BucketStart: raw.At.UTC().Truncate(time.Minute).Format(time.RFC3339Nano), HostState: raw.HostState,
		HostErrorCode: raw.HostErrorCode, MemoryTotalBytes: raw.MemoryTotalBytes, MemoryAvailableBytes: raw.MemoryAvailableBytes,
		DiskAvailableBytes: raw.DiskAvailableBytes, DiskTotalBytes: raw.DiskTotalBytes, NetworkState: raw.NetworkState, NetworkErrorCode: raw.NetworkErrorCode,
		ProcessState: raw.ProcessState, ProcessErrorCode: raw.ProcessErrorCode, ProcessRSSBytes: raw.ProcessRSSBytes,
		ProcessUptimeSeconds: raw.ProcessUptimeSeconds,
		GoroutineCount:       raw.GoroutineCount, OpenFileDescriptors: raw.OpenFileDescriptors, ReadinessState: raw.ReadinessState,
		ReadinessErrorCode: raw.ReadinessErrorCode}
	// 内存/磁盘已用量在采样点直接按 total − available 给值：与总量同一采样同源，
	// 避免前端另算一套口径；任一端缺失则保持空值，由监控组状态解释而不是伪造零。
	if raw.MemoryTotalBytes != nil && raw.MemoryAvailableBytes != nil {
		used := *raw.MemoryTotalBytes - *raw.MemoryAvailableBytes
		item.MemoryUsedBytes = &used
	}
	if raw.DiskTotalBytes != nil && raw.DiskAvailableBytes != nil {
		used := *raw.DiskTotalBytes - *raw.DiskAvailableBytes
		item.DiskUsedBytes = &used
	}
	// 网络累计总量只在采集成功时落库（自网卡启动以来的聚合字节数）；采集失败保持 NULL，不伪造零。
	if raw.NetworkState == repository.MetricStateOK {
		receiveTotal, transmitTotal := int64(raw.NetworkReceiveBytes), int64(raw.NetworkTransmitBytes)
		item.NetworkReceiveBytesTotal = &receiveTotal
		item.NetworkTransmitBytesTotal = &transmitTotal
	}
	return item
}

// hostInterfaceSamplesFromRaw 把本次采样的逐网卡累计计数与上一份样本按**同名网卡**配对，
// 组装为逐网卡分钟行：累计总量原样落库，速率由同名网卡的相邻样本差分得出。
// 网卡新增（没有上一份同名样本）或时间未前进时速率留空，不伪造零；上一份里已消失的网卡不写行。
func hostInterfaceSamplesFromRaw(bucket string, raw HostRawSample, previous *HostRawSample, elapsed float64) []repository.HostNetworkInterfaceSample {
	if raw.NetworkState != repository.MetricStateOK {
		return nil
	}
	previousByName := make(map[string]HostNetworkInterface, 0)
	if previous != nil && previous.NetworkState == repository.MetricStateOK {
		for _, item := range previous.NetworkInterfaces {
			previousByName[item.Name] = item
		}
	}
	samples := make([]repository.HostNetworkInterfaceSample, 0, len(raw.NetworkInterfaces))
	for _, current := range raw.NetworkInterfaces {
		receiveTotal, transmitTotal := int64(current.ReceiveBytes), int64(current.TransmitBytes)
		sample := repository.HostNetworkInterfaceSample{
			BucketStart: bucket, Interface: current.Name, State: raw.NetworkState, ErrorCode: raw.NetworkErrorCode,
			ReceiveBytesTotal: &receiveTotal, TransmitBytesTotal: &transmitTotal,
		}
		if prev, ok := previousByName[current.Name]; ok && elapsed > 0 {
			sample.ReceiveBytesPerSecond = bytesPerSecond(current.ReceiveBytes, prev.ReceiveBytes, elapsed)
			sample.TransmitBytesPerSecond = bytesPerSecond(current.TransmitBytes, prev.TransmitBytes, elapsed)
		}
		samples = append(samples, sample)
	}
	return samples
}

func cpuPercent(idle, total, previousIdle, previousTotal uint64) *float64 {
	if total <= previousTotal || idle < previousIdle {
		return nil
	}
	value := 100 * (1 - float64(idle-previousIdle)/float64(total-previousTotal))
	return &value
}

func bytesPerSecond(value, previous uint64, elapsed float64) *float64 {
	if value < previous || elapsed <= 0 {
		return nil
	}
	result := float64(value-previous) / elapsed
	return &result
}

func processCPUPercent(value, previous uint64, elapsed float64) *float64 {
	if value < previous || elapsed <= 0 {
		return nil
	}
	result := 100 * float64(value-previous) / (elapsed * float64(clockTicksPerSecond()))
	return &result
}
