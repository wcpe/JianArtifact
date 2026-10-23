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

// 本文件覆盖 FR-159 查询端点的验收项（规格 §2.3 与 §5.1）：
// 非 proxy 返回 400、未知仓库 404、未接线 503、非管理员 403、按仓库隔离、
// 空态 connectivity 为 null（而不是 0 或 100%）。

// newUpstreamHealthTestHandlers 组装端点测试环境：一个 proxy 仓库、一个 hosted 仓库，
// 以及真实的 UpstreamHealthRepo（走真实 SQLite，确保查询语义与生产一致）。
func newUpstreamHealthTestHandlers(t *testing.T) (*Handlers, *repository.UpstreamHealthRepo, int64, int64) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	repoSvc := domain.NewRepositoryService(
		repository.NewRepoRepo(db),
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
	proxyRepo, err := repoSvc.Create("uh-proxy", "raw", "proxy", "private", "",
		repository.RepositoryConfig{RemoteURL: "http://upstream.invalid"})
	if err != nil {
		t.Fatalf("创建 proxy 仓库：%v", err)
	}
	hostedRepo, err := repoSvc.Create("uh-hosted", "raw", "hosted", "private", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	healthRepo := repository.NewUpstreamHealthRepo(db)
	return NewHandlers(Deps{Repos: repoSvc, UpstreamHealth: healthRepo}), healthRepo, proxyRepo.ID, hostedRepo.ID
}

// upstreamHealthAdminContext 构造一个已通过管理员鉴权的测试上下文。
func upstreamHealthAdminContext(method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, target, nil)
	context.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
	return context, recorder
}

// 非 proxy 仓库（hosted）返回 400：它没有上游概念，用 200 + 空序列会让调用方
// 误以为「只是还没采样」。
func TestGetRepositoryUpstreamHealthRejectsNonProxy(t *testing.T) {
	handlers, _, _, _ := newUpstreamHealthTestHandlers(t)
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-hosted/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-hosted"), GetRepositoryUpstreamHealthParams{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("hosted 仓库应返回 400，得 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 最近明细端点同一口径。
	context, recorder = upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-hosted/upstream-health/recent")
	handlers.GetRepositoryUpstreamHealthRecent(context, RepoNameParam("uh-hosted"), GetRepositoryUpstreamHealthRecentParams{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("hosted 仓库的最近明细应返回 400，得 %d", recorder.Code)
	}
}

// 未知仓库返回 404。
func TestGetRepositoryUpstreamHealthUnknownRepository(t *testing.T) {
	handlers, _, _, _ := newUpstreamHealthTestHandlers(t)
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/missing/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("missing"), GetRepositoryUpstreamHealthParams{})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知仓库应返回 404，得 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// 未接线（Deps 未注入 UpstreamHealth）返回 503，而不是 200 + 空序列。
func TestGetRepositoryUpstreamHealthUnwiredReturns503(t *testing.T) {
	handlers, _, _, _ := newUpstreamHealthTestHandlers(t)
	// 换成未接线的实例：只保留仓库服务。
	unwired := NewHandlers(Deps{Repos: handlers.repos})
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	unwired.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"), GetRepositoryUpstreamHealthParams{})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线应返回 503，得 %d：%s", recorder.Code, recorder.Body.String())
	}

	context, recorder = upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health/recent")
	unwired.GetRepositoryUpstreamHealthRecent(context, RepoNameParam("uh-proxy"), GetRepositoryUpstreamHealthRecentParams{})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线的最近明细应返回 503，得 %d", recorder.Code)
	}
}

// 非管理员返回 403（响应含失败原因分布，不宜对普通用户暴露）。
func TestGetRepositoryUpstreamHealthRequiresAdmin(t *testing.T) {
	handlers, _, _, _ := newUpstreamHealthTestHandlers(t)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health", nil)
	context.Set("auth.principal", &auth.Principal{Role: "user", Username: "bob", UserID: 2})
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"), GetRepositoryUpstreamHealthParams{})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应返回 403，得 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// 空态：窗口内没有采样时 points 为空、summaries 为空、connectivity 缺失（不是 0 或 1）。
// 这条钉住「尚无采样」与「100% 健康」必须可区分。
func TestGetRepositoryUpstreamHealthEmptyWindowHasNoConnectivity(t *testing.T) {
	handlers, _, _, _ := newUpstreamHealthTestHandlers(t)
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"), GetRepositoryUpstreamHealthParams{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("空窗口应返回 200，得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body UpstreamHealthSeries
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(body.Points) != 0 {
		t.Fatalf("空窗口不应有采样点：%+v", body.Points)
	}
	if len(body.Summaries) != 0 {
		t.Fatalf("空窗口不应有来源汇总：%+v", body.Summaries)
	}
	if body.Repository != "uh-proxy" {
		t.Fatalf("应回显仓库名，得 %s", body.Repository)
	}
}

// 按仓库隔离：A 仓库的采样不出现在 B 仓库的查询结果里；
// 同时验证 request / probe 两条序列各自成汇总、互不平均。
func TestGetRepositoryUpstreamHealthIsolatesRepositoriesAndSources(t *testing.T) {
	handlers, healthRepo, proxyRepoID, _ := newUpstreamHealthTestHandlers(t)
	// 另造一个 proxy 仓库用于隔离断言。
	otherRepo, err := handlers.repos.Create("uh-proxy-other", "raw", "proxy", "private", "",
		repository.RepositoryConfig{RemoteURL: "http://other.invalid"})
	if err != nil {
		t.Fatalf("创建第二个 proxy 仓库：%v", err)
	}

	now := time.Now().UTC()
	bucket := repository.UpstreamHealthBucketStart(now.Add(-2 * time.Minute))
	// 目标仓库：request 3 次（2 成功 1 超时）+ probe 1 次成功。
	if err := healthRepo.AddSamples(repository.UpstreamHealthDelta{
		BucketStart: bucket, RepositoryID: proxyRepoID, Source: repository.UpstreamHealthSourceRequest,
		Samples: 3, OK: 2, Timeout: 1, LatencySumMs: 200, LatencyCountMs: 2, MaxLatencyMs: 150,
	}); err != nil {
		t.Fatalf("写入 request 采样：%v", err)
	}
	if err := healthRepo.AddSamples(repository.UpstreamHealthDelta{
		BucketStart: bucket, RepositoryID: proxyRepoID, Source: repository.UpstreamHealthSourceProbe,
		Samples: 1, OK: 1, LatencySumMs: 20, LatencyCountMs: 1, MaxLatencyMs: 20,
	}); err != nil {
		t.Fatalf("写入 probe 采样：%v", err)
	}
	// 另一个仓库：1 次不可达。
	if err := healthRepo.AddSamples(repository.UpstreamHealthDelta{
		BucketStart: bucket, RepositoryID: otherRepo.ID, Source: repository.UpstreamHealthSourceRequest,
		Samples: 1, Unreachable: 1,
	}); err != nil {
		t.Fatalf("写入其它仓库采样：%v", err)
	}

	from, to := now.Add(-time.Hour), now
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"),
		GetRepositoryUpstreamHealthParams{From: &from, To: &to})
	if recorder.Code != http.StatusOK {
		t.Fatalf("应返回 200，得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body UpstreamHealthSeries
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(body.Points) != 2 {
		t.Fatalf("应看到本仓库的两条来源各一个点，实际 %d 个：%+v", len(body.Points), body.Points)
	}
	for _, point := range body.Points {
		if point.Samples == 0 {
			t.Fatalf("采样点数不应为 0：%+v", point)
		}
	}
	requestSummary, ok := body.Summaries[string(UpstreamHealthSource("request"))]
	if !ok {
		t.Fatalf("应有 request 汇总：%+v", body.Summaries)
	}
	// request：2/3 成功 → 连通率 0.6667；失败 1 次全在 timeout 列。
	if requestSummary.Samples != 3 || requestSummary.Ok != 2 || requestSummary.Timeout != 1 {
		t.Fatalf("request 汇总错误：%+v", requestSummary)
	}
	if requestSummary.Connectivity == nil || *requestSummary.Connectivity < 0.66 || *requestSummary.Connectivity > 0.67 {
		t.Fatalf("request 连通率应为 2/3，得 %v", requestSummary.Connectivity)
	}
	// probe：1/1 成功 → 连通率 1。两条序列必须各自成率（若被平均会得 3/4）。
	probeSummary, ok := body.Summaries[string(UpstreamHealthSource("probe"))]
	if !ok {
		t.Fatalf("应有 probe 汇总：%+v", body.Summaries)
	}
	if probeSummary.Connectivity == nil || *probeSummary.Connectivity != 1 {
		t.Fatalf("probe 连通率应为 1，得 %v", probeSummary.Connectivity)
	}

	// 按来源过滤：只要 probe 时不应出现 request 的汇总。
	source := GetRepositoryUpstreamHealthParamsSource(repository.UpstreamHealthSourceProbe)
	context, recorder = upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"),
		GetRepositoryUpstreamHealthParams{From: &from, To: &to, Source: &source})
	if recorder.Code != http.StatusOK {
		t.Fatalf("按来源过滤应返回 200，得 %d", recorder.Code)
	}
	var filtered UpstreamHealthSeries
	if err := json.Unmarshal(recorder.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("解析过滤响应：%v", err)
	}
	if len(filtered.Summaries) != 1 {
		t.Fatalf("按来源过滤后应只剩一个汇总：%+v", filtered.Summaries)
	}
	if _, found := filtered.Summaries[string(UpstreamHealthSource("probe"))]; !found {
		t.Fatalf("过滤后应保留 probe 汇总：%+v", filtered.Summaries)
	}

	// 另一个仓库的序列不得混进来：它的样本数是 1（不可达），本仓库两路合计 4。
	for _, point := range filtered.Points {
		if point.Unreachable != 0 {
			t.Fatalf("本仓库不应出现其它仓库的不可达样本：%+v", point)
		}
	}
}

// 非法 source 与非法时间范围返回 400。
func TestGetRepositoryUpstreamHealthValidatesParameters(t *testing.T) {
	handlers, _, _, _ := newUpstreamHealthTestHandlers(t)

	badSource := GetRepositoryUpstreamHealthParamsSource("bogus")
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"),
		GetRepositoryUpstreamHealthParams{Source: &badSource})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 source 应返回 400，得 %d", recorder.Code)
	}

	// from 单独提供（缺 to）→ 400。
	from := time.Now().UTC()
	context, recorder = upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"),
		GetRepositoryUpstreamHealthParams{From: &from})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("只提供 from 应返回 400，得 %d", recorder.Code)
	}

	// 倒置窗口 → 400。
	to := from.Add(-time.Hour)
	context, recorder = upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health")
	handlers.GetRepositoryUpstreamHealth(context, RepoNameParam("uh-proxy"),
		GetRepositoryUpstreamHealthParams{From: &from, To: &to})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("倒置窗口应返回 400，得 %d", recorder.Code)
	}
}

// 最近明细：按时间倒序返回分钟桶摘要，并回显仓库名；空态是空数组而非错误。
func TestGetRepositoryUpstreamHealthRecentReturnsBuckets(t *testing.T) {
	handlers, healthRepo, proxyRepoID, _ := newUpstreamHealthTestHandlers(t)
	now := time.Now().UTC()

	// 空态：空数组。
	context, recorder := upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health/recent")
	handlers.GetRepositoryUpstreamHealthRecent(context, RepoNameParam("uh-proxy"), GetRepositoryUpstreamHealthRecentParams{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("空态应返回 200，得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var empty UpstreamHealthRecent
	if err := json.Unmarshal(recorder.Body.Bytes(), &empty); err != nil {
		t.Fatalf("解析空态响应：%v", err)
	}
	if len(empty.Items) != 0 {
		t.Fatalf("空态 items 应为空数组：%+v", empty.Items)
	}

	// 两个分钟桶，较新的应排在前面。
	for i := 0; i < 2; i++ {
		bucket := repository.UpstreamHealthBucketStart(now.Add(-time.Duration(i+1) * time.Minute))
		if err := healthRepo.AddSamples(repository.UpstreamHealthDelta{
			BucketStart: bucket, RepositoryID: proxyRepoID, Source: repository.UpstreamHealthSourceRequest,
			Samples: 1, OK: 1, LatencySumMs: int64(10 * (i + 1)), LatencyCountMs: 1, MaxLatencyMs: int64(10 * (i + 1)),
		}); err != nil {
			t.Fatalf("写入采样：%v", err)
		}
	}
	context, recorder = upstreamHealthAdminContext(http.MethodGet, "/api/v1/repositories/uh-proxy/upstream-health/recent")
	handlers.GetRepositoryUpstreamHealthRecent(context, RepoNameParam("uh-proxy"), GetRepositoryUpstreamHealthRecentParams{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("应返回 200，得 %d", recorder.Code)
	}
	var body UpstreamHealthRecent
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("应返回 2 个桶，实际 %d", len(body.Items))
	}
	if body.Items[0].BucketStart <= body.Items[1].BucketStart {
		t.Fatalf("明细应按时间倒序：%+v", body.Items)
	}
	if body.Repository != "uh-proxy" {
		t.Fatalf("应回显仓库名，得 %s", body.Repository)
	}
}

// upstreamHealthSummary 的延迟口径：无成功样本时 avg / p95 均为 nil（不伪造 0）。
func TestUpstreamHealthSummaryLatencyNullWithoutSuccess(t *testing.T) {
	summary := upstreamHealthSummary([]repository.UpstreamHealthBucket{
		{BucketStart: "2026-08-27 10:00:00", Source: repository.UpstreamHealthSourceRequest, Samples: 2, Unreachable: 1, Timeout: 1},
	})
	if summary.Samples != 2 || summary.Ok != 0 {
		t.Fatalf("汇总计数错误：%+v", summary)
	}
	if summary.AvgLatencyMs != nil || summary.P95LatencyMs != nil {
		t.Fatalf("无成功样本时延迟必须为 nil：avg=%v p95=%v", summary.AvgLatencyMs, summary.P95LatencyMs)
	}
	// 连通率是 0（有样本、0 成功）：这与「无采样」不是一回事，因此必须给出 0 而不是 nil。
	if summary.Connectivity == nil || *summary.Connectivity != 0 {
		t.Fatalf("有样本但全失败时连通率应为 0，得 %v", summary.Connectivity)
	}
}

// 空汇总（零样本）时连通率必须是 nil，不能是 0 或 1。
func TestUpstreamHealthSummaryEmptyHasNoConnectivity(t *testing.T) {
	summary := upstreamHealthSummary(nil)
	if summary.Connectivity != nil {
		t.Fatalf("无样本时连通率必须为 nil，得 %v", *summary.Connectivity)
	}
	if summary.Samples != 0 {
		t.Fatalf("无样本时 samples 应为 0，得 %d", summary.Samples)
	}
}
