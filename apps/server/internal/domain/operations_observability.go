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
	NetworkState         repository.MetricState
	NetworkErrorCode     string
	NetworkReceiveBytes  uint64
	NetworkTransmitBytes uint64
	ProcessState         repository.MetricState
	ProcessErrorCode     string
	ProcessRSSBytes      *int64
	ProcessCPUTicks      uint64
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
	if previous != nil {
		elapsed := raw.At.Sub(previous.At).Seconds()
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
	s.previous = &raw
	s.mu.Unlock()
	if err := s.repo.PutHostSample(item); err != nil {
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
	return repository.HostMetricSample{BucketStart: raw.At.UTC().Truncate(time.Minute).Format(time.RFC3339Nano), HostState: raw.HostState,
		HostErrorCode: raw.HostErrorCode, MemoryTotalBytes: raw.MemoryTotalBytes, MemoryAvailableBytes: raw.MemoryAvailableBytes,
		DiskAvailableBytes: raw.DiskAvailableBytes, NetworkState: raw.NetworkState, NetworkErrorCode: raw.NetworkErrorCode,
		ProcessState: raw.ProcessState, ProcessErrorCode: raw.ProcessErrorCode, ProcessRSSBytes: raw.ProcessRSSBytes,
		GoroutineCount: raw.GoroutineCount, OpenFileDescriptors: raw.OpenFileDescriptors, ReadinessState: raw.ReadinessState,
		ReadinessErrorCode: raw.ReadinessErrorCode}
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
