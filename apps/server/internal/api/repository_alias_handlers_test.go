package api

import (
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

// newRepoAliasTestHandlers 组装仓库别名/重命名端点测试环境。
func newRepoAliasTestHandlers(t *testing.T) *Handlers {
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
	return NewHandlers(Deps{Repos: repoSvc})
}

// serveRepoRoute 经生成的 ServerInterfaceWrapper 发起请求（测试口径即线上口径）。
func serveRepoRoute(h *Handlers, principal *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
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
	router.POST("/api/v1/repositories", siw.CreateRepository)
	router.POST("/api/v1/repositories/:name/rename", siw.RenameRepository)

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// 重命名仅管理员：匿名 401、非管理员 403、管理员 200。
func TestRenameRepositoryRequiresAdmin(t *testing.T) {
	h := newRepoAliasTestHandlers(t)
	if rec := serveRepoRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", `{"name":"raw-a","format":"raw","type":"hosted"}`); rec.Code != http.StatusCreated {
		t.Fatalf("预置仓库应 201，得 %d：%s", rec.Code, rec.Body.String())
	}

	const body = `{"newName":"raw-b"}`
	if rec := serveRepoRoute(h, nil, http.MethodPost, "/api/v1/repositories/raw-a/rename", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名重命名应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveRepoRoute(h, userPrincipal(), http.MethodPost, "/api/v1/repositories/raw-a/rename", body); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员重命名应 403，得 %d：%s", rec.Code, rec.Body.String())
	}

	rec := serveRepoRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories/raw-a/rename", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员重命名应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var repo Repository
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if repo.Name != "raw-b" {
		t.Fatalf("响应应返回新名，得 %q", repo.Name)
	}
	if repo.Aliases == nil || !containsString(*repo.Aliases, "raw-a") {
		t.Fatalf("旧名应成为别名，得 %v", repo.Aliases)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// 重命名非法输入与冲突的错误码映射。
func TestRenameRepositoryErrors(t *testing.T) {
	h := newRepoAliasTestHandlers(t)
	for _, spec := range []string{
		`{"name":"raw-a","format":"raw","type":"hosted"}`,
		`{"name":"raw-b","format":"raw","type":"hosted"}`,
	} {
		if rec := serveRepoRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", spec); rec.Code != http.StatusCreated {
			t.Fatalf("预置仓库应 201，得 %d：%s", rec.Code, rec.Body.String())
		}
	}

	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"新名为空", "/api/v1/repositories/raw-a/rename", `{"newName":"   "}`, http.StatusBadRequest},
		{"撞既有主名", "/api/v1/repositories/raw-a/rename", `{"newName":"raw-b"}`, http.StatusConflict},
		{"仓库不存在", "/api/v1/repositories/ghost/rename", `{"newName":"raw-c"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		if rec := serveRepoRoute(h, adminPrincipal(), http.MethodPost, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s：应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
}

// 创建时透传别名，响应与后续按名解析都应体现。
func TestCreateRepositoryWithAliases(t *testing.T) {
	h := newRepoAliasTestHandlers(t)
	rec := serveRepoRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories",
		`{"name":"raw-a","format":"raw","type":"hosted","aliases":["raw-legacy"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201，得 %d：%s", rec.Code, rec.Body.String())
	}
	var repo Repository
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if repo.Aliases == nil || !containsString(*repo.Aliases, "raw-legacy") {
		t.Fatalf("创建响应应带出别名，得 %v", repo.Aliases)
	}
}
