package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件覆盖负缓存查看/清理两个维护端点：鉴权、过滤、清理范围、错误码与审计。

// newNegativeCacheTestEnv 组装带真实 AssetService 的测试环境（负缓存是它的内存态），
// 返回 handlers、仓库 Repo、领域服务与审计 Repo。
func newNegativeCacheTestEnv(t *testing.T) (*Handlers, *repository.RepoRepo, *domain.AssetService, *repository.AuditLogRepo) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	repoSvc := domain.NewRepositoryService(
		repoRepo,
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
	assets := domain.NewAssetService(repoRepo, repository.NewAssetRepo(db),
		blobstore.NewStore(t.TempDir()), upstream.NewTestClient(5*time.Second))
	auditLogs := repository.NewAuditLogRepo(db)
	return NewHandlers(Deps{Repos: repoSvc, Assets: assets, AuditLogs: auditLogs}), repoRepo, assets, auditLogs
}

// serveNegativeCacheRoute 经生成的 ServerInterfaceWrapper 发请求（测试口径即线上口径）。
func serveNegativeCacheRoute(h *Handlers, principal *auth.Principal, method, path string) *httptest.ResponseRecorder {
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
	router.GET("/api/v1/maintenance/negative-cache", siw.GetNegativeCache)
	router.DELETE("/api/v1/maintenance/negative-cache", siw.PurgeNegativeCache)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// seedNotFoundUpstream 起一个恒 404 的上游并返回其 URL。
func seedNotFoundUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 两个端点都只对管理员开放：匿名 401、普通用户 403，且越权请求不得清理任何条目。
func TestNegativeCacheEndpointsRequireAdmin(t *testing.T) {
	h, repoRepo, assets, _ := newNegativeCacheTestEnv(t)
	upstreamURL := seedNotFoundUpstream(t)
	if _, err := repoRepo.Create("nc-auth", "raw", "proxy", "private", proxyConfigJSONForTest(t, upstreamURL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	if _, _, err := assets.Resolve(t.Context(), "nc-auth", "keep.jar"); err != domain.ErrNotFound {
		t.Fatalf("应以 ErrNotFound 写入负缓存，实际：%v", err)
	}

	cases := []struct {
		name      string
		principal *auth.Principal
		method    string
		path      string
		want      int
	}{
		{"匿名查看", nil, http.MethodGet, "/api/v1/maintenance/negative-cache", http.StatusUnauthorized},
		{"普通用户查看", userPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache", http.StatusForbidden},
		{"匿名清理", nil, http.MethodDelete, "/api/v1/maintenance/negative-cache", http.StatusUnauthorized},
		{"普通用户清理", userPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache", http.StatusForbidden},
	}
	for _, tc := range cases {
		rec := serveNegativeCacheRoute(h, tc.principal, tc.method, tc.path)
		if rec.Code != tc.want {
			t.Errorf("%s：应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	// 越权清理不得落地：条目仍在。
	snap, err := assets.NegativeCacheSnapshot(domain.NegativeCacheFilter{})
	if err != nil {
		t.Fatalf("取快照：%v", err)
	}
	if snap.Total != 1 {
		t.Fatalf("越权请求不得清理负缓存，实际剩 %d 条", snap.Total)
	}
}

// GET 响应形状与过滤：字段对应契约，total 为过滤前总数，repo / path_prefix / limit 生效。
func TestGetNegativeCacheShapeAndFilters(t *testing.T) {
	h, repoRepo, assets, _ := newNegativeCacheTestEnv(t)
	upstreamURL := seedNotFoundUpstream(t)
	assets.SetNegativeCacheTTL(time.Hour)
	repoAID, err := repoRepo.Create("nc-a", "raw", "proxy", "private", proxyConfigJSONForTest(t, upstreamURL))
	if err != nil {
		t.Fatalf("建仓库 A：%v", err)
	}
	if _, err := repoRepo.Create("nc-b", "raw", "proxy", "private", proxyConfigJSONForTest(t, upstreamURL)); err != nil {
		t.Fatalf("建仓库 B：%v", err)
	}
	for _, path := range []string{"lib/a1.jar", "lib/a2.jar", "other/a3.jar"} {
		if _, _, err := assets.Resolve(t.Context(), "nc-a", path); err != domain.ErrNotFound {
			t.Fatalf("A/%s 应 ErrNotFound，实际：%v", path, err)
		}
	}
	if _, _, err := assets.Resolve(t.Context(), "nc-b", "lib/b1.jar"); err != domain.ErrNotFound {
		t.Fatalf("B/lib/b1.jar 应 ErrNotFound，实际：%v", err)
	}

	rec := serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache")
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员查看应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var summary NegativeCacheSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if summary.Total != 4 || len(summary.Items) != 4 {
		t.Fatalf("应为 4 条，实际 total=%d items=%d", summary.Total, len(summary.Items))
	}
	if summary.TtlSeconds != int64(time.Hour/time.Second) || summary.MaxEntries <= 0 {
		t.Fatalf("应带出 TTL 与上限，实际 %+v", summary)
	}
	first := summary.Items[0]
	if first.RepositoryId <= 0 || first.Path == "" || first.WrittenAt.IsZero() || first.RemainingSeconds <= 0 {
		t.Fatalf("条目字段不完整：%+v", first)
	}
	if first.Repository == nil || *first.Repository == "" {
		t.Fatalf("条目应补全仓库名：%+v", first)
	}

	// 按仓库过滤：total 仍是过滤前总数。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache?repo=nc-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("按仓库过滤应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	summary = NegativeCacheSummary{}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if summary.Total != 4 {
		t.Fatalf("过滤时 total 应为过滤前总数 4，实际 %d", summary.Total)
	}
	if len(summary.Items) != 3 {
		t.Fatalf("nc-a 应有 3 条，实际 %d", len(summary.Items))
	}
	for _, item := range summary.Items {
		if item.RepositoryId != repoAID {
			t.Fatalf("过滤结果混入其它仓库：%+v", item)
		}
	}

	// 路径前缀过滤 + limit 截断（升序第一条）。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache?repo=nc-a&path_prefix=lib/&limit=1")
	summary = NegativeCacheSummary{}
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(summary.Items) != 1 || summary.Items[0].Path != "lib/a1.jar" {
		t.Fatalf("limit=1 应按写入时间升序返回最早的一条 lib/a1.jar，实际 %+v", summary.Items)
	}

	// limit 超上限按截断处理（契约声明），不返回 400。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache?limit=999")
	if rec.Code != http.StatusOK {
		t.Fatalf("limit 超上限应截断而非报错，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 仓库名不存在 → 404（而不是空列表，否则会被误读成「从没缓存过」）。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache?repo=nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知仓库应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// DELETE 语义：无参数全清、仅 repo 按仓库清、repo+path 精确清；path 缺 repo → 400。
// 清理写审计，查看不写。
func TestPurgeNegativeCacheScopesAndAudit(t *testing.T) {
	h, repoRepo, assets, auditLogs := newNegativeCacheTestEnv(t)
	upstreamURL := seedNotFoundUpstream(t)
	assets.SetNegativeCacheTTL(time.Hour)
	repoAID, err := repoRepo.Create("purge-a", "raw", "proxy", "private", proxyConfigJSONForTest(t, upstreamURL))
	if err != nil {
		t.Fatalf("建仓库 A：%v", err)
	}
	if _, err := repoRepo.Create("purge-b", "raw", "proxy", "private", proxyConfigJSONForTest(t, upstreamURL)); err != nil {
		t.Fatalf("建仓库 B：%v", err)
	}
	for _, path := range []string{"x1.jar", "x2.jar"} {
		if _, _, err := assets.Resolve(t.Context(), "purge-a", path); err != domain.ErrNotFound {
			t.Fatalf("A/%s 应 ErrNotFound，实际：%v", path, err)
		}
	}
	if _, _, err := assets.Resolve(t.Context(), "purge-b", "y1.jar"); err != domain.ErrNotFound {
		t.Fatalf("B/y1.jar 应 ErrNotFound，实际：%v", err)
	}

	// 查看不写审计。
	if rec := serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache"); rec.Code != http.StatusOK {
		t.Fatalf("查看应 200，得 %d", rec.Code)
	}
	if entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceNegativeCachePurge}); err != nil {
		t.Fatalf("查审计：%v", err)
	} else if len(entries) != 0 {
		t.Fatalf("查看不得写审计，实际 %d 条", len(entries))
	}

	// path 未指定 repo → 400。
	rec := serveNegativeCacheRoute(h, adminPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache?path=x1.jar")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("path 缺 repo 应 400，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 精确清：只删这一条。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache?repo=purge-a&path=x1.jar")
	if rec.Code != http.StatusOK {
		t.Fatalf("精确清应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var purge NegativeCachePurgeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &purge); err != nil {
		t.Fatalf("解析清理结果：%v", err)
	}
	if purge.Removed != 1 {
		t.Fatalf("精确清应删 1 条，实际 %d", purge.Removed)
	}

	// 按仓库清：删掉该仓库剩余条目。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache?repo=purge-a")
	purge = NegativeCachePurgeResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &purge); err != nil {
		t.Fatalf("解析清理结果：%v", err)
	}
	if purge.Removed != 1 {
		t.Fatalf("按仓库清应删剩余 1 条，实际 %d", purge.Removed)
	}
	snap, err := assets.NegativeCacheSnapshot(domain.NegativeCacheFilter{})
	if err != nil {
		t.Fatalf("取快照：%v", err)
	}
	if snap.Total != 1 || snap.Items[0].RepositoryID == repoAID {
		t.Fatalf("按仓库清不应影响 purge-b，实际 %+v", snap)
	}

	// 全清。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache")
	purge = NegativeCachePurgeResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &purge); err != nil {
		t.Fatalf("解析清理结果：%v", err)
	}
	if purge.Removed != 1 {
		t.Fatalf("全清应删最后 1 条，实际 %d", purge.Removed)
	}
	snap, err = assets.NegativeCacheSnapshot(domain.NegativeCacheFilter{})
	if err != nil {
		t.Fatalf("取快照：%v", err)
	}
	if snap.Total != 0 {
		t.Fatalf("全清后应为空，实际 %d", snap.Total)
	}

	// 三次清理各写一条审计。
	entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceNegativeCachePurge})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("三次清理应各写一条审计，实际 %d 条", len(entries))
	}
	for _, entry := range entries {
		if entry.Result != "ok" {
			t.Fatalf("清理审计结果应为 ok，实际 %q", entry.Result)
		}
		// 审计详情是单行文本：不得带换行，也不得回显任何凭据引用名。
		if strings.ContainsAny(entry.Detail, "\n\r") {
			t.Fatalf("审计详情不得含换行：%q", entry.Detail)
		}
		if strings.Contains(entry.Detail, "upstream-secret") {
			t.Fatalf("审计详情不得包含凭据引用名：%q", entry.Detail)
		}
	}

	// 未知仓库 → 404。
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache?repo=nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知仓库清理应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 未接线 AssetService 时两个端点回 503，而不是 panic 或 200 谎报成功。
func TestNegativeCacheEndpointsUnavailableWithoutAssetService(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	h := NewHandlers(Deps{})
	rec := serveNegativeCacheRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/negative-cache")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线查看应 503，得 %d：%s", rec.Code, rec.Body.String())
	}
	rec = serveNegativeCacheRoute(h, adminPrincipal(), http.MethodDelete, "/api/v1/maintenance/negative-cache")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线清理应 503，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 响应映射的脱敏与转义：路径控制字符被转义、仓库名为空时不落成空串指针、
// 剩余 TTL 不为负（与诊断日志共用同一个助手）。
func TestToAPINegativeCacheSummarySanitizes(t *testing.T) {
	snap := domain.NegativeCacheSnapshot{
		Total:      2,
		TTL:        60 * time.Second,
		MaxEntries: 10000,
		Items: []domain.NegativeCacheEntry{
			{RepositoryID: 1, Repository: "r1", Path: "a\nb.jar", WrittenAt: time.Now(), Remaining: 1500 * time.Millisecond},
			// 已过期但仍在映射里（快照实现会清理，这里直接构造以钉住映射的兜底行为）。
			{RepositoryID: 2, Path: "c.jar", WrittenAt: time.Now(), Remaining: -time.Second},
		},
	}
	got := toAPINegativeCacheSummary(snap)
	if len(got.Items) != 2 {
		t.Fatalf("应映射 2 条，实际 %d", len(got.Items))
	}
	if got.Items[0].Path != `a\nb.jar` {
		t.Fatalf("路径中的换行应转义为字面 \\n，实际 %q", got.Items[0].Path)
	}
	if got.Items[0].Repository == nil || *got.Items[0].Repository != "r1" {
		t.Fatalf("仓库名应带出：%+v", got.Items[0])
	}
	if got.Items[1].Repository != nil {
		t.Fatalf("仓库名缺失时应为 nil（序列化为缺省），实际 %q", *got.Items[1].Repository)
	}
	if got.Items[1].RemainingSeconds != 0 {
		t.Fatalf("剩余 TTL 不得为负，实际 %d", got.Items[1].RemainingSeconds)
	}
	if got.Items[0].RemainingSeconds != 1 {
		t.Fatalf("剩余秒数应向下取整为 1，实际 %d", got.Items[0].RemainingSeconds)
	}
}

// proxyConfigJSONForTest 构造仅含 remoteUrl 的仓库配置 JSON（本文件专用，
// 不依赖 domain 包的测试助手）。
func proxyConfigJSONForTest(t *testing.T, remoteURL string) string {
	t.Helper()
	s, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{RemoteURL: remoteURL})
	if err != nil {
		t.Fatalf("编码 proxy 配置：%v", err)
	}
	return s
}
