package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 仓库列表分页的回归测试：非管理员与匿名请求此前把 total 改写成「当前页可见条数」，
// 而前端（listAllRepositories）正是按 total 判断要不要继续翻页，于是可见仓库超过单页
// 上限时永远只加载第一页；公开列表则固定在默认 20 条。这里锁住两件事：
// 总数按「全部可见仓库」计，且跨批次读取不丢条目、不泄漏私有仓库。

// repoListTestEnv 是仓库列表测试环境（仅需仓库仓储与处理器，保持自足）。
type repoListTestEnv struct {
	handlers *Handlers
	repos    *repository.RepoRepo
}

func newRepoListTestEnv(t *testing.T) *repoListTestEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	if _, err := users.Create("member", "hash", "user"); err != nil {
		t.Fatalf("创建 member：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	repoSvc := domain.NewRepositoryService(
		repoRepo,
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		users,
	)
	return &repoListTestEnv{
		handlers: NewHandlers(Deps{
			Repos:    repoSvc,
			Settings: domain.NewSettingService(repository.NewSettingRepo(db)),
		}),
		repos: repoRepo,
	}
}

func (e *repoListTestEnv) seed(t *testing.T, name, visibility string) {
	t.Helper()
	if _, err := e.repos.Create(name, "raw", "hosted", visibility, "{}"); err != nil {
		t.Fatalf("创建仓库 %s：%v", name, err)
	}
}

// repoListMember 返回一个非管理员主体（普通登录用户）。
func repoListMember() *auth.Principal {
	return &auth.Principal{UserID: 2, Kind: auth.KindSession}
}

func serveRepoList(e *repoListTestEnv, principal *auth.Principal, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: e.handlers,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.GET("/api/v1/repositories", siw.ListRepositories)
	router.GET("/api/v1/public/repositories", siw.ListPublicRepositories)

	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// 普通用户与匿名列表都应跨越单批 100 条仓库后继续翻页，且只统计可读仓库。
func TestRepositoryListPaginationFiltersVisibleRepositoriesAcrossBatches(t *testing.T) {
	env := newRepoListTestEnv(t)
	const visibleCount = 101
	for i := 0; i < visibleCount; i++ {
		env.seed(t, fmt.Sprintf("list-public-%03d", i), "public")
	}
	env.seed(t, "list-private", "private")

	for _, tc := range []struct {
		name      string
		principal *auth.Principal
	}{
		{name: "普通用户", principal: repoListMember()},
		{name: "匿名用户", principal: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRepoList(env, tc.principal, "/api/v1/repositories?page=6&page_size=20")
			if rec.Code != http.StatusOK {
				t.Fatalf("仓库列表应 200，得 %d：%s", rec.Code, rec.Body.String())
			}
			var out RepositoryList
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("解析仓库列表失败：%v", err)
			}
			if out.Total != visibleCount {
				t.Fatalf("可见仓库总数应为 %d，得 %d", visibleCount, out.Total)
			}
			if len(out.Items) != 1 || out.Items[0].Name != "list-public-100" {
				t.Fatalf("第 6 页应为最后一个公开仓库，得 %+v", out.Items)
			}
		})
	}
}

// 公开列表不受默认 20 条分页限制，且不得泄漏私有仓库。
func TestListPublicRepositoriesReturnsAllVisibleRepositories(t *testing.T) {
	env := newRepoListTestEnv(t)
	const visibleCount = 21
	for i := 0; i < visibleCount; i++ {
		env.seed(t, fmt.Sprintf("public-list-%03d", i), "public")
	}
	env.seed(t, "private-list", "private")

	rec := serveRepoList(env, nil, "/api/v1/public/repositories")
	if rec.Code != http.StatusOK {
		t.Fatalf("公开列表应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var out RepositoryList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析公开列表失败：%v", err)
	}
	if out.Total != visibleCount || len(out.Items) != visibleCount {
		t.Fatalf("公开列表应完整返回 %d 个仓库，得 total=%d items=%d", visibleCount, out.Total, len(out.Items))
	}
	for _, item := range out.Items {
		if item.Name == "private-list" {
			t.Fatalf("公开列表不应泄漏私有仓库：%+v", item)
		}
	}
}
