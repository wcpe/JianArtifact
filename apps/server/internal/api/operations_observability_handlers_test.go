package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestDashboardKPIIsServerOwned(t *testing.T) {
	current := repository.CapacitySnapshot{RepositoryCount: 3, AssetCount: 8, LogicalBytes: 4096}
	minutes := []repository.ProtocolMinute{
		{BucketStart: time.Now().UTC().Format(time.RFC3339Nano), RequestCount: 12, DownloadCount: 7, FailureCount: 2, CacheHitCount: 9, CacheMissCount: 3},
		{BucketStart: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), RequestCount: 5, DownloadCount: 4, FailureCount: 1},
	}

	kpi := dashboardKPI(current, minutes)
	if kpi.RepositoryCount != 3 || kpi.AssetCount != 8 || kpi.LogicalBytes != 4096 {
		t.Fatalf("容量 KPI 不正确：%+v", kpi)
	}
	if kpi.RequestCount != 17 || kpi.DownloadCount != 11 || kpi.FailureCount != 3 {
		t.Fatalf("协议 KPI 不正确：%+v", kpi)
	}
	if kpi.CacheHitRate == nil || math.Abs(*kpi.CacheHitRate-0.75) > 0.000001 {
		t.Fatalf("缓存命中率不正确：%+v", kpi.CacheHitRate)
	}
}

func TestDashboardKPIReturnsNilCacheHitRateWithoutCacheSamples(t *testing.T) {
	kpi := dashboardKPI(repository.CapacitySnapshot{}, []repository.ProtocolMinute{{RequestCount: 1}})
	if kpi.CacheHitRate != nil {
		t.Fatalf("无缓存样本时命中率必须为空：%+v", *kpi.CacheHitRate)
	}
}

func TestGetOperationsDashboardReturnsServerKPI(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	db := newObservabilityDB(t)
	metrics := repository.NewOperationsObservabilityRepo(db)
	now := time.Now().UTC().Truncate(time.Minute)
	if err := metrics.AddProtocolMinute(repository.ProtocolMinute{
		BucketStart: now.Format(time.RFC3339Nano), RequestCount: 9, DownloadCount: 4, FailureCount: 2, CacheHitCount: 6, CacheMissCount: 2,
	}); err != nil {
		t.Fatalf("写入协议计数：%v", err)
	}
	handlers := NewHandlers(Deps{OperationsObservability: metrics})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboard", nil)
	context.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
	from, to := now.Add(-time.Minute), now.Add(time.Minute)
	handlers.GetOperationsDashboard(context, GetOperationsDashboardParams{From: &from, To: &to})
	if recorder.Code != http.StatusOK {
		t.Fatalf("仪表盘应返回 200，得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		KPI OperationsDashboardKpi `json:"kpi"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if body.KPI.RequestCount != 9 || body.KPI.DownloadCount != 4 || body.KPI.FailureCount != 2 {
		t.Fatalf("响应未返回服务端 KPI：%+v", body.KPI)
	}
	if body.KPI.CacheHitRate == nil || math.Abs(*body.KPI.CacheHitRate-0.75) > 0.000001 {
		t.Fatalf("响应缓存命中率不正确：%+v", body.KPI.CacheHitRate)
	}
}

func TestGetOperationsDashboardRejectsUnauthenticatedRequest(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/observability/dashboard", nil)
	NewHandlers(Deps{}).GetOperationsDashboard(context, GetOperationsDashboardParams{})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未认证读取仪表盘应返回 401，得 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// newObservabilityDB 打开一个临时目录下的 SQLite 库并完成迁移，供可观测性处理器测试使用。
// 原定义位于已退役的 cluster_observability_handlers_test.go，现迁移至此处（dashboard 测试为其唯一使用者）。
func newObservabilityDB(t *testing.T) *persistence.DB {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "observability.db"))
	if err != nil {
		t.Fatalf("打开可观测性数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移可观测性数据库：%v", err)
	}
	return db
}

// readHostMonitoring 以管理员身份调用 GetHostMonitoring 并解码响应。
func readHostMonitoring(t *testing.T, handlers *Handlers, from, to time.Time, iface *HostInterfaceParam) HostMonitoring {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/observability/host", nil)
	context.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
	handlers.GetHostMonitoring(context, GetHostMonitoringParams{From: &from, To: &to, Interface: iface})
	if recorder.Code != http.StatusOK {
		t.Fatalf("主机监控应返回 200，得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body HostMonitoring
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	return body
}

// TestGetHostMonitoringExposesNetworkTotalsAndInterfaceFilter 覆盖网络总发送/总接收与网卡选择：
// 未指定网卡时按全部非回环网卡聚合，并返回可用网卡列表与各网卡累计总量；
// 指定网卡时趋势点与 latest 的速率与总量都换成该网卡；未知网卡留空（不沿用聚合值、不伪造 0）。
func TestGetHostMonitoringExposesNetworkTotalsAndInterfaceFilter(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	db := newObservabilityDB(t)
	metrics := repository.NewOperationsObservabilityRepo(db)
	bucket := time.Now().UTC().Truncate(time.Minute)
	aggregateReceive, aggregateTransmit := int64(1000), int64(2000)
	aggregateReceiveRate, aggregateTransmitRate := 50.0, 25.0
	if err := metrics.PutHostSample(repository.HostMetricSample{
		BucketStart: bucket.Format(time.RFC3339Nano), HostState: repository.MetricStateOK,
		NetworkState: repository.MetricStateOK, ProcessState: repository.MetricStateOK, ReadinessState: repository.MetricStateOK,
		NetworkReceiveBytesTotal: &aggregateReceive, NetworkTransmitBytesTotal: &aggregateTransmit,
		NetworkReceiveBytesPerSecond: &aggregateReceiveRate, NetworkTransmitBytesPerSecond: &aggregateTransmitRate,
	}); err != nil {
		t.Fatalf("写入主机样本：%v", err)
	}
	ethReceive, ethTransmit := int64(600), int64(900)
	wlanReceive, wlanTransmit := int64(400), int64(1100)
	ethReceiveRate, ethTransmitRate := 8.0, 4.0
	if err := metrics.PutHostNetworkInterfaces([]repository.HostNetworkInterfaceSample{
		{BucketStart: bucket.Format(time.RFC3339Nano), Interface: "eth0", State: repository.MetricStateOK,
			ReceiveBytesTotal: &ethReceive, TransmitBytesTotal: &ethTransmit,
			ReceiveBytesPerSecond: &ethReceiveRate, TransmitBytesPerSecond: &ethTransmitRate},
		{BucketStart: bucket.Format(time.RFC3339Nano), Interface: "wlan0", State: repository.MetricStateOK,
			ReceiveBytesTotal: &wlanReceive, TransmitBytesTotal: &wlanTransmit},
	}); err != nil {
		t.Fatalf("写入逐网卡样本：%v", err)
	}
	handlers := NewHandlers(Deps{OperationsObservability: metrics})
	from, to := bucket.Add(-time.Minute), bucket.Add(2*time.Minute)

	// 未指定网卡：总量与速率按全部非回环网卡聚合，并返回可用网卡列表与各网卡累计总量。
	aggregate := readHostMonitoring(t, handlers, from, to, nil)
	if len(aggregate.Samples) != 1 {
		t.Fatalf("样本数 = %d，期望 1：%+v", len(aggregate.Samples), aggregate.Samples)
	}
	if aggregate.Samples[0].NetworkReceiveBytesTotal == nil || *aggregate.Samples[0].NetworkReceiveBytesTotal != aggregateReceive ||
		aggregate.Samples[0].NetworkTransmitBytesTotal == nil || *aggregate.Samples[0].NetworkTransmitBytesTotal != aggregateTransmit {
		t.Fatalf("聚合累计总量错误：%+v", aggregate.Samples[0])
	}
	if aggregate.Samples[0].NetworkReceiveBytesPerSecond == nil || *aggregate.Samples[0].NetworkReceiveBytesPerSecond != aggregateReceiveRate {
		t.Fatalf("聚合速率错误：%+v", aggregate.Samples[0])
	}
	if aggregate.NetworkInterfaces == nil || len(*aggregate.NetworkInterfaces) != 2 {
		t.Fatalf("网卡列表缺失：%+v", aggregate.NetworkInterfaces)
	}
	options := map[string]HostNetworkInterface{}
	for _, item := range *aggregate.NetworkInterfaces {
		options[item.Name] = item
	}
	ethOption, ethOK := options["eth0"]
	if !ethOK || ethOption.ReceiveBytesTotal == nil || *ethOption.ReceiveBytesTotal != ethReceive ||
		ethOption.TransmitBytesTotal == nil || *ethOption.TransmitBytesTotal != ethTransmit {
		t.Fatalf("eth0 选择器条目错误：%+v", ethOption)
	}

	// 指定网卡：速率与总量都换成该网卡的取值（趋势点与 latest 一致）。
	selected := HostInterfaceParam("eth0")
	filtered := readHostMonitoring(t, handlers, from, to, &selected)
	if filtered.Samples[0].NetworkReceiveBytesTotal == nil || *filtered.Samples[0].NetworkReceiveBytesTotal != ethReceive ||
		filtered.Samples[0].NetworkReceiveBytesPerSecond == nil || *filtered.Samples[0].NetworkReceiveBytesPerSecond != ethReceiveRate {
		t.Fatalf("选定网卡的趋势点未按该网卡：%+v", filtered.Samples[0])
	}
	if filtered.Latest == nil || filtered.Latest.NetworkReceiveBytesTotal == nil || *filtered.Latest.NetworkReceiveBytesTotal != ethReceive {
		t.Fatalf("选定网卡的 latest 未按该网卡：%+v", filtered.Latest)
	}

	// 未知网卡：不沿用聚合值、也不伪造 0，而是留空；网卡列表仍列出全部可用网卡。
	ghost := HostInterfaceParam("ghost0")
	unknown := readHostMonitoring(t, handlers, from, to, &ghost)
	if unknown.Samples[0].NetworkReceiveBytesTotal != nil || unknown.Samples[0].NetworkReceiveBytesPerSecond != nil ||
		unknown.Latest.NetworkReceiveBytesTotal != nil {
		t.Fatalf("未知网卡必须留空而不是沿用聚合值：%+v", unknown.Samples[0])
	}
	if unknown.NetworkInterfaces == nil || len(*unknown.NetworkInterfaces) != 2 {
		t.Fatalf("未知网卡时仍应列出可用网卡：%+v", unknown.NetworkInterfaces)
	}
}

// TestGetHostMonitoringOmitsNetworkInterfacesWithoutData 验证无网卡样本时响应省略网卡列表，
// 且采集失败样本的总量为 null 而不是伪造的 0。
func TestGetHostMonitoringOmitsNetworkInterfacesWithoutData(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	db := newObservabilityDB(t)
	metrics := repository.NewOperationsObservabilityRepo(db)
	bucket := time.Now().UTC().Truncate(time.Minute)
	if err := metrics.PutHostSample(repository.HostMetricSample{
		BucketStart: bucket.Format(time.RFC3339Nano), HostState: repository.MetricStateOK,
		NetworkState: repository.MetricStateUnavailable, NetworkErrorCode: "network_unavailable",
		ProcessState: repository.MetricStateOK, ReadinessState: repository.MetricStateOK,
	}); err != nil {
		t.Fatalf("写入主机样本：%v", err)
	}
	handlers := NewHandlers(Deps{OperationsObservability: metrics})
	body := readHostMonitoring(t, handlers, bucket.Add(-time.Minute), bucket.Add(2*time.Minute), nil)
	if body.NetworkInterfaces != nil {
		t.Fatalf("无网卡数据时应省略网卡列表：%+v", body.NetworkInterfaces)
	}
	if body.Samples[0].NetworkReceiveBytesTotal != nil || body.Samples[0].NetworkTransmitBytesTotal != nil {
		t.Fatalf("采集失败时总量必须为 null：%+v", body.Samples[0])
	}
}
