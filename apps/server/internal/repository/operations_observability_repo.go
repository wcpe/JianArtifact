package repository

import (
	"database/sql"
	"errors"
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
	BucketStart                   string      `db:"bucket_start"`
	HostState                     MetricState `db:"host_state"`
	HostErrorCode                 string      `db:"host_error_code"`
	CPUPercent                    *float64    `db:"cpu_percent"`
	MemoryTotalBytes              *int64      `db:"memory_total_bytes"`
	MemoryAvailableBytes          *int64      `db:"memory_available_bytes"`
	DiskAvailableBytes            *int64      `db:"disk_available_bytes"`
	NetworkState                  MetricState `db:"network_state"`
	NetworkErrorCode              string      `db:"network_error_code"`
	NetworkReceiveBytesPerSecond  *float64    `db:"network_receive_bytes_per_sec"`
	NetworkTransmitBytesPerSecond *float64    `db:"network_transmit_bytes_per_sec"`
	ProcessState                  MetricState `db:"process_state"`
	ProcessErrorCode              string      `db:"process_error_code"`
	ProcessRSSBytes               *int64      `db:"process_rss_bytes"`
	ProcessCPUPercent             *float64    `db:"process_cpu_percent"`
	GoroutineCount                *int64      `db:"goroutine_count"`
	OpenFileDescriptors           *int64      `db:"open_file_descriptors"`
	ReadinessState                MetricState `db:"readiness_state"`
	ReadinessErrorCode            string      `db:"readiness_error_code"`
}

// OperationsObservabilityRepo 保存当前节点的可视化聚合；它从不参与复制。
type OperationsObservabilityRepo struct{ db *persistence.DB }

func NewOperationsObservabilityRepo(db *persistence.DB) *OperationsObservabilityRepo {
	return &OperationsObservabilityRepo{db: db}
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

func (r *OperationsObservabilityRepo) PutHostSample(value HostMetricSample) error {
	_, err := r.db.Exec(`INSERT INTO host_metric_minute
		(bucket_start, host_state, host_error_code, cpu_percent, memory_total_bytes, memory_available_bytes, disk_available_bytes,
		network_state, network_error_code, network_receive_bytes_per_sec, network_transmit_bytes_per_sec,
		process_state, process_error_code, process_rss_bytes, process_cpu_percent, goroutine_count, open_file_descriptors,
		readiness_state, readiness_error_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_start) DO UPDATE SET
		host_state=excluded.host_state, host_error_code=excluded.host_error_code, cpu_percent=excluded.cpu_percent,
		memory_total_bytes=excluded.memory_total_bytes, memory_available_bytes=excluded.memory_available_bytes,
		disk_available_bytes=excluded.disk_available_bytes, network_state=excluded.network_state,
		network_error_code=excluded.network_error_code, network_receive_bytes_per_sec=excluded.network_receive_bytes_per_sec,
		network_transmit_bytes_per_sec=excluded.network_transmit_bytes_per_sec, process_state=excluded.process_state,
		process_error_code=excluded.process_error_code, process_rss_bytes=excluded.process_rss_bytes,
		process_cpu_percent=excluded.process_cpu_percent, goroutine_count=excluded.goroutine_count,
		open_file_descriptors=excluded.open_file_descriptors, readiness_state=excluded.readiness_state,
		readiness_error_code=excluded.readiness_error_code`,
		value.BucketStart, value.HostState, value.HostErrorCode, value.CPUPercent, value.MemoryTotalBytes, value.MemoryAvailableBytes, value.DiskAvailableBytes,
		value.NetworkState, value.NetworkErrorCode, value.NetworkReceiveBytesPerSecond, value.NetworkTransmitBytesPerSecond,
		value.ProcessState, value.ProcessErrorCode, value.ProcessRSSBytes, value.ProcessCPUPercent, value.GoroutineCount, value.OpenFileDescriptors,
		value.ReadinessState, value.ReadinessErrorCode)
	return err
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
	err := r.db.Select(&items, `SELECT bucket_start, host_state, host_error_code, cpu_percent, memory_total_bytes, memory_available_bytes, disk_available_bytes,
		network_state, network_error_code, network_receive_bytes_per_sec, network_transmit_bytes_per_sec,
		process_state, process_error_code, process_rss_bytes, process_cpu_percent, goroutine_count, open_file_descriptors,
		readiness_state, readiness_error_code FROM host_metric_minute WHERE bucket_start >= ? AND bucket_start < ? ORDER BY bucket_start`, formatMetricTime(from), formatMetricTime(to))
	return items, err
}

func (r *OperationsObservabilityRepo) LatestHostSample() (*HostMetricSample, error) {
	var item HostMetricSample
	err := r.db.Get(&item, `SELECT bucket_start, host_state, host_error_code, cpu_percent, memory_total_bytes, memory_available_bytes, disk_available_bytes,
		network_state, network_error_code, network_receive_bytes_per_sec, network_transmit_bytes_per_sec,
		process_state, process_error_code, process_rss_bytes, process_cpu_percent, goroutine_count, open_file_descriptors,
		readiness_state, readiness_error_code FROM host_metric_minute ORDER BY bucket_start DESC LIMIT 1`)
	if err != nil {
		return nil, mapMetricNotFound(err)
	}
	return &item, nil
}

func (r *OperationsObservabilityRepo) CurrentCapacity() (CapacitySnapshot, error) {
	var item CapacitySnapshot
	// DISTINCT 防止 LEFT JOIN 资产行放大仓库计数（每仓库按资产数重复统计）。
	err := r.db.Get(&item, `SELECT COUNT(DISTINCT repository.id) AS repository_count, COALESCE(SUM(size),0) AS logical_bytes, COUNT(asset.id) AS asset_count
		FROM repository LEFT JOIN asset ON asset.repository_id = repository.id`)
	return item, err
}

func (r *OperationsObservabilityRepo) DeleteBefore(cutoff time.Time) error {
	value := formatMetricTime(cutoff)
	for _, table := range []string{"protocol_metric_minute", "capacity_snapshot_hour", "host_metric_minute"} {
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
