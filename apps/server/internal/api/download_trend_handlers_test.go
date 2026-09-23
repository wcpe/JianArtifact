package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// downloadTrendBase 固定样本时间：2026-09-21 10:00 UTC。
var downloadTrendBase = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

// newDownloadTrendTestHandlers 组装趋势端点测试环境：仓库服务（含 public/private 各一）
// 与下载计量仓储。匿名访问全局开关走默认开启（键缺失视为开启，FR-66）。
func newDownloadTrendTestHandlers(t *testing.T) (*Handlers, *repository.AssetDownloadRepo) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	downloads := repository.NewAssetDownloadRepo(db)
	repoSvc := domain.NewRepositoryService(
		repository.NewRepoRepo(db),
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
	for _, spec := range []struct{ name, visibility string }{
		{"trend-public", "public"},
		{"trend-private", "private"},
	} {
		if _, err := repoSvc.Create(spec.name, "raw", "hosted", spec.visibility, "", repository.RepositoryConfig{}); err != nil {
			t.Fatalf("创建仓库 %s：%v", spec.name, err)
		}
	}
	// 样本：public 两分钟（maven@1.1.1.1=2、curl@2.2.2.2=3），private 一分钟（maven@1.1.1.1=5）。
	if err := downloads.AddMinutes([]repository.AssetDownloadMinute{
		{BucketStart: repository.FormatMetricTime(downloadTrendBase), Repo: "trend-public", AssetPath: "a.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 2},
		{BucketStart: repository.FormatMetricTime(downloadTrendBase.Add(time.Minute)), Repo: "trend-public", AssetPath: "a.jar", ClientIP: "2.2.2.2", UAFamily: "curl", DownloadCount: 3},
		{BucketStart: repository.FormatMetricTime(downloadTrendBase), Repo: "trend-private", AssetPath: "b.jar", ClientIP: "1.1.1.1", UAFamily: "maven", DownloadCount: 5},
	}); err != nil {
		t.Fatalf("写入下载样本：%v", err)
	}
	return NewHandlers(Deps{Repos: repoSvc, AssetDownloads: downloads}), downloads
}

// serveDownloadTrend 以给定主体（nil 为匿名）经路由发起 GET；principal 中间件模拟
// 鉴权层已注入的主体，与生产 authenticator.Optional() 行为一致。
// 分组趋势已收口进契约：经 ServerInterfaceWrapper 注册，走生成的参数绑定链路
// （ErrorHandler 与生产 RegisterHandlersWithOptions 的默认实现一致），保证测试口径即线上口径。
func serveDownloadTrend(h *Handlers, principal *auth.Principal, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: h,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.GET("/api/v1/observability/downloads/trend", siw.GetDownloadTrendGrouped)
	router.GET("/api/v1/repositories/:name/download-trend", func(c *gin.Context) { h.GetRepositoryDownloadTrend(c) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func adminPrincipal() *auth.Principal {
	return &auth.Principal{Role: "admin", Username: "admin", UserID: 1}
}

func userPrincipal() *auth.Principal {
	return &auth.Principal{Role: "user", Username: "bob", UserID: 2}
}

// 分组趋势权限：IP 明文仅管理员 —— 未认证 401、非管理员 403（与 by-client 同边界）。
func TestGetDownloadTrendGroupedRequiresAdmin(t *testing.T) {
	handlers, _ := newDownloadTrendTestHandlers(t)
	const path = "/api/v1/observability/downloads/trend?from=2026-09-21T09:59:00Z&to=2026-09-21T10:30:00Z"

	if rec := serveDownloadTrend(handlers, nil, path); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名访问分组趋势应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveDownloadTrend(handlers, userPrincipal(), path); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员访问分组趋势应 403，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 分组趋势响应：缺省按 UA 族分组并给出降序总计（饼图）、ip 分组、repo 过滤、
// 非法 groupBy 400、空区间返回空序列。
func TestGetDownloadTrendGroupedReturnsSeriesAndTotals(t *testing.T) {
	handlers, _ := newDownloadTrendTestHandlers(t)
	admin := adminPrincipal()
	const base = "/api/v1/observability/downloads/trend?from=2026-09-21T09:59:00Z&to=2026-09-21T10:30:00Z"

	// 缺省 family 分组：全局 10:00 maven=7（2+5）、10:01 curl=3；totals 按计数降序。
	rec := serveDownloadTrend(handlers, admin, base)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员分组趋势应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var familyResp DownloadGroupedTrendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &familyResp); err != nil {
		t.Fatalf("解析分组趋势：%v", err)
	}
	if familyResp.GroupBy != "family" || familyResp.EffectiveBucket != ObservabilityBucket("minute") {
		t.Fatalf("分组维度或桶粒度错误：%+v", familyResp)
	}
	if len(familyResp.Points) != 2 {
		t.Fatalf("缺省分组应 2 个时序点，得 %d：%+v", len(familyResp.Points), familyResp.Points)
	}
	first := familyResp.Points[0]
	if !first.From.Equal(downloadTrendBase) || !first.To.Equal(downloadTrendBase.Add(time.Minute)) ||
		first.Group != "maven" || first.Count != 7 {
		t.Fatalf("首个时序点应 (10:00~10:01, maven, 7)，得 %+v", first)
	}
	if second := familyResp.Points[1]; second.Group != "curl" || second.Count != 3 ||
		!second.From.Equal(downloadTrendBase.Add(time.Minute)) {
		t.Fatalf("第二个时序点应 (10:01~10:02, curl, 3)，得 %+v", second)
	}
	if len(familyResp.Totals) != 2 || familyResp.Totals[0].Group != "maven" || familyResp.Totals[0].Count != 7 ||
		familyResp.Totals[1].Group != "curl" || familyResp.Totals[1].Count != 3 {
		t.Fatalf("分组总计应按计数降序：%+v", familyResp.Totals)
	}

	// ip 分组：10:00 1.1.1.1=7、10:01 2.2.2.2=3。
	rec = serveDownloadTrend(handlers, admin, base+"&groupBy=ip")
	if rec.Code != http.StatusOK {
		t.Fatalf("ip 分组应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var ipResp DownloadGroupedTrendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ipResp); err != nil {
		t.Fatalf("解析 ip 分组：%v", err)
	}
	if ipResp.GroupBy != "ip" || len(ipResp.Points) != 2 ||
		ipResp.Points[0].Group != "1.1.1.1" || ipResp.Points[0].Count != 7 {
		t.Fatalf("ip 分组结果错误：%+v", ipResp)
	}

	// repo 过滤：仅 private 仓的 10:00 maven=5。
	rec = serveDownloadTrend(handlers, admin, base+"&repo=trend-private")
	var repoResp DownloadGroupedTrendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &repoResp); err != nil {
		t.Fatalf("解析 repo 过滤：%v", err)
	}
	if len(repoResp.Points) != 1 || repoResp.Points[0].Count != 5 || repoResp.Points[0].Group != "maven" {
		t.Fatalf("repo 过滤应仅 private 仓一行：%+v", repoResp.Points)
	}

	// 非法 groupBy → 400。
	if rec := serveDownloadTrend(handlers, admin, base+"&groupBy=path"); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 groupBy 应 400，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 空区间（数据之外）→ 空序列（JSON 空数组而非 null）。
	rec = serveDownloadTrend(handlers, admin,
		"/api/v1/observability/downloads/trend?from=2026-09-21T12:00:00Z&to=2026-09-21T13:00:00Z")
	var emptyResp DownloadGroupedTrendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &emptyResp); err != nil {
		t.Fatalf("解析空区间：%v", err)
	}
	if emptyResp.Points == nil || len(emptyResp.Points) != 0 || emptyResp.Totals == nil || len(emptyResp.Totals) != 0 {
		t.Fatalf("空区间应返回空数组：%s", rec.Body.String())
	}
}

// 仓库详情下载趋势权限：与树同走 requireRepoRead —— 匿名可读 public 仓、
// 匿名读 private 仓 401、已认证无授权读 private 仓 403、不存在 404。
func TestGetRepositoryDownloadTrendPermissions(t *testing.T) {
	handlers, _ := newDownloadTrendTestHandlers(t)
	const window = "?from=2026-09-21T09:59:00Z&to=2026-09-21T10:02:00Z"

	if rec := serveDownloadTrend(handlers, adminPrincipal(), "/api/v1/repositories/trend-private/download-trend"+window); rec.Code != http.StatusOK {
		t.Fatalf("管理员读 private 仓趋势应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 仓库详情页匿名也可见制品树（FR-66 开关默认开 + public 仓），下载趋势同权限。
	if rec := serveDownloadTrend(handlers, nil, "/api/v1/repositories/trend-public/download-trend"+window); rec.Code != http.StatusOK {
		t.Fatalf("匿名读 public 仓趋势应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveDownloadTrend(handlers, nil, "/api/v1/repositories/trend-private/download-trend"+window); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读 private 仓趋势应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveDownloadTrend(handlers, userPrincipal(), "/api/v1/repositories/trend-private/download-trend"+window); rec.Code != http.StatusForbidden {
		t.Fatalf("无授权用户读 private 仓趋势应 403，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveDownloadTrend(handlers, adminPrincipal(), "/api/v1/repositories/missing/download-trend"+window); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的仓库应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 仓库详情下载趋势响应：补零连续桶（桶标签=桶起点）、仓库总下载、空区间全 0 补零。
func TestGetRepositoryDownloadTrendZeroFillsAndTotals(t *testing.T) {
	handlers, _ := newDownloadTrendTestHandlers(t)
	admin := adminPrincipal()

	// private 仓：10:00 有 5 次；窗口 [09:59, 10:02) 对齐出 3 个分钟桶 [0,5,0]。
	rec := serveDownloadTrend(handlers, admin,
		"/api/v1/repositories/trend-private/download-trend?from=2026-09-21T09:59:00Z&to=2026-09-21T10:02:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("仓库趋势应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var resp RepositoryDownloadTrendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析仓库趋势：%v", err)
	}
	if resp.TotalDownloadCount != 5 {
		t.Fatalf("仓库总下载应 5，得 %d", resp.TotalDownloadCount)
	}
	if resp.EffectiveBucket != ObservabilityBucket("minute") {
		t.Fatalf("桶粒度应 minute，得 %s", resp.EffectiveBucket)
	}
	if len(resp.Trend) != 3 {
		t.Fatalf("右开范围 [09:59,10:02) 补零后应 3 个连续桶，得 %d：%+v", len(resp.Trend), resp.Trend)
	}
	wantCounts := []int64{0, 5, 0}
	for i, want := range wantCounts {
		if resp.Trend[i].DownloadCount != want {
			t.Fatalf("第 %d 桶应 %d，得 %d：%+v", i, want, resp.Trend[i].DownloadCount, resp.Trend[i])
		}
	}
	if !resp.Trend[0].From.Equal(downloadTrendBase.Add(-time.Minute)) || !resp.Trend[0].To.Equal(downloadTrendBase) {
		t.Fatalf("首桶标签应为桶起点 09:59~10:00，得 %+v", resp.Trend[0])
	}

	// public 仓总下载 = 2+3 = 5（跨仓不混淆），空数据区间补零全 0。
	rec = serveDownloadTrend(handlers, admin,
		"/api/v1/repositories/trend-public/download-trend?from=2026-09-21T12:00:00Z&to=2026-09-21T12:10:00Z")
	var emptyResp RepositoryDownloadTrendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &emptyResp); err != nil {
		t.Fatalf("解析空区间趋势：%v", err)
	}
	if emptyResp.TotalDownloadCount != 5 {
		t.Fatalf("public 仓总下载应 5，得 %d", emptyResp.TotalDownloadCount)
	}
	if len(emptyResp.Trend) != 10 {
		t.Fatalf("右开空区间 [12:00,12:10) 应补零出 10 个分钟桶，得 %d", len(emptyResp.Trend))
	}
	for _, point := range emptyResp.Trend {
		if point.DownloadCount != 0 {
			t.Fatalf("空区间所有桶应补 0，得 %+v", point)
		}
	}
}

// 零补桶轴必须与仓储 [from,to) 查询一致，不把 to 所在桶误加为最后一个桶。
func TestZeroFilledDownloadTrendPointsUsesHalfOpenUpperBound(t *testing.T) {
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

	t.Run("小时粒度 to 整点不包含终点桶", func(t *testing.T) {
		values := map[time.Time]int64{base: 7, base.Add(time.Hour): 9}
		points := zeroFilledDownloadTrendPoints(values, base, base.Add(time.Hour), "hour")
		if len(points) != 1 || !points[0].From.Equal(base) || !points[0].To.Equal(base.Add(time.Hour)) || points[0].DownloadCount != 7 {
			t.Fatalf("[10:00,11:00) 仅应返回 10:00 桶，不含 11:00：%+v", points)
		}
	})

	t.Run("分钟粒度 from 非整分时跳过起点之前的源桶", func(t *testing.T) {
		values := map[time.Time]int64{base: 7, base.Add(time.Minute): 4}
		points := zeroFilledDownloadTrendPoints(values, base.Add(30*time.Second), base.Add(2*time.Minute), "minute")
		if len(points) != 1 || !points[0].From.Equal(base.Add(time.Minute)) || points[0].DownloadCount != 4 {
			t.Fatalf("[10:00:30,10:02) 仅应返回 10:01 源桶：%+v", points)
		}
	})
}

// 零补桶轴遵循右开范围，并与分钟源桶起点的筛选口径一致。
func TestZeroFilledDownloadTrendPointsUsesHalfOpenUpperBoundary(t *testing.T) {
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

	hourly := zeroFilledDownloadTrendPoints(
		map[time.Time]int64{base: 7, base.Add(time.Hour): 9}, base, base.Add(time.Hour), "hour",
	)
	if len(hourly) != 1 || !hourly[0].From.Equal(base) || hourly[0].DownloadCount != 7 {
		t.Fatalf("[10:00,11:00) 不得生成或纳入 11:00 终点桶：%+v", hourly)
	}

	minuteFrom := base.Add(30 * time.Second)
	minutely := zeroFilledDownloadTrendPoints(
		map[time.Time]int64{base: 7, base.Add(time.Minute): 4}, minuteFrom, base.Add(2*time.Minute), "minute",
	)
	if len(minutely) != 1 || !minutely[0].From.Equal(base.Add(time.Minute)) || minutely[0].DownloadCount != 4 {
		t.Fatalf("分钟粒度应跳过 from 前的源桶并排除 to 桶：%+v", minutely)
	}
}

// from/to 查询参数校验：单边提供、非 RFC3339 均 400；缺省走最近 24 小时。
func TestDownloadTrendRangeQueryValidation(t *testing.T) {
	handlers, _ := newDownloadTrendTestHandlers(t)
	admin := adminPrincipal()

	cases := []struct {
		name string
		path string
		want int
	}{
		{"仅提供 from", "/api/v1/observability/downloads/trend?from=2026-09-21T09:59:00Z", http.StatusBadRequest},
		{"to 非 RFC3339", "/api/v1/repositories/trend-public/download-trend?from=2026-09-21T09:59:00Z&to=yesterday", http.StatusBadRequest},
		{"范围倒置", "/api/v1/observability/downloads/trend?from=2026-09-21T10:00:00Z&to=2026-09-21T09:00:00Z", http.StatusBadRequest},
		{"缺省最近 24 小时", "/api/v1/repositories/trend-public/download-trend", http.StatusOK},
	}
	for _, tc := range cases {
		if rec := serveDownloadTrend(handlers, admin, tc.path); rec.Code != tc.want {
			t.Fatalf("%s：应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
}
