package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const maxOperationsRange = 30 * 24 * time.Hour

// GetOperationsDashboard 返回当前节点真实业务聚合；统计口径在服务端固定，前端不得复算。
func (h *Handlers) GetOperationsDashboard(c *gin.Context, params GetOperationsDashboardParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	from, to, ok := operationsRange(c, params.From, params.To)
	if !ok {
		return
	}
	if h.operationsObservability == nil {
		authWriteUnavailable(c)
		return
	}
	minutes, err := h.operationsObservability.ProtocolMinutes(from, to)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	capacity, err := h.operationsObservability.CapacitySnapshots(from, to)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	current, err := h.operationsObservability.CurrentCapacity()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	bucket := operationsBucket(from, to)
	downloadTrend, ok := h.assetDownloadTrend(c, from, to, bucket)
	if !ok {
		return
	}
	response := OperationsDashboard{
		From: from, To: to, EffectiveBucket: ObservabilityBucket(bucket),
		Current:       capacityPoint(current, time.Now().UTC().Truncate(time.Hour), time.Now().UTC()),
		Kpi:           dashboardKPI(current, minutes),
		RequestTrend:  aggregateProtocolMinutes(minutes, from, to, bucket),
		DownloadTrend: downloadTrend,
		CapacityTrend: aggregateCapacitySnapshots(capacity, from, to, bucket),
		Alerts:        h.operationsAlerts(),
	}
	c.JSON(http.StatusOK, response)
}

// assetDownloadTrend 查询并转换下载累计趋势（FR-143；原始口径，与 KPI 走同一采集点）。
// repo 未接线时返回空集——不伪造数据，也不因可选依赖缺失而整体 500。
func (h *Handlers) assetDownloadTrend(c *gin.Context, from, to time.Time, bucket string) ([]DownloadTrendPoint, bool) {
	points := make([]DownloadTrendPoint, 0)
	if h.assetDownloads == nil {
		return points, true
	}
	buckets, err := h.assetDownloads.DownloadTrend(from, to, bucket)
	if err != nil {
		writeDomainErr(c, err)
		return nil, false
	}
	for _, item := range buckets {
		start, parseErr := time.Parse(time.RFC3339, item.Bucket)
		if parseErr != nil {
			continue
		}
		points = append(points, DownloadTrendPoint{From: start, To: bucketEnd(start, bucket), DownloadCount: item.Count})
	}
	return points, true
}

// GetDownloadByClient 返回下载来源聚合（FR-144；独立来源口径：同 IP + 同制品 1 小时窗口
// 只计一次贡献）。IP 明文仅管理员可见；客户端类型为 UA 归类结果，不含原始 UA 串。
func (h *Handlers) GetDownloadByClient(c *gin.Context, params GetDownloadByClientParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	from, to, ok := operationsRange(c, params.From, params.To)
	if !ok {
		return
	}
	if h.assetDownloads == nil {
		authWriteUnavailable(c)
		return
	}
	const topIPs = 10
	ips, families, err := h.assetDownloads.DownloadClientRanking(from, to, topIPs)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	response := DownloadClientRanking{
		TopIps:   make([]DownloadIpCount, 0, len(ips)),
		Families: make([]DownloadFamilyCount, 0, len(families)),
	}
	for _, item := range ips {
		response.TopIps = append(response.TopIps, DownloadIpCount{Ip: item.IP, Count: item.Count})
	}
	for _, item := range families {
		response.Families = append(response.Families, DownloadFamilyCount{Family: item.Family, Count: item.Count})
	}
	c.JSON(http.StatusOK, response)
}

// dashboardKPI 统一计算范围内业务计数，避免每个客户端独立重算造成统计口径漂移。
func dashboardKPI(current repository.CapacitySnapshot, minutes []repository.ProtocolMinute) OperationsDashboardKpi {
	kpi := OperationsDashboardKpi{
		RepositoryCount: current.RepositoryCount,
		AssetCount:      current.AssetCount,
		LogicalBytes:    current.LogicalBytes,
	}
	var cacheHits, cacheTotal int64
	for _, item := range minutes {
		kpi.RequestCount += item.RequestCount
		kpi.DownloadCount += item.DownloadCount
		kpi.FailureCount += item.FailureCount
		cacheHits += item.CacheHitCount
		cacheTotal += item.CacheHitCount + item.CacheMissCount
	}
	if cacheTotal > 0 {
		rate := float64(cacheHits) / float64(cacheTotal)
		kpi.CacheHitRate = &rate
	}
	return kpi
}

// GetHostMonitoring 返回当前节点主机持久样本；首次采样前与过期样本明确区分。
func (h *Handlers) GetHostMonitoring(c *gin.Context, params GetHostMonitoringParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	from, to, ok := operationsRange(c, params.From, params.To)
	if !ok {
		return
	}
	if h.operationsObservability == nil {
		authWriteUnavailable(c)
		return
	}
	items, err := h.operationsObservability.HostSamples(from, to)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	bucket := operationsBucket(from, to)
	response := HostMonitoring{EffectiveBucket: ObservabilityBucket(bucket), Samples: aggregateHostSamples(items, from, to, bucket)}
	latest, err := h.operationsObservability.LatestHostSample()
	if errors.Is(err, repository.ErrNotFound) {
		response.HostState = HostMonitoringHostState("unknown")
		c.JSON(http.StatusOK, response)
		return
	}
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	latestAt, parseErr := time.Parse(time.RFC3339Nano, latest.BucketStart)
	if parseErr != nil {
		writeDomainErr(c, parseErr)
		return
	}
	response.LatestSampleAt = &latestAt
	latestPoint := hostPoint(*latest, latestAt, latestAt.Add(time.Minute))
	response.Latest = &latestPoint
	if time.Since(latestAt) > 2*time.Minute {
		response.HostState = HostMonitoringHostState("stale")
	} else {
		response.HostState = HostMonitoringHostState("healthy")
	}
	c.JSON(http.StatusOK, response)
}

func operationsRange(c *gin.Context, fromValue *ObservabilityFromParam, toValue *ObservabilityToParam) (time.Time, time.Time, bool) {
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now
	if (fromValue == nil) != (toValue == nil) {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "from 与 to 必须同时提供")
		return time.Time{}, time.Time{}, false
	}
	if fromValue != nil {
		from, to = fromValue.UTC(), toValue.UTC()
	}
	if !to.After(from) || to.Sub(from) > maxOperationsRange {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "时间范围无效或超过 30 天")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func operationsBucket(from, to time.Time) string {
	duration := to.Sub(from)
	if duration <= 24*time.Hour {
		return "minute"
	}
	if duration <= 7*24*time.Hour {
		return "hour"
	}
	return "day"
}

func aggregateProtocolMinutes(items []repository.ProtocolMinute, from, to time.Time, bucket string) []ProtocolMetricPoint {
	values := map[time.Time]ProtocolMetricPoint{}
	for _, item := range items {
		at, err := time.Parse(time.RFC3339Nano, item.BucketStart)
		if err != nil {
			continue
		}
		start := truncateOperationsBucket(at, bucket)
		point := values[start]
		point.From, point.To = start, bucketEnd(start, bucket)
		point.RequestCount += item.RequestCount
		point.DownloadCount += item.DownloadCount
		point.FailureCount += item.FailureCount
		point.CacheHitCount += item.CacheHitCount
		point.CacheMissCount += item.CacheMissCount
		values[start] = point
	}
	return orderedProtocolPoints(values, from, to)
}

func aggregateCapacitySnapshots(items []repository.CapacitySnapshot, from, to time.Time, bucket string) []CapacityPoint {
	values := map[time.Time]CapacityPoint{}
	for _, item := range items {
		at, err := time.Parse(time.RFC3339Nano, item.BucketStart)
		if err != nil {
			continue
		}
		start := truncateOperationsBucket(at, bucket)
		values[start] = capacityPoint(item, start, bucketEnd(start, bucket))
	}
	result := make([]CapacityPoint, 0, len(values))
	for _, point := range values {
		if point.To.After(from) && point.From.Before(to) {
			result = append(result, point)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].From.Before(result[j].From) })
	return result
}

func aggregateHostSamples(items []repository.HostMetricSample, from, to time.Time, bucket string) []HostMetricPoint {
	values := map[time.Time]HostMetricPoint{}
	for _, item := range items {
		at, err := time.Parse(time.RFC3339Nano, item.BucketStart)
		if err != nil {
			continue
		}
		start := truncateOperationsBucket(at, bucket)
		values[start] = hostPoint(item, start, bucketEnd(start, bucket))
	}
	result := make([]HostMetricPoint, 0, len(values))
	for _, point := range values {
		if point.To.After(from) && point.From.Before(to) {
			result = append(result, point)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].From.Before(result[j].From) })
	return result
}

func orderedProtocolPoints(values map[time.Time]ProtocolMetricPoint, from, to time.Time) []ProtocolMetricPoint {
	result := make([]ProtocolMetricPoint, 0, len(values))
	for _, point := range values {
		if point.To.After(from) && point.From.Before(to) {
			result = append(result, point)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].From.Before(result[j].From) })
	return result
}

func truncateOperationsBucket(at time.Time, bucket string) time.Time {
	at = at.UTC()
	switch bucket {
	case "hour":
		return at.Truncate(time.Hour)
	case "day":
		return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return at.Truncate(time.Minute)
	}
}

func bucketEnd(start time.Time, bucket string) time.Time {
	switch bucket {
	case "hour":
		return start.Add(time.Hour)
	case "day":
		return start.AddDate(0, 0, 1)
	default:
		return start.Add(time.Minute)
	}
}

func capacityPoint(item repository.CapacitySnapshot, from, to time.Time) CapacityPoint {
	return CapacityPoint{From: from, To: to, RepositoryCount: item.RepositoryCount, AssetCount: item.AssetCount, LogicalBytes: item.LogicalBytes}
}

func hostPoint(item repository.HostMetricSample, from, to time.Time) HostMetricPoint {
	return HostMetricPoint{From: from, To: to, HostState: metricGroup(item.HostState, item.HostErrorCode), CpuPercent: item.CPUPercent,
		MemoryTotalBytes: item.MemoryTotalBytes, MemoryAvailableBytes: item.MemoryAvailableBytes, DiskAvailableBytes: item.DiskAvailableBytes,
		NetworkState: metricGroup(item.NetworkState, item.NetworkErrorCode), NetworkReceiveBytesPerSecond: item.NetworkReceiveBytesPerSecond,
		NetworkTransmitBytesPerSecond: item.NetworkTransmitBytesPerSecond, ProcessState: metricGroup(item.ProcessState, item.ProcessErrorCode),
		ProcessRssBytes: item.ProcessRSSBytes, ProcessCpuPercent: item.ProcessCPUPercent, GoroutineCount: item.GoroutineCount,
		OpenFileDescriptors: item.OpenFileDescriptors, ReadinessState: metricGroup(item.ReadinessState, item.ReadinessErrorCode)}
}

func metricGroup(state repository.MetricState, code string) HostMetricGroup {
	value := HostMetricGroup{State: MetricGroupState(state)}
	if code != "" {
		value.ErrorCode = &code
	}
	return value
}

// operationsAlerts 返回当前节点运维告警；v0.8.0 起按 (code, source) 指纹去重持久化：
// 相同告警只存一份（保留首次发现时间、滚动最近观察时间），恢复后自动清除；
// upstream_auto_blocked 携带自动阻止窗口截止时间（blockedUntil）。
func (h *Handlers) operationsAlerts() []OperationsAlert {
	now := time.Now().UTC()
	active := h.currentOperationsAlerts(now)
	if h.operationsAlertStore == nil {
		return active
	}
	// 合并去重：已存在行保留 first_observed_at，未存在行以本次观察时间为首次。
	activeKeys := make(map[string]bool, len(active))
	rows := make([]repository.OperationsAlertRow, 0, len(active))
	for _, alert := range active {
		activeKeys[alertRowKey(alert.Code, alert.Source)] = true
		row := repository.OperationsAlertRow{
			Code:            alert.Code,
			Source:          alert.Source,
			Severity:        string(alert.Severity),
			FirstObservedAt: repository.FormatMetricTime(alert.ObservedAt),
			LastObservedAt:  repository.FormatMetricTime(alert.ObservedAt),
		}
		if alert.BlockedUntil != nil {
			value := repository.FormatMetricTime(*alert.BlockedUntil)
			row.BlockedUntil = &value
		}
		rows = append(rows, row)
	}
	if err := h.operationsAlertStore.UpsertActive(rows); err != nil {
		return active
	}
	if err := h.operationsAlertStore.RemoveRecovered(activeKeys); err != nil {
		return active
	}
	return h.persistedOperationsAlerts()
}

// currentOperationsAlerts 计算当前活跃告警（不持久化，供合并前使用）。
func (h *Handlers) currentOperationsAlerts(now time.Time) []OperationsAlert {
	alerts := []OperationsAlert{}
	if !h.ready() {
		alerts = append(alerts, OperationsAlert{Code: "instance_not_ready", Severity: OperationsAlertSeverity("critical"), Source: "readyz", ObservedAt: now})
	}
	// 复制退役：同步失败告警不再产生（sync_logs 不再有新数据）。
	if h.repos != nil && h.assets != nil {
		repositories, _, err := h.repos.List(1000, 0)
		if err == nil {
			for _, repo := range repositories {
				if repo.Type != "proxy" {
					continue
				}
				health := h.assets.Status(repo.ID)
				if health.Status != domain.StatusAutoBlocked {
					continue
				}
				alert := OperationsAlert{Code: "upstream_auto_blocked", Severity: OperationsAlertSeverity("warning"), Source: "repository:" + repo.Name, ObservedAt: now}
				if !health.BlockedUntil.IsZero() {
					blockedUntil := health.BlockedUntil
					alert.BlockedUntil = &blockedUntil
				}
				alerts = append(alerts, alert)
			}
		}
	}
	return alerts
}

// persistedOperationsAlerts 从去重持久化表读取告警，补齐 firstObservedAt / blockedUntil。
func (h *Handlers) persistedOperationsAlerts() []OperationsAlert {
	rows, err := h.operationsAlertStore.List()
	if err != nil {
		return []OperationsAlert{}
	}
	alerts := make([]OperationsAlert, 0, len(rows))
	for _, row := range rows {
		alert := OperationsAlert{
			Code:       row.Code,
			Severity:   OperationsAlertSeverity(row.Severity),
			Source:     row.Source,
			ObservedAt: parseObservedAt(&row.LastObservedAt, time.Now().UTC()),
		}
		if first, ok := parseObservedAtOpt(row.FirstObservedAt); ok {
			alert.FirstObservedAt = &first
		}
		if row.BlockedUntil != nil {
			if until, ok := parseObservedAtOpt(*row.BlockedUntil); ok {
				alert.BlockedUntil = &until
			}
		}
		alerts = append(alerts, alert)
	}
	return alerts
}

func alertRowKey(code, source string) string { return code + "\x00" + source }

func parseObservedAtOpt(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func parseObservedAt(value *string, fallback time.Time) time.Time {
	if value == nil {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return fallback
	}
	return parsed
}

func authWriteUnavailable(c *gin.Context) {
	auth.WriteError(c, http.StatusConflict, "conflict", "运维观测存储未就绪")
}
