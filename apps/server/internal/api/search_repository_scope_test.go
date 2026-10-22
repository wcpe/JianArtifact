package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 `/api/v1/search?repository=` 的**反枚举口径**。
//
// 领域层对「仓库不存在」返回 ErrNotFound、对「存在但无权读」返回 (false, nil)。若 API 层把
// 两者分别映射成 404 与 401/403，调用方只要反复改动 repository= 参数并观察状态码，就能把
// 实例上有哪些私有仓库枚举出来——一条匿名可达的信息泄漏路径。因此非管理员（含匿名）对两种
// 情况必须得到**逐字节相同**的 404。
//
// 这与 suggest 端点新立的口径是同一条（可读集合为空时返回空结果，绝不 403：403 会泄漏
// 「这里存在一个你看不到的仓库」）。

// searchScopeEnv 是本组用例的最小环境：一公开一私有仓库，各带一条资产。
type searchScopeEnv struct {
	handlers   *Handlers
	publicName string
}

// newSearchScopeEnv 建库、装配处理器，并写入公开 / 私有各一个仓库。
func newSearchScopeEnv(t *testing.T) *searchScopeEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	// bob 是非管理员主体（ID 与 userPrincipal() 一致）——用例要覆盖「已登录但无权读」。
	if _, err := users.Create("bob", "hash", "user"); err != nil {
		t.Fatalf("创建 bob：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assets, settings, users)

	publicID, err := repoRepo.Create("open-repo", "raw", "hosted", "public", "{}")
	if err != nil {
		t.Fatalf("创建公开仓库：%v", err)
	}
	if err := assets.Upsert(publicID, "libs/open-1.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入公开仓库资产：%v", err)
	}
	privateID, err := repoRepo.Create("secret-repo", "raw", "hosted", "private", "{}")
	if err != nil {
		t.Fatalf("创建私有仓库：%v", err)
	}
	if err := assets.Upsert(privateID, "libs/secret-1.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入私有仓库资产：%v", err)
	}
	return &searchScopeEnv{
		handlers:   NewHandlers(Deps{Repos: svc, Settings: settings}),
		publicName: "open-repo",
	}
}

// serve 以指定主体发起一次 /api/v1/search；p 为 nil 表示匿名。
func (e *searchScopeEnv) serve(t *testing.T, p *auth.Principal, query string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if p != nil {
			c.Set("auth.principal", p)
		}
	})
	router.GET("/api/v1/search", e.handlers.SearchAssets)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search?"+query, nil))
	return rec
}

// TestSearchRepositoryFilterHidesPrivateExistence 验证：非管理员 / 匿名在 repository=
// 上无法区分「仓库不存在」与「仓库存在但无读权限」——两种请求的状态码与响应体必须完全一致。
func TestSearchRepositoryFilterHidesPrivateExistence(t *testing.T) {
	e := newSearchScopeEnv(t)

	requesters := []struct {
		name string
		p    *auth.Principal
	}{
		{"匿名", nil},
		{"已登录非管理员", userPrincipal()},
	}
	for _, r := range requesters {
		t.Run(r.name, func(t *testing.T) {
			// 存在但无权读
			unreadable := e.serve(t, r.p, "q=secret&repository=secret-repo")
			// 完全不存在
			missing := e.serve(t, r.p, "q=secret&repository=no-such-repo")

			if unreadable.Code != http.StatusNotFound {
				t.Fatalf("无权读的仓库状态码 = %d（体：%s），期望 404", unreadable.Code, unreadable.Body.String())
			}
			if missing.Code != http.StatusNotFound {
				t.Fatalf("不存在的仓库状态码 = %d（体：%s），期望 404", missing.Code, missing.Body.String())
			}
			// 关键断言：两者不仅要同级，还要逐字节相同；否则仍可从响应差异反推仓库是否存在。
			if unreadable.Body.String() != missing.Body.String() {
				t.Fatalf("两种失败响应的响应体不同，可被用于枚举私有仓库：\n存在=%s\n不存在=%s",
					unreadable.Body.String(), missing.Body.String())
			}
			var out struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(unreadable.Body.Bytes(), &out); err != nil {
				t.Fatalf("解析错误响应：%v（体：%s）", err, unreadable.Body.String())
			}
			if out.Error.Code != "not_found" {
				t.Fatalf("错误码 = %q，期望 not_found", out.Error.Code)
			}
		})
	}

	// 对照组：公开仓库对非管理员仍正常返回 200——上面的 404 不能把可读的仓库也挡掉。
	if rec := e.serve(t, userPrincipal(), "q=open&repository="+e.publicName); rec.Code != http.StatusOK {
		t.Fatalf("可读的公开仓库状态码 = %d（体：%s），期望 200", rec.Code, rec.Body.String())
	}

	// 管理员不受该口径影响：私有仓库照常可搜，不存在的仓库照常 404。
	if rec := e.serve(t, adminPrincipal(), "q=secret&repository=secret-repo"); rec.Code != http.StatusOK {
		t.Fatalf("管理员搜私有仓库状态码 = %d（体：%s），期望 200", rec.Code, rec.Body.String())
	}
	if rec := e.serve(t, adminPrincipal(), "q=secret&repository=no-such-repo"); rec.Code != http.StatusNotFound {
		t.Fatalf("管理员搜不存在的仓库状态码 = %d（体：%s），期望 404", rec.Code, rec.Body.String())
	}
}
