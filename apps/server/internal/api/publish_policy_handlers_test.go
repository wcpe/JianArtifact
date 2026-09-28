package api

// 发布策略响应的 allowedPrefixes 必须是 JSON 数组（契约 required），不能是 null：
// 管理端 value.allowedPrefixes.join(...) 在 null 上会抛
// TypeError: Cannot read properties of null (reading 'join')。

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

// GET（无策略行）与 PUT（显式空数组）两种来源都必须返回 []。
func TestPublishPolicyResponsePrefixesNeverNull(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	rec := servePublishPolicy(h, adminPrincipal(), http.MethodGet, "/api/v1/users/1/publish-policies/raw-a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 发布策略应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	assertAllowedPrefixesIsArray(t, rec.Body.String())

	rec = servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies/raw-a", `{"allowedPrefixes":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 发布策略应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	assertAllowedPrefixesIsArray(t, rec.Body.String())
}

// 非管理员不得读取发布策略（与既有鉴权边界一致，防测试环境误放宽）。
func TestPublishPolicyRequiresAdmin(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	if rec := servePublishPolicy(h, nil, http.MethodGet, "/api/v1/users/1/publish-policies/raw-a", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读发布策略应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := servePublishPolicy(h, userPrincipal(), http.MethodGet, "/api/v1/users/1/publish-policies/raw-a", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员读发布策略应 403，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// assertAllowedPrefixesIsArray 断言 allowedPrefixes 字段存在且为 JSON 数组（不是 null）。
func assertAllowedPrefixesIsArray(t *testing.T, body string) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析响应：%v（体：%s）", err, body)
	}
	raw, ok := payload["allowedPrefixes"]
	if !ok {
		t.Fatalf("响应缺少 allowedPrefixes：%s", body)
	}
	if string(raw) == "null" {
		t.Fatalf("allowedPrefixes 为 null（应为 []），前端 .join 会抛 TypeError：%s", body)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Fatalf("allowedPrefixes 不是数组：%s", raw)
	}
}

// newPublishPolicyTestHandlers 装配发布策略端点所需依赖，并预置一个管理员与两个 hosted
// 仓库、一个 proxy 仓库（批量端点需要多仓库与「非 hosted」样本）。
func newPublishPolicyTestHandlers(t *testing.T) *Handlers {
	t.Helper()
	db := openAPITestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	userRepo := repository.NewUserRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	repoSvc := domain.NewRepositoryService(
		repoRepo,
		repository.NewAclRepo(db),
		assetRepo,
		domain.NewSettingService(repository.NewSettingRepo(db)),
		userRepo,
	)
	if _, err := userRepo.Create("policy-admin", "hash", "admin"); err != nil {
		t.Fatalf("建管理员：%v", err)
	}
	for _, name := range []string{"raw-a", "raw-b"} {
		if _, err := repoRepo.Create(name, "raw", "hosted", "private", ""); err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
	}
	if _, err := repoRepo.Create("maven-proxy", "maven", "proxy", "public", `{"remoteUrl":"https://repo.example.com"}`); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}
	return NewHandlers(Deps{
		Repos:           repoSvc,
		Users:           domain.NewUserService(userRepo),
		PublishPolicies: domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repoRepo, assetRepo),
	})
}

// servePublishPolicy 经生成的 ServerInterfaceWrapper 发起请求（测试口径即线上口径）。
func servePublishPolicy(h *Handlers, principal *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.ReleaseMode)
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
	router.GET("/api/v1/users/:id/publish-policies/:repo", siw.GetPublishPolicy)
	router.PUT("/api/v1/users/:id/publish-policies/:repo", siw.PutPublishPolicy)
	router.PUT("/api/v1/users/:id/publish-policies", siw.PutPublishPolicies)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// batchPolicyResults 解析批量端点的逐仓库结果。
type batchPolicyResults struct {
	Results []struct {
		Repository string  `json:"repository"`
		OK         bool    `json:"ok"`
		Error      *string `json:"error"`
	} `json:"results"`
}

// 批量端点：多仓库全部成功时逐仓库返回 ok，且每个仓库都能回读到同一份策略。
func TestPutPublishPoliciesBatchAppliesToAllRepositories(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	rec := servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a","raw-b"],"allowedPrefixes":["releases"],"maxAssetsHour":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("批量保存应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var payload batchPolicyResults
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析批量结果：%v（体：%s）", err, rec.Body.String())
	}
	if len(payload.Results) != 2 {
		t.Fatalf("应逐仓库返回 2 条结果，得 %d：%s", len(payload.Results), rec.Body.String())
	}
	for i, want := range []string{"raw-a", "raw-b"} {
		if payload.Results[i].Repository != want || !payload.Results[i].OK {
			t.Fatalf("第 %d 条结果应为 %s/ok，得 %+v", i, want, payload.Results[i])
		}
		if payload.Results[i].Error != nil {
			t.Fatalf("成功项不得带 error：%+v", payload.Results[i])
		}
	}

	// 逐仓库回读：两个仓库都落库了同一份策略。
	for _, name := range []string{"raw-a", "raw-b"} {
		got := servePublishPolicy(h, adminPrincipal(), http.MethodGet,
			"/api/v1/users/1/publish-policies/"+name, "")
		if got.Code != http.StatusOK {
			t.Fatalf("回读 %s 应 200，得 %d：%s", name, got.Code, got.Body.String())
		}
		if !strings.Contains(got.Body.String(), `"releases"`) {
			t.Fatalf("%s 应落库新前缀：%s", name, got.Body.String())
		}
		if !strings.Contains(got.Body.String(), `"maxAssetsHour":5`) {
			t.Fatalf("%s 应落库新额度：%s", name, got.Body.String())
		}
	}
}

// 批量端点：仓库名重复时去重，避免同一仓库写两次、结果列表出现重复行。
func TestPutPublishPoliciesBatchDeduplicatesRepositories(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	rec := servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a","raw-a"],"allowedPrefixes":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("批量保存应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var payload batchPolicyResults
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析批量结果：%v", err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("重复仓库名应去重为 1 条结果，得 %d：%s", len(payload.Results), rec.Body.String())
	}
}

// 批量端点：含不存在的仓库时整体拒绝，错误点名该仓库，且合法仓库也不得落库。
func TestPutPublishPoliciesBatchRejectsUnknownRepositoryAsWhole(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	rec := servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a","ghost"],"allowedPrefixes":["releases"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("含不存在仓库应 404，得 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Fatalf("错误信息必须点名问题仓库：%s", rec.Body.String())
	}

	got := servePublishPolicy(h, adminPrincipal(), http.MethodGet,
		"/api/v1/users/1/publish-policies/raw-a", "")
	if strings.Contains(got.Body.String(), `"releases"`) {
		t.Fatalf("整体拒绝后不得写入任何仓库：%s", got.Body.String())
	}
}

// 批量端点：含非 hosted 仓库时同样整体拒绝（400），并点名该仓库。
func TestPutPublishPoliciesBatchRejectsNonHostedRepository(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	rec := servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a","maven-proxy"],"allowedPrefixes":["releases"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("含非 hosted 仓库应 400，得 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maven-proxy") {
		t.Fatalf("错误信息必须点名问题仓库：%s", rec.Body.String())
	}
}

// 批量端点：空仓库列表（含缺失字段）返回 400，不做任何写入。
func TestPutPublishPoliciesBatchRejectsEmptyRepositories(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	for _, body := range []string{`{"repositories":[],"allowedPrefixes":[]}`, `{"allowedPrefixes":[]}`} {
		rec := servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("空仓库列表应 400（体 %s），得 %d：%s", body, rec.Code, rec.Body.String())
		}
	}
}

// 批量端点：拒绝写入 immutableRelease（仓库级配置），与单仓库端点口径一致。
func TestPutPublishPoliciesBatchRejectsImmutableReleaseWrite(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	rec := servePublishPolicy(h, adminPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a"],"immutableRelease":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("写入 immutableRelease 应 400，得 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "immutable_release_moved") {
		t.Fatalf("应返回 immutable_release_moved：%s", rec.Body.String())
	}
}

// 批量端点：鉴权边界与单仓库一致——匿名 401、非管理员 403。
func TestPutPublishPoliciesBatchRequiresAdmin(t *testing.T) {
	h := newPublishPolicyTestHandlers(t)

	if rec := servePublishPolicy(h, nil, http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a"]}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名批量保存应 401，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := servePublishPolicy(h, userPrincipal(), http.MethodPut, "/api/v1/users/1/publish-policies",
		`{"repositories":["raw-a"]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员批量保存应 403，得 %d：%s", rec.Code, rec.Body.String())
	}
}
