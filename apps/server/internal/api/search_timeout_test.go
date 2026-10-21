package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 FR-149 §3.4 的**错误码契约**：搜索超出预算时必须映射为
// HTTP 504 + 错误码 `search_timeout`，而不是复用上游的 `upstream_timeout`——
// 两者的处置方向完全相反（一个该缩小检索范围 / 查索引，一个该查上游）。

// TestSearchTimeoutErrorMapping 直接钉住错误码映射本身（纯映射，无数据库依赖）。
func TestSearchTimeoutErrorMapping(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.GET("/mapped", func(c *gin.Context) { writeDomainErr(c, domain.ErrSearchTimeout) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mapped", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d，期望 504", rec.Code)
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析错误响应：%v（体：%s）", err, rec.Body.String())
	}
	if out.Error.Code != "search_timeout" {
		t.Fatalf("错误码 = %q，期望 search_timeout（不得复用 upstream_timeout）", out.Error.Code)
	}
	if out.Error.Message == "" {
		t.Fatal("超时响应缺少可读消息")
	}
}

// 搜索 handler 的端到端行为：正常预算下结构不变；请求上下文被取消（客户端断开 / 预算耗尽）
// 时返回 504 + search_timeout。
//
// 这里用「已取消的请求上下文」而不是极小超时：setTimeout 级别的预算与查询存在竞态
// （查询可能在定时器触发前就跑完），会让用例时好时坏；取消上下文是确定性的，且正是
// handler 把 c.Request.Context() 交给领域层这条链路的真实语义。

// searchTimeoutEnv 是一个只关心搜索错误映射的最小环境。
type searchTimeoutEnv struct {
	handlers *Handlers
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
}

// newSearchTimeoutEnv 建库并装配处理器。
func newSearchTimeoutEnv(t *testing.T) *searchTimeoutEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assets, settings, users)
	return &searchTimeoutEnv{
		handlers: NewHandlers(Deps{Repos: svc, Settings: settings}),
		repos:    repoRepo,
		assets:   assets,
	}
}

// seed 建一个公开仓库并放一条资产。
func (e *searchTimeoutEnv) seed(t *testing.T) {
	t.Helper()
	repoID, err := e.repos.Create("search-timeout-repo", "raw", "hosted", "public", "{}")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	if err := e.assets.Upsert(repoID, "libs/timeout-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
}

// serveSearch 以管理员身份发起一次 /api/v1/search 请求；ctx 为 nil 表示用默认上下文。
func (e *searchTimeoutEnv) serveSearch(t *testing.T, ctx context.Context, query string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 1, Role: "admin", Kind: auth.KindSession})
	})
	router.GET("/api/v1/search", e.handlers.SearchAssets)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?"+query, strings.NewReader(""))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestSearchHandlerTimeoutPath 验证 handler 把请求上下文交给领域层，并映射为 504。
func TestSearchHandlerTimeoutPath(t *testing.T) {
	e := newSearchTimeoutEnv(t)
	e.seed(t)

	// 正常预算：200 且响应结构不变（items / total / facets）。
	rec := e.serveSearch(t, nil, "q=timeout")
	if rec.Code != http.StatusOK {
		t.Fatalf("正常搜索状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	var ok struct {
		Items  []any `json:"items"`
		Total  int   `json:"total"`
		Facets []any `json:"facets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("解析搜索响应：%v（体：%s）", err, rec.Body.String())
	}
	if ok.Total != 1 || len(ok.Items) != 1 {
		t.Fatalf("应命中 1 条，得 total=%d items=%d", ok.Total, len(ok.Items))
	}

	// 请求上下文已取消：与客户端断开 / 预算耗尽同一条链路，必须 504 + search_timeout。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rec = e.serveSearch(t, cancelled, "q=timeout")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("超时状态码 = %d（体：%s），期望 504", rec.Code, rec.Body.String())
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析错误响应：%v（体：%s）", err, rec.Body.String())
	}
	if out.Error.Code != "search_timeout" {
		t.Fatalf("错误码 = %q，期望 search_timeout", out.Error.Code)
	}
}
