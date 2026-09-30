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

// newRepoGovernanceTestHandlers 组装 FR-41 存储治理端点（仓库配额 / 代理缓存保留 / 空目录清理）的测试环境。
// 审计仓储一并接上：这些用例既要断言字段生效，也要断言治理变更真的留痕（含 detail 内容）。
func newRepoGovernanceTestHandlers(t *testing.T) (*Handlers, *repository.AuditLogRepo) {
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
	auditLogs := repository.NewAuditLogRepo(db)
	return NewHandlers(Deps{Repos: repoSvc, AuditLogs: auditLogs}), auditLogs
}

// serveGovernanceRoute 走生成的 ServerInterfaceWrapper 发请求（测试口径即线上口径）。
// 清理端点是非契约路由，直接挂 handler，与 main.go 的注册方式一致。
func serveGovernanceRoute(h *Handlers, principal *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
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
	router.PATCH("/api/v1/repositories/:name", siw.UpdateRepository)
	router.POST("/api/v1/repositories/:name/cleanup", h.CleanupEmptyMavenArtifacts)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// governanceTestHostedQuotaBody 是「hosted + 配额两项」的创建请求体。
// 配额只有 hosted 可设：group 不承载写入，proxy 的缓存写入在回源路径上、没有预检与早拒。
const governanceTestHostedQuotaBody = `{"name":"raw-quota","format":"raw","type":"hosted",` +
	`"quotaBytes":1048576,"quotaAssets":120}`

// governanceTestProxyBody 是「proxy + 代理缓存保留天数」的创建请求体（proxy 不接受非 0 配额）。
const governanceTestProxyBody = `{"name":"npm-cache","format":"npm","type":"proxy",` +
	`"remoteUrl":"https://registry.npmjs.org","cacheRetentionDays":30}`

// createGovernanceTestRepo 按创建请求体预置一个治理测试仓库，返回其契约响应。
func createGovernanceTestRepo(t *testing.T, h *Handlers, body string) Repository {
	t.Helper()
	rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("预置仓库应 201，得 %d：%s", rec.Code, rec.Body.String())
	}
	var repo Repository
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析创建响应：%v", err)
	}
	return repo
}

// decodeStoredConfig 直读被持久化的 config 列，用于把「响应回显」与「真的落库」分开验证。
func decodeStoredConfig(t *testing.T, h *Handlers, name string) repository.RepositoryConfig {
	t.Helper()
	row, err := h.repos.Get(name)
	if err != nil {
		t.Fatalf("读仓库 %s：%v", name, err)
	}
	cfg, err := row.DecodeConfig()
	if err != nil {
		t.Fatalf("解析 %s 的 config：%v", name, err)
	}
	return cfg
}

// 创建：治理字段必须写进 config 并回显，审计 detail 也要带上它们。
func TestCreateRepositoryPersistsGovernanceFields(t *testing.T) {
	h, auditLogs := newRepoGovernanceTestHandlers(t)
	repo := createGovernanceTestRepo(t, h, governanceTestHostedQuotaBody)

	if repo.QuotaBytes == nil || *repo.QuotaBytes != 1048576 {
		t.Fatalf("创建响应应回显 quotaBytes=1048576，得 %v", repo.QuotaBytes)
	}
	if repo.QuotaAssets == nil || *repo.QuotaAssets != 120 {
		t.Fatalf("创建响应应回显 quotaAssets=120，得 %v", repo.QuotaAssets)
	}

	cfg := decodeStoredConfig(t, h, "raw-quota")
	if cfg.QuotaBytes != 1048576 || cfg.QuotaAssets != 120 {
		t.Fatalf("config 未落库配额两项：%+v", cfg)
	}

	entries := listAudit(t, auditLogs, "repo.create")
	if len(entries) != 1 {
		t.Fatalf("创建应写 1 条 repo.create 审计，实际 %d 条", len(entries))
	}
	for _, want := range []string{"quotaBytes=1048576", "quotaAssets=120"} {
		if !strings.Contains(entries[0].Detail, want) {
			t.Errorf("审计 detail 应含 %s，得 %q", want, entries[0].Detail)
		}
	}
	if entries[0].Result != "ok" || entries[0].Repo != "raw-quota" {
		t.Fatalf("审计条目不符：%+v", entries[0])
	}

	// 代理缓存保留天数只有 proxy 可设，单独创建一次并核对回显、落库与审计。
	proxy := createGovernanceTestRepo(t, h, governanceTestProxyBody)
	if proxy.CacheRetentionDays == nil || *proxy.CacheRetentionDays != 30 {
		t.Fatalf("创建响应应回显 cacheRetentionDays=30，得 %v", proxy.CacheRetentionDays)
	}
	if proxy.QuotaBytes != nil || proxy.QuotaAssets != nil {
		t.Fatalf("proxy 仓库不得回显配额，得 quotaBytes=%v quotaAssets=%v", proxy.QuotaBytes, proxy.QuotaAssets)
	}
	proxyCfg := decodeStoredConfig(t, h, "npm-cache")
	if proxyCfg.CacheRetentionDays != 30 || proxyCfg.QuotaBytes != 0 || proxyCfg.QuotaAssets != 0 {
		t.Fatalf("proxy config 未落库保留天数：%+v", proxyCfg)
	}
	entries = listAudit(t, auditLogs, "repo.create")
	if len(entries) != 2 {
		t.Fatalf("两次创建应各写 1 条审计，实际 %d 条", len(entries))
	}
	if !strings.Contains(entries[0].Detail, "cacheRetentionDays=30") {
		t.Errorf("proxy 创建的审计 detail 应含 cacheRetentionDays=30，得 %q", entries[0].Detail)
	}
}

// 更新：nil 字段不覆盖、显式 0 生效（改为不限 / 关闭），两者都必须落库。
func TestUpdateRepositoryGovernanceFieldPointerSemantics(t *testing.T) {
	h, auditLogs := newRepoGovernanceTestHandlers(t)
	createGovernanceTestRepo(t, h, governanceTestHostedQuotaBody)
	createGovernanceTestRepo(t, h, governanceTestProxyBody)

	// 只带 quotaAssets：quotaBytes 必须原样保留（nil = 不修改）。
	rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/raw-quota",
		`{"quotaAssets":60}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("只传治理字段的更新应 200（不得被判为空请求体），得 %d：%s", rec.Code, rec.Body.String())
	}
	var repo Repository
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析更新响应：%v", err)
	}
	if repo.QuotaBytes == nil || *repo.QuotaBytes != 1048576 || repo.QuotaAssets == nil || *repo.QuotaAssets != 60 {
		t.Fatalf("未传的配额字段不得被清空，得 quotaBytes=%v quotaAssets=%v", repo.QuotaBytes, repo.QuotaAssets)
	}
	cfg := decodeStoredConfig(t, h, "raw-quota")
	if cfg.QuotaBytes != 1048576 || cfg.QuotaAssets != 60 {
		t.Fatalf("局部更新后的 config 不符：%+v", cfg)
	}
	entries := listAudit(t, auditLogs, "repo.update")
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "quotaAssets=60") {
		t.Fatalf("更新审计应只记本次传入的字段，得 %+v", entries)
	}
	if strings.Contains(entries[0].Detail, "quotaBytes=") {
		t.Errorf("未传的字段不该出现在审计 detail：%q", entries[0].Detail)
	}

	// 显式 0：两项都改成「不限」，响应不再回显（0 与缺省同义），config 里确实为 0。
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/raw-quota",
		`{"quotaBytes":0,"quotaAssets":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("显式传 0 应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	repo = Repository{}
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析更新响应：%v", err)
	}
	if repo.QuotaBytes != nil || repo.QuotaAssets != nil {
		t.Fatalf("0 = 不限，响应不应回显这两个字段，得 %v/%v", repo.QuotaBytes, repo.QuotaAssets)
	}
	cfg = decodeStoredConfig(t, h, "raw-quota")
	if cfg.QuotaBytes != 0 || cfg.QuotaAssets != 0 {
		t.Fatalf("显式 0 未落库：%+v", cfg)
	}

	// 显式 0 是一次真实变更，审计必须记下来（否则「谁关了配额」查不到）。
	entries = listAudit(t, auditLogs, "repo.update")
	if len(entries) != 2 {
		t.Fatalf("两次更新应各写 1 条审计，实际 %d 条", len(entries))
	}
	latest := entries[0].Detail
	for _, want := range []string{"quotaBytes=0", "quotaAssets=0"} {
		if !strings.Contains(latest, want) {
			t.Errorf("显式归零的审计 detail 应含 %s，得 %q", want, latest)
		}
	}

	// proxy 的保留天数同样是指针语义：显式 0 = 关闭，且同样只记本次传入的字段。
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/npm-cache",
		`{"cacheRetentionDays":7}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新保留天数应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	repo = Repository{}
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析更新响应：%v", err)
	}
	if repo.CacheRetentionDays == nil || *repo.CacheRetentionDays != 7 {
		t.Fatalf("更新响应应回显 cacheRetentionDays=7，得 %v", repo.CacheRetentionDays)
	}
	proxyCfg := decodeStoredConfig(t, h, "npm-cache")
	if proxyCfg.CacheRetentionDays != 7 {
		t.Fatalf("保留天数未落库：%+v", proxyCfg)
	}
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/npm-cache",
		`{"cacheRetentionDays":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("显式关闭保留应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	repo = Repository{}
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatalf("解析更新响应：%v", err)
	}
	if repo.CacheRetentionDays != nil {
		t.Fatalf("0 = 关闭，响应不应回显保留天数，得 %v", repo.CacheRetentionDays)
	}
	if proxyCfg = decodeStoredConfig(t, h, "npm-cache"); proxyCfg.CacheRetentionDays != 0 {
		t.Fatalf("显式关闭未落库：%+v", proxyCfg)
	}
}

// 非法值：负配额、负保留天数、在非 proxy 仓库上设置保留天数、在非 hosted 仓库上设置配额
// 都必须 400，且不得落库。
func TestRepositoryGovernanceFieldValidation(t *testing.T) {
	h, _ := newRepoGovernanceTestHandlers(t)

	createCases := []struct {
		name string
		body string
	}{
		{"hosted 上设置代理缓存保留天数", `{"name":"raw-x","format":"raw","type":"hosted","cacheRetentionDays":30}`},
		{"group 上设置代理缓存保留天数", `{"name":"grp-x","format":"raw","type":"group","members":["raw-a"],"cacheRetentionDays":1}`},
		{"负数配额", `{"name":"raw-x","format":"raw","type":"hosted","quotaBytes":-1}`},
		{"负数制品数配额", `{"name":"raw-x","format":"raw","type":"hosted","quotaAssets":-1}`},
		{"proxy 上负数保留天数", `{"name":"npm-x","format":"npm","type":"proxy","remoteUrl":"https://registry.npmjs.org","cacheRetentionDays":-1}`},
		// 配额只对 hosted 强制：proxy 的缓存写入在回源路径上、强制点尚未覆盖该路径，
		// 因此与 group 一样在配置层直接拒绝，避免"设了但永不生效"。
		{"proxy 上设置字节配额", `{"name":"npm-x","format":"npm","type":"proxy","remoteUrl":"https://registry.npmjs.org","quotaBytes":1}`},
		{"proxy 上设置制品数配额", `{"name":"npm-x","format":"npm","type":"proxy","remoteUrl":"https://registry.npmjs.org","quotaAssets":1}`},
		{"group 上设置字节配额", `{"name":"grp-x","format":"raw","type":"group","members":["raw-a"],"quotaBytes":1}`},
	}
	for _, tc := range createCases {
		rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：创建应 400，得 %d：%s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// 更新路径同样拒绝：hosted 仓库不接受保留天数，proxy 仓库不接受配额。
	if rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories",
		`{"name":"raw-a","format":"raw","type":"hosted"}`); rec.Code != http.StatusCreated {
		t.Fatalf("预置 raw-a 应 201，得 %d：%s", rec.Code, rec.Body.String())
	}
	rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/raw-a",
		`{"cacheRetentionDays":30}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("hosted 仓库更新保留天数应 400，得 %d：%s", rec.Code, rec.Body.String())
	}
	cfg := decodeStoredConfig(t, h, "raw-a")
	if cfg.CacheRetentionDays != 0 {
		t.Fatalf("非法值不得落库，得 %+v", cfg)
	}

	if rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories",
		`{"name":"npm-a","format":"npm","type":"proxy","remoteUrl":"https://registry.npmjs.org"}`); rec.Code != http.StatusCreated {
		t.Fatalf("预置 npm-a 应 201，得 %d：%s", rec.Code, rec.Body.String())
	}
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPatch, "/api/v1/repositories/npm-a",
		`{"quotaBytes":1024}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("proxy 仓库更新配额应 400，得 %d：%s", rec.Code, rec.Body.String())
	}
	if proxyCfg := decodeStoredConfig(t, h, "npm-a"); proxyCfg.QuotaBytes != 0 || proxyCfg.QuotaAssets != 0 {
		t.Fatalf("非法配额不得落库，得 %+v", proxyCfg)
	}
}

// 清理端点：成功与失败都写 repo.cleanup 审计，且响应体形状保持 {"deleted":n} 不变。
func TestCleanupEmptyMavenArtifactsWritesAudit(t *testing.T) {
	// 本次不接审计仓储：清理端点必须在不写审计时也照常返回业务结果。
	noAudit, _ := newRepoGovernanceTestHandlers(t)
	for _, spec := range []string{
		`{"name":"maven-empty","format":"maven","type":"hosted"}`,
		`{"name":"raw-a","format":"raw","type":"hosted"}`,
	} {
		if rec := serveGovernanceRoute(noAudit, adminPrincipal(), http.MethodPost, "/api/v1/repositories", spec); rec.Code != http.StatusCreated {
			t.Fatalf("预置仓库应 201，得 %d：%s", rec.Code, rec.Body.String())
		}
	}

	rec := serveGovernanceRoute(noAudit, adminPrincipal(), http.MethodPost, "/api/v1/repositories/maven-empty/cleanup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("清理应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 响应结构向后兼容：仍是且仅是 {"deleted":n}，不因新增审计而多出字段。
	if body := rec.Body.String(); body != `{"deleted":0}` {
		t.Fatalf("清理响应结构必须保持 {\"deleted\":n}，得 %s", body)
	}

	h, auditLogs := newRepoGovernanceTestHandlers(t)
	for _, spec := range []string{
		`{"name":"maven-empty","format":"maven","type":"hosted"}`,
		`{"name":"raw-a","format":"raw","type":"hosted"}`,
	} {
		if rec := serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories", spec); rec.Code != http.StatusCreated {
			t.Fatalf("预置仓库应 201，得 %d：%s", rec.Code, rec.Body.String())
		}
	}

	// 成功：deleted=0（无空目录）也要留痕，且 result=ok。
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories/maven-empty/cleanup", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("清理应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != `{"deleted":0}` {
		t.Fatalf("清理响应结构必须保持 {\"deleted\":n}，得 %s", body)
	}
	okEntries := listAudit(t, auditLogs, "repo.cleanup")
	if len(okEntries) != 1 {
		t.Fatalf("成功清理应写 1 条 repo.cleanup 审计，实际 %d 条", len(okEntries))
	}
	if okEntry := okEntries[0]; okEntry.Result != "ok" || okEntry.EntityType != "repository" ||
		okEntry.EntityKey != "maven-empty" || okEntry.Repo != "maven-empty" ||
		!strings.Contains(okEntry.Detail, "deleted=0") {
		t.Fatalf("成功清理的审计条目不符：%+v", okEntry)
	}

	// 失败：非 maven 仓库 → 400，仍要留痕（result=failed），且 detail 不得回显文件系统路径。
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories/raw-a/cleanup", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非 maven 仓库清理应 400，得 %d：%s", rec.Code, rec.Body.String())
	}
	// 仓库不存在 → 404，同样留痕。
	rec = serveGovernanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/repositories/ghost/cleanup", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在仓库清理应 404，得 %d：%s", rec.Code, rec.Body.String())
	}

	entries := listAudit(t, auditLogs, "repo.cleanup")
	if len(entries) != 3 {
		t.Fatalf("成功 1 条 + 失败 2 条 = 3 条审计，实际 %d 条", len(entries))
	}
	wantReasons := map[string]string{"raw-a": "reason=format_unsupported", "ghost": "reason=not_found"}
	for _, entry := range entries {
		if entry.Result != "failed" {
			continue
		}
		want, ok := wantReasons[entry.EntityKey]
		if !ok {
			t.Fatalf("未预期的失败留痕：%+v", entry)
		}
		if !strings.Contains(entry.Detail, want) {
			t.Errorf("%s 的失败 detail 应含 %s，得 %q", entry.EntityKey, want, entry.Detail)
		}
		if strings.ContainsAny(entry.Detail, `/\:`) {
			t.Errorf("审计 detail 不得含文件系统路径：%q", entry.Detail)
		}
		delete(wantReasons, entry.EntityKey)
	}
	if len(wantReasons) != 0 {
		t.Fatalf("失败路径应各有 1 条留痕，缺失：%v", wantReasons)
	}
	// 失败留痕要带上调用方真正看到的状态码，便于按「4xx」回溯。
	for _, entry := range entries {
		if entry.Result != "failed" {
			continue
		}
		if entry.StatusCode != http.StatusBadRequest && entry.StatusCode != http.StatusNotFound {
			t.Errorf("%s 的审计状态码应与响应一致，得 %d", entry.EntityKey, entry.StatusCode)
		}
	}
}

// listAudit 按动作查审计（时间倒序，最新在前）。
func listAudit(t *testing.T, logs *repository.AuditLogRepo, action string) []repository.AuditLogEntry {
	t.Helper()
	entries, err := logs.List(repository.AuditFilter{Action: action})
	if err != nil {
		t.Fatalf("查审计 %s：%v", action, err)
	}
	return entries
}
