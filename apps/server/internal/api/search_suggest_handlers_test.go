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

// 本文件钉住的 **HTTP 契约口径**（规格 §3.8），特别是那条只能在这一层验证的硬约束：
// 可读仓库集合为空时必须返回 **200 + 空 items**，而不是 403 —— 403 会把「这里存在一个你看不到
// 的仓库」作为信号泄漏出去。
//
// 其余候选口径（两类候选、稳定序、去重、超时归一）在领域层用例
// internal/domain/search_suggest_test.go 里逐条钉住，此处不重复。

// suggestEnv 是 suggest 端点的最小环境：真实库 + 真实领域服务 + 与生产一致的中间件形状。
type suggestEnv struct {
	handlers *Handlers
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
	aclRepo  *repository.AclRepo
	users    *repository.UserRepo
	settings *domain.SettingService
}

func newSuggestEnv(t *testing.T) *suggestEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	aclRepo := repository.NewAclRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repoRepo, aclRepo, assets, settings, users)
	return &suggestEnv{
		handlers: NewHandlers(Deps{Repos: svc, Settings: settings}),
		repos:    repoRepo,
		assets:   assets,
		aclRepo:  aclRepo,
		users:    users,
		settings: settings,
	}
}

// serveSuggest 以给定主体（nil 为匿名）发起一次 suggest 请求；authMW 只注入主体。
func (e *suggestEnv) serveSuggest(principal *auth.Principal, query string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	router.GET("/api/v1/search/suggest", e.handlers.SuggestSearch)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/suggest?"+query, nil))
	return rec
}

// suggestBody 是响应体的形状（与规格 §3.8 一致：items[].type / items[].value）。
type suggestBody struct {
	Items []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"items"`
}

func decodeSuggest(t *testing.T, rec *httptest.ResponseRecorder) suggestBody {
	t.Helper()
	var body suggestBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 suggest 响应：%v（体：%s）", err, rec.Body.String())
	}
	return body
}

// TestSuggestHandlerEmptyQuery 覆盖空 q：200 + 空 items，而不是 400。
func TestSuggestHandlerEmptyQuery(t *testing.T) {
	e := newSuggestEnv(t)
	for _, query := range []string{"", "q=", "q=%20%20"} {
		rec := e.serveSuggest(adminPrincipal(), query)
		if rec.Code != http.StatusOK {
			t.Fatalf("query=%q 状态码 = %d（体：%s），期望 200", query, rec.Code, rec.Body.String())
		}
		if body := decodeSuggest(t, rec); len(body.Items) != 0 {
			t.Fatalf("query=%q 应返回空 items，得 %+v", query, body.Items)
		}
	}
}

// TestSuggestHandlerNoReadableRepoReturns200 是规格 §3.8 那条硬约束的端到端验证：
// 可读集合为空（全部仓库私有且未授权）→ 200 + 空 items，绝不 403。
func TestSuggestHandlerNoReadableRepoReturns200(t *testing.T) {
	e := newSuggestEnv(t)
	privateRepo, err := e.repos.Create("secret-repo", "maven", "hosted", "private", "{}")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := e.assets.Upsert(privateRepo, "org/demo/secretpkg/1.0.0/secretpkg-1.0.0.jar", "h", 1,
		"application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	userID, err := e.users.Create("stranger", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}

	principal := &auth.Principal{UserID: userID, Username: "stranger", Role: "user"}
	rec := e.serveSuggest(principal, "q=secretpkg")
	if rec.Code != http.StatusOK {
		t.Fatalf("无任何可读仓库时状态码 = %d（体：%s），期望 200：返回 403 会泄漏仓库存在性",
			rec.Code, rec.Body.String())
	}
	if body := decodeSuggest(t, rec); len(body.Items) != 0 {
		t.Fatalf("无任何可读仓库时应返回空 items，得 %+v", body.Items)
	}
}

// TestSuggestHandlerScopesCandidates 覆盖权限收窄与响应形状：普通用户看不到无 read 权限仓库下的
// 候选；管理员能看到；两类候选共用同一响应结构。
func TestSuggestHandlerScopesCandidates(t *testing.T) {
	e := newSuggestEnv(t)
	pubRepo, err := e.repos.Create("public-repo", "maven", "hosted", "public", "{}")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	privateRepo, err := e.repos.Create("private-repo", "maven", "hosted", "private", "{}")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := e.assets.Upsert(pubRepo, "org/demo/pubpkg/1.0.0/pubpkg-1.0.0.jar", "h", 1,
		"application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	if err := e.assets.Upsert(privateRepo, "org/demo/privpkg/1.0.0/privpkg-1.0.0.jar", "h", 1,
		"application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	userID, err := e.users.Create("plain", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	principal := &auth.Principal{UserID: userID, Username: "plain", Role: "user"}

	// 普通用户：私有的包与仓库名都不出现。
	rec := e.serveSuggest(principal, "q=p")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	body := decodeSuggest(t, rec)
	values := map[string]string{}
	for _, item := range body.Items {
		values[item.Value] = item.Type
	}
	if values["pubpkg"] != domain.SuggestTypePackage {
		t.Fatalf("普通用户应看到公开仓库的包名候选，得 %+v", body.Items)
	}
	if _, ok := values["privpkg"]; ok {
		t.Fatalf("普通用户不得看到无 read 权限仓库的包名候选，得 %+v", body.Items)
	}
	if _, ok := values["private-repo"]; ok {
		t.Fatalf("普通用户不得看到无 read 权限仓库的仓库名候选，得 %+v", body.Items)
	}
	if values["public-repo"] != domain.SuggestTypeRepository {
		t.Fatalf("普通用户应看到公开仓库的仓库名候选，得 %+v", body.Items)
	}

	// 管理员：两类候选都能看到私有仓库下的内容。
	adminRec := e.serveSuggest(adminPrincipal(), "q=p")
	if adminRec.Code != http.StatusOK {
		t.Fatalf("管理员状态码 = %d（体：%s）", adminRec.Code, adminRec.Body.String())
	}
	adminValues := map[string]string{}
	for _, item := range decodeSuggest(t, adminRec).Items {
		adminValues[item.Value] = item.Type
	}
	if adminValues["privpkg"] != domain.SuggestTypePackage {
		t.Fatalf("管理员应看到私有仓库的包名候选，得 %+v", adminValues)
	}
	if adminValues["private-repo"] != domain.SuggestTypeRepository {
		t.Fatalf("管理员应看到私有仓库的仓库名候选，得 %+v", adminValues)
	}
}

// TestSuggestHandlerAnonymousClosed 覆盖匿名开关关闭：与 /api/v1/search 一致地返回 401，
// 而不是悄悄给出空候选（那会让前端以为「服务正常但没有建议」）。
func TestSuggestHandlerAnonymousClosed(t *testing.T) {
	e := newSuggestEnv(t)
	if err := e.settings.SetAnonymousAccessEnabled(false); err != nil {
		t.Fatalf("关闭匿名访问：%v", err)
	}
	rec := e.serveSuggest(nil, "q=anything")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名访问关闭时状态码 = %d（体：%s），期望 401", rec.Code, rec.Body.String())
	}
}
