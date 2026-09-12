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
