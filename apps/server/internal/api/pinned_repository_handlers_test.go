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

// pinnedTestEnv 是置顶端点测试环境：处理器 + 仓库仓储（预置仓库用）+ 置顶仓储。
type pinnedTestEnv struct {
	handlers *Handlers
	repos    *repository.RepoRepo
	pinned   *repository.PinnedRepoRepo
}

// newPinnedTestEnv 组装置顶端点测试环境：admin(id=1)/bob(id=2) 两个用户（外键需要），
// 匿名访问开关默认开启（键缺失即开启，FR-66）。
func newPinnedTestEnv(t *testing.T) *pinnedTestEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	if _, err := users.Create("bob", "hash", "user"); err != nil {
		t.Fatalf("创建 bob：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	repoSvc := domain.NewRepositoryService(
		repoRepo,
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		users,
	)
	return &pinnedTestEnv{
		handlers: NewHandlers(Deps{
			Repos:    repoSvc,
			Settings: domain.NewSettingService(repository.NewSettingRepo(db)),
			Pinned:   repository.NewPinnedRepoRepo(db),
		}),
		repos:  repoRepo,
		pinned: repository.NewPinnedRepoRepo(db),
	}
}

// seed 建一个仓库并返回 ID（visibility 影响匿名可读性）。
func (e *pinnedTestEnv) seed(t *testing.T, name, visibility string) int64 {
	t.Helper()
	id, err := e.repos.Create(name, "raw", "hosted", visibility, "{}")
	if err != nil {
		t.Fatalf("创建仓库 %s：%v", name, err)
	}
	return id
}

// servePinned 经生成的 ServerInterfaceWrapper 发起请求（测试口径即线上口径）。
// principal 为 nil 表示匿名（不注入主体，与 authenticator.Optional() 行为一致）。
func servePinned(env *pinnedTestEnv, principal *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: env.handlers,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.GET("/api/v1/me/pinned-repositories", siw.GetMyPinnedRepositories)
	router.PUT("/api/v1/me/pinned-repositories", siw.PutMyPinnedRepositories)
	router.GET("/api/v1/settings/pinned-repositories", siw.GetGlobalPinnedRepositories)
	router.PUT("/api/v1/settings/pinned-repositories", siw.PutGlobalPinnedRepositories)
	router.GET("/api/v1/public/repositories", siw.ListPublicRepositories)
	router.GET("/api/v1/repositories", siw.ListRepositories)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decodePinned 解析置顶响应。
func decodePinned(t *testing.T, rec *httptest.ResponseRecorder) PinnedRepositoriesResponse {
	t.Helper()
	var out PinnedRepositoriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析置顶响应失败：%v（body=%s）", err, rec.Body.String())
	}
	return out
}

// 鉴权矩阵：写匿名 401、普通用户可写自己的、普通用户写全局 403、管理员可写全局。
func TestPinnedWriteAuthMatrix(t *testing.T) {
	env := newPinnedTestEnv(t)
	a := env.seed(t, "pin-a", "public")
	b := env.seed(t, "pin-b", "public")

	userBody := fmt.Sprintf(`{"repositoryIds":[%d]}`, a)
	// 匿名写自己的置顶：401（匿名写入没有归属主体）。
	if rec := servePinned(env, nil, http.MethodPut, "/api/v1/me/pinned-repositories", userBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名写用户置顶应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 普通用户写自己的：200。
	rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", userBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("普通用户写自己置顶应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	if got := decodePinned(t, rec); len(got.RepositoryIds) != 1 || got.RepositoryIds[0] != a || got.Names[0] != "pin-a" {
		t.Fatalf("用户置顶响应异常：%+v", got)
	}

	globalBody := fmt.Sprintf(`{"repositoryIds":[%d]}`, b)
	// 普通用户写全局：403。
	if rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/settings/pinned-repositories", globalBody); rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户写全局置顶应 403，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 匿名写全局：401。
	if rec := servePinned(env, nil, http.MethodPut, "/api/v1/settings/pinned-repositories", globalBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名写全局置顶应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 匿名读全局：401。
	if rec := servePinned(env, nil, http.MethodGet, "/api/v1/settings/pinned-repositories", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读全局置顶应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 管理员写全局：200。
	if rec := servePinned(env, adminPrincipal(), http.MethodPut, "/api/v1/settings/pinned-repositories", globalBody); rec.Code != http.StatusOK {
		t.Fatalf("管理员写全局置顶应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 管理员读全局：200 且为刚写入的集合。
	if rec := servePinned(env, adminPrincipal(), http.MethodGet, "/api/v1/settings/pinned-repositories", ""); rec.Code != http.StatusOK {
		t.Fatalf("管理员读全局置顶应 200，得 %d：%s", rec.Code, rec.Body.String())
	} else if got := decodePinned(t, rec); len(got.RepositoryIds) != 1 || got.RepositoryIds[0] != b {
		t.Fatalf("全局置顶响应异常：%+v", got)
	}
}

// 读作用域：登录用户读自己的置顶；匿名回退全局置顶；用户级与全局互不覆盖。
func TestPinnedReadScopes(t *testing.T) {
	env := newPinnedTestEnv(t)
	a := env.seed(t, "pin-a", "public")
	b := env.seed(t, "pin-b", "public")

	if err := env.pinned.Set(nil, []int64{a}); err != nil {
		t.Fatalf("写全局置顶：%v", err)
	}
	if err := env.pinned.Set(ptrInt64(2), []int64{b}); err != nil {
		t.Fatalf("写用户置顶：%v", err)
	}

	// 登录用户（bob, id=2）读到的是自己的 [b]。
	rec := servePinned(env, userPrincipal(), http.MethodGet, "/api/v1/me/pinned-repositories", "")
	if got := decodePinned(t, rec); len(got.Names) != 1 || got.Names[0] != "pin-b" {
		t.Fatalf("用户应读到自己的置顶 [pin-b]，得 %+v", got)
	}
	// 匿名读到的是全局 [a]。
	rec = servePinned(env, nil, http.MethodGet, "/api/v1/me/pinned-repositories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("匿名读置顶应 200（回退全局），得 %d：%s", rec.Code, rec.Body.String())
	}
	if got := decodePinned(t, rec); len(got.Names) != 1 || got.Names[0] != "pin-a" {
		t.Fatalf("匿名应回退全局置顶 [pin-a]，得 %+v", got)
	}
}

// 覆盖式写入：整体替换、空数组清空。
func TestPinnedWriteIsOverwriting(t *testing.T) {
	env := newPinnedTestEnv(t)
	a := env.seed(t, "pin-a", "public")
	b := env.seed(t, "pin-b", "public")

	if rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", fmt.Sprintf(`{"repositoryIds":[%d,%d]}`, a, b)); rec.Code != http.StatusOK {
		t.Fatalf("首次写入应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 第二次只写 a：b 不再保留。
	rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", fmt.Sprintf(`{"repositoryIds":[%d]}`, a))
	if got := decodePinned(t, rec); len(got.RepositoryIds) != 1 || got.RepositoryIds[0] != a {
		t.Fatalf("覆盖写入后应只剩 a，得 %+v", got)
	}
	// 空数组清空。
	rec = servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", `{"repositoryIds":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	if got := decodePinned(t, rec); len(got.RepositoryIds) != 0 {
		t.Fatalf("清空后应为空，得 %+v", got)
	}
}

// 非法仓库 ID：404 且不改动既有置顶。
func TestPinnedWriteUnknownRepositoryIs404(t *testing.T) {
	env := newPinnedTestEnv(t)
	a := env.seed(t, "pin-a", "public")

	if rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", fmt.Sprintf(`{"repositoryIds":[%d]}`, a)); rec.Code != http.StatusOK {
		t.Fatalf("预置置顶应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", fmt.Sprintf(`{"repositoryIds":[%d,999999]}`, a))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知仓库应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 失败写入不产生副作用。
	rec = servePinned(env, userPrincipal(), http.MethodGet, "/api/v1/me/pinned-repositories", "")
	if got := decodePinned(t, rec); len(got.RepositoryIds) != 1 || got.RepositoryIds[0] != a {
		t.Fatalf("失败写入不应改动既有置顶，得 %+v", got)
	}
}

// 公开列表携带全局置顶名：只含匿名可读者，私有仓库的全局置顶不外泄。
func TestListPublicRepositoriesIncludesGlobalPinnedNames(t *testing.T) {
	env := newPinnedTestEnv(t)
	pubA := env.seed(t, "pub-a", "public")
	pubB := env.seed(t, "pub-b", "public")
	priv := env.seed(t, "priv-c", "private")

	if err := env.pinned.Set(nil, []int64{pubB, priv, pubA}); err != nil {
		t.Fatalf("写全局置顶：%v", err)
	}

	rec := servePinned(env, nil, http.MethodGet, "/api/v1/public/repositories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("公开列表应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var out PublicRepositoryList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析公开列表失败：%v", err)
	}
	// 顺序即置顶顺序；私有仓库（priv-c）被过滤，不泄漏名字。
	want := []string{"pub-b", "pub-a"}
	if len(out.PinnedNames) != len(want) {
		t.Fatalf("全局置顶名应为 %v，得 %v", want, out.PinnedNames)
	}
	for i := range want {
		if out.PinnedNames[i] != want[i] {
			t.Fatalf("全局置顶名应为 %v，得 %v", want, out.PinnedNames)
		}
	}
}

// 未接线置顶仓储时写入明确失败（409），不静默成功。
func TestPinnedWriteWithoutStoreConflicts(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	h := NewHandlers(Deps{})
	env := &pinnedTestEnv{handlers: h}
	rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", `{"repositoryIds":[]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("置顶仓储未接线应 409，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 可见性边界：不可读的仓库既不能置顶，也不出现在响应里（不泄漏私有仓库名）。
func TestPinnedRespectsRepositoryReadability(t *testing.T) {
	env := newPinnedTestEnv(t)
	pub := env.seed(t, "pin-public", "public")
	priv := env.seed(t, "pin-private", "private")

	// 普通用户（无 ACL）置顶私有仓库：视同不存在 → 404，且不产生副作用。
	rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", fmt.Sprintf(`{"repositoryIds":[%d]}`, priv))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("置顶不可读仓库应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 普通用户可以置顶公开仓库。
	if rec := servePinned(env, userPrincipal(), http.MethodPut, "/api/v1/me/pinned-repositories", fmt.Sprintf(`{"repositoryIds":[%d]}`, pub)); rec.Code != http.StatusOK {
		t.Fatalf("置顶公开仓库应 200，得 %d：%s", rec.Code, rec.Body.String())
	}

	// 仓储层直接写入「私有仓库」的全局置顶与用户置顶（模拟权限变更等旁路）。
	if err := env.pinned.Set(nil, []int64{priv}); err != nil {
		t.Fatalf("写全局置顶：%v", err)
	}
	if err := env.pinned.Set(ptrInt64(2), []int64{pub, priv}); err != nil {
		t.Fatalf("写用户置顶：%v", err)
	}

	// 匿名读全局置顶：私有仓库名被过滤。
	rec = servePinned(env, nil, http.MethodGet, "/api/v1/me/pinned-repositories", "")
	if got := decodePinned(t, rec); len(got.Names) != 0 {
		t.Fatalf("匿名不应看到私有仓库的全局置顶，得 %+v", got)
	}
	// 普通用户读自己的置顶：私有仓库名同样被过滤。
	rec = servePinned(env, userPrincipal(), http.MethodGet, "/api/v1/me/pinned-repositories", "")
	if got := decodePinned(t, rec); len(got.Names) != 1 || got.Names[0] != "pin-public" {
		t.Fatalf("普通用户只应看到可读仓库，得 %+v", got)
	}
	// 管理员读全局置顶：不过滤（需要能管理私有仓库的全局置顶）。
	rec = servePinned(env, adminPrincipal(), http.MethodGet, "/api/v1/settings/pinned-repositories", "")
	if got := decodePinned(t, rec); len(got.Names) != 1 || got.Names[0] != "pin-private" {
		t.Fatalf("管理员应看到全部全局置顶，得 %+v", got)
	}
}

func ptrInt64(v int64) *int64 { return &v }
