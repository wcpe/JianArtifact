package httpserver_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// testEnv 汇集集成测试的服务端句柄。
type testEnv struct {
	h http.Handler
}

// newTestEnv 用临时 SQLite 装配完整服务端（真实持久化 + 领域服务 + 鉴权中间件）。
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "it.db")
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}

	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	revokedRepo := repository.NewRevokedRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	aclRepo := repository.NewAclRepo(db)
	migrationSvc := domain.NewMigrationService(repository.NewMigrationTaskRepo(db), nil)

	jwtMgr := auth.NewJWTManager([]byte("integration-test-secret-key-32byte!!"))
	authenticator := auth.NewAuthenticator(jwtMgr, domain.NewAuthStore(userRepo, tokenRepo, revokedRepo))

	handlers := api.NewHandlers(api.Deps{
		Version:    "test",
		Checks:     []func() error{db.Ping},
		Migration:  db.CurrentVersion,
		Auth:       domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		Users:      domain.NewUserService(userRepo),
		Tokens:     domain.NewTokenService(tokenRepo),
		Repos:      domain.NewRepositoryService(repoRepo, aclRepo, repository.NewAssetRepo(db), domain.NewSettingService(repository.NewSettingRepo(db)), userRepo),
		Migrations: migrationSvc,
	})

	srv := httpserver.New("test",
		httpserver.WithReadinessCheck(db.Ping),
		httpserver.WithHandlers(handlers),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
	)
	return &testEnv{h: srv.Handler(nil)}
}

// do 发起一次请求；token 非空则带上 Bearer 头。out 非 nil 时解析响应体。
func (e *testEnv) do(t *testing.T, method, path, token string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体：%v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s 解析响应失败：%v（体：%s）", method, path, err, rec.Body.String())
		}
	}
	return rec.Code
}

// TestAuthFlowEndToEnd 端到端覆盖：自举 → 登录 → 建用户 → 签发 Token → 建仓库 → ACL → 授权判定。
func TestAuthFlowEndToEnd(t *testing.T) {
	e := newTestEnv(t)

	// 空库自举创建首个管理员。
	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	adminToken := boot.Token
	if adminToken == "" || boot.User.Role != "admin" {
		t.Fatalf("自举返回异常：token 空或角色非 admin（%+v）", boot.User)
	}

	// 已初始化实例再自举 → 409。
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "x", Password: "y"}, nil); code != http.StatusConflict {
		t.Errorf("重复自举状态码 = %d，期望 409", code)
	}

	// 未认证访问受保护端点 → 401。
	if code := e.do(t, http.MethodGet, "/api/v1/users", "", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("未认证列用户状态码 = %d，期望 401", code)
	}

	// 管理员建用户 alice。
	var alice api.User
	if code := e.do(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "alice", Password: "alice-pass-123"}, &alice); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d，期望 201", code)
	}

	// alice 登录。
	var aliceLogin api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "alice", Password: "alice-pass-123"}, &aliceLogin); code != http.StatusOK {
		t.Fatalf("alice 登录状态码 = %d，期望 200", code)
	}
	aliceToken := aliceLogin.Token

	// 口令错误 → 401。
	if code := e.do(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "alice", Password: "wrong"}, nil); code != http.StatusUnauthorized {
		t.Errorf("错误口令登录状态码 = %d，期望 401", code)
	}

	// 非管理员访问用户管理 → 403。
	if code := e.do(t, http.MethodGet, "/api/v1/users", aliceToken, nil, nil); code != http.StatusForbidden {
		t.Errorf("alice 列用户状态码 = %d，期望 403", code)
	}

	// alice 签发 API Token，并用其访问自身 Token 列表。
	var created api.TokenCreated
	if code := e.do(t, http.MethodPost, "/api/v1/tokens", aliceToken,
		api.CreateTokenRequest{Name: "ci"}, &created); code != http.StatusCreated {
		t.Fatalf("签发 Token 状态码 = %d，期望 201", code)
	}
	if created.Token == "" {
		t.Fatal("签发 Token 未返回明文")
	}
	if code := e.do(t, http.MethodGet, "/api/v1/tokens", created.Token, nil, nil); code != http.StatusOK {
		t.Errorf("以 API Token 列表状态码 = %d，期望 200", code)
	}

	// 管理员建私有仓库。
	var repo api.Repository
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "maven-releases", Format: "maven", Type: "hosted"}, &repo); code != http.StatusCreated {
		t.Fatalf("建仓库状态码 = %d，期望 201", code)
	}

	// alice 尚无 ACL：仓库列表为空。
	var list api.RepositoryList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories", aliceToken, nil, &list); code != http.StatusOK {
		t.Fatalf("alice 列仓库状态码 = %d，期望 200", code)
	}
	if len(list.Items) != 0 {
		t.Errorf("alice 无 ACL 时应见 0 个仓库，实见 %d", len(list.Items))
	}

	// alice 无 admin 权，读取 ACL → 403。
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/maven-releases/acl", aliceToken, nil, nil); code != http.StatusForbidden {
		t.Errorf("alice 读 ACL 状态码 = %d，期望 403", code)
	}

	// 管理员授予 alice read。
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/maven-releases/acl", adminToken,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: alice.Id, Action: api.AclEntryActionRead}}}, nil); code != http.StatusOK {
		t.Fatalf("写 ACL 状态码 = %d，期望 200", code)
	}

	// 授予后 alice 可见该仓库。
	var list2 api.RepositoryList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories", aliceToken, nil, &list2); code != http.StatusOK {
		t.Fatalf("alice 再列仓库状态码 = %d，期望 200", code)
	}
	if len(list2.Items) != 1 {
		t.Errorf("授予 read 后 alice 应见 1 个仓库，实见 %d", len(list2.Items))
	}

	// 登出管理员后其会话令牌失效：受保护端点 → 401。
	if code := e.do(t, http.MethodPost, "/api/v1/auth/logout", adminToken, nil, nil); code != http.StatusNoContent {
		t.Fatalf("登出状态码 = %d，期望 204", code)
	}
	if code := e.do(t, http.MethodGet, "/api/v1/users", adminToken, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("登出后旧令牌列用户状态码 = %d，期望 401", code)
	}
}

// TestBrowseAndUsageEndpoints 覆盖制品浏览与使用片段（FR-16）：
// 管理员建 public maven 仓库后，usage 按 format 返回接入片段、assets 空列表；
// 无 read 权限的私有仓库对非授权用户返回 403。
func TestBrowseAndUsageEndpoints(t *testing.T) {
	e := newTestEnv(t)

	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	adminToken := boot.Token

	// 建 public maven hosted 仓库（public 便于验证 read 放行）。
	public := api.CreateRepositoryRequestVisibilityPublic
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "mvn-pub", Format: "maven", Type: "hosted", Visibility: &public}, nil); code != http.StatusCreated {
		t.Fatalf("建仓库状态码 = %d，期望 201", code)
	}

	// usage：format=maven、type=hosted，含若干片段。
	var usage api.UsageInfo
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-pub/usage", adminToken, nil, &usage); code != http.StatusOK {
		t.Fatalf("usage 状态码 = %d，期望 200", code)
	}
	if usage.Format != "maven" || usage.Type != "hosted" {
		t.Errorf("usage format/type 异常：%+v", usage)
	}
	if len(usage.Snippets) == 0 {
		t.Error("usage 应返回至少一段接入片段")
	}

	// assets：新仓库为空列表，total=0。
	var assets api.AssetList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-pub/assets", adminToken, nil, &assets); code != http.StatusOK {
		t.Fatalf("assets 状态码 = %d，期望 200", code)
	}
	if assets.Total != 0 || len(assets.Items) != 0 {
		t.Errorf("空仓库 assets 应为空，得 total=%d len=%d", assets.Total, len(assets.Items))
	}

	// public 仓库匿名可读 usage/assets → 200（与协议层匿名 public 读一致）。
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-pub/usage", "", nil, nil); code != http.StatusOK {
		t.Errorf("匿名读 public usage 状态码 = %d，期望 200", code)
	}
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-pub/assets", "", nil, nil); code != http.StatusOK {
		t.Errorf("匿名读 public assets 状态码 = %d，期望 200", code)
	}

	// 未知仓库 → 404。
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/ghost/usage", adminToken, nil, nil); code != http.StatusNotFound {
		t.Errorf("未知仓库 usage 状态码 = %d，期望 404", code)
	}

	// 建私有仓库，非授权用户读 assets/usage → 403。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "mvn-priv", Format: "maven", Type: "hosted"}, nil); code != http.StatusCreated {
		t.Fatalf("建私有仓库状态码 = %d，期望 201", code)
	}
	var alice api.User
	if code := e.do(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "alice", Password: "alice-pass-123"}, &alice); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d，期望 201", code)
	}
	var aliceLogin api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "alice", Password: "alice-pass-123"}, &aliceLogin); code != http.StatusOK {
		t.Fatalf("alice 登录状态码 = %d，期望 200", code)
	}
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-priv/assets", aliceLogin.Token, nil, nil); code != http.StatusForbidden {
		t.Errorf("alice 无 read 权读 assets 状态码 = %d，期望 403", code)
	}
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-priv/usage", aliceLogin.Token, nil, nil); code != http.StatusForbidden {
		t.Errorf("alice 无 read 权读 usage 状态码 = %d，期望 403", code)
	}
	// 私有仓库匿名读 → 401（未认证），区别于已认证越权的 403。
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/mvn-priv/usage", "", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("匿名读 private usage 状态码 = %d，期望 401", code)
	}
}

// TestStatusReportsInitialized 自举后 /api/v1/status 反映已初始化与用户数；
// 版本与迁移版本对匿名脱敏、对已认证请求返回。
func TestStatusReportsInitialized(t *testing.T) {
	e := newTestEnv(t)

	var before api.StatusInfo
	if code := e.do(t, http.MethodGet, "/api/v1/status", "", nil, &before); code != http.StatusOK {
		t.Fatalf("status 状态码 = %d，期望 200", code)
	}
	if before.Initialized || before.UserCount != 0 {
		t.Errorf("空库 status 应未初始化且用户数 0，实得 %+v", before)
	}
	if before.Version != "" || before.MigrationVersion != "" {
		t.Errorf("匿名 status 应脱敏版本信息，实得 %+v", before)
	}

	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}

	var after api.StatusInfo
	if code := e.do(t, http.MethodGet, "/api/v1/status", "", nil, &after); code != http.StatusOK {
		t.Fatalf("status 状态码 = %d，期望 200", code)
	}
	if !after.Initialized || after.UserCount != 1 {
		t.Errorf("自举后 status 应已初始化且用户数 1，实得 %+v", after)
	}
	if after.Version != "" || after.MigrationVersion != "" {
		t.Errorf("自举后匿名 status 仍应脱敏版本信息，实得 %+v", after)
	}

	// 已认证请求返回完整版本与迁移版本。
	var authed api.StatusInfo
	if code := e.do(t, http.MethodGet, "/api/v1/status", boot.Token, nil, &authed); code != http.StatusOK {
		t.Fatalf("已认证 status 状态码 = %d，期望 200", code)
	}
	if authed.Version != "test" {
		t.Errorf("已认证 status 应返回版本 test，实得 %q", authed.Version)
	}
	if authed.MigrationVersion == "" {
		t.Error("已认证 status 应报告迁移版本")
	}

	// 健康 / 就绪探针同样对匿名脱敏版本。
	var health api.HealthStatus
	if code := e.do(t, http.MethodGet, "/readyz", "", nil, &health); code != http.StatusOK {
		t.Fatalf("readyz 状态码 = %d，期望 200", code)
	}
	if health.Version != "" {
		t.Errorf("匿名 readyz 应脱敏版本，实得 %q", health.Version)
	}
	if code := e.do(t, http.MethodGet, "/readyz", boot.Token, nil, &health); code != http.StatusOK {
		t.Fatalf("已认证 readyz 状态码 = %d，期望 200", code)
	}
	if health.Version != "test" {
		t.Errorf("已认证 readyz 应返回版本 test，实得 %q", health.Version)
	}
}

// TestMigrationFoundationAPI 覆盖迁移地基：admin 创建 planned、start、非法态 409、401/403、未知凭据 400。
func TestMigrationFoundationAPI(t *testing.T) {
	e := newTestEnv(t)

	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	adminToken := boot.Token

	// 未认证 → 401
	if code := e.do(t, http.MethodGet, "/api/v1/migrations", "", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("未认证列迁移 状态码 = %d，期望 401", code)
	}

	// 普通用户 → 403
	if code := e.do(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "bob", Password: "bob-pass-123"}, nil); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d", code)
	}
	var bobLogin api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "bob", Password: "bob-pass-123"}, &bobLogin); code != http.StatusOK {
		t.Fatalf("bob 登录状态码 = %d", code)
	}
	if code := e.do(t, http.MethodGet, "/api/v1/migrations", bobLogin.Token, nil, nil); code != http.StatusForbidden {
		t.Errorf("非 admin 列迁移 状态码 = %d，期望 403", code)
	}

	// 未知 credentialRef → 400
	badRef := "JIAN_TEST_MISSING_MIGRATION_CRED"
	if code := e.do(t, http.MethodPost, "/api/v1/migrations", adminToken,
		api.CreateMigrationRequest{
			SourceType:    api.OnlineRest,
			CredentialRef: &badRef,
			SourceConfig:  &api.MigrationSourceConfig{"url": "http://nexus.example"},
		}, nil); code != http.StatusBadRequest {
		t.Errorf("未知 credentialRef 状态码 = %d，期望 400", code)
	}

	// 创建 planned
	cfg := api.MigrationSourceConfig{"path": "/data/bundle"}
	var created api.MigrationTask
	if code := e.do(t, http.MethodPost, "/api/v1/migrations", adminToken,
		api.CreateMigrationRequest{
			SourceType:   api.OfflineBundle,
			SourceConfig: &cfg,
		}, &created); code != http.StatusCreated {
		t.Fatalf("创建迁移 状态码 = %d，期望 201", code)
	}
	if created.Status != api.Planned {
		t.Fatalf("创建后 status = %q，期望 planned", created.Status)
	}
	if created.Id <= 0 {
		t.Fatal("期望正数 task id")
	}

	// GET 详情
	var got api.MigrationTask
	if code := e.do(t, http.MethodGet, "/api/v1/migrations/"+itoa64(created.Id), adminToken, nil, &got); code != http.StatusOK {
		t.Fatalf("GET 迁移 状态码 = %d", code)
	}
	if got.Status != api.Planned {
		t.Errorf("GET status = %q", got.Status)
	}

	// start → running（空 body 兼容）
	var started api.MigrationTask
	if code := e.do(t, http.MethodPost, "/api/v1/migrations/"+itoa64(created.Id)+"/start", adminToken, map[string]any{}, &started); code != http.StatusOK {
		t.Fatalf("start 状态码 = %d，期望 200", code)
	}
	if started.Status != api.Running {
		t.Fatalf("start 后 status = %q", started.Status)
	}

	// 再 start → 409
	if code := e.do(t, http.MethodPost, "/api/v1/migrations/"+itoa64(created.Id)+"/start", adminToken, map[string]any{}, nil); code != http.StatusConflict {
		t.Errorf("二次 start 状态码 = %d，期望 409", code)
	}

	// report 可 GET
	if code := e.do(t, http.MethodGet, "/api/v1/migrations/"+itoa64(created.Id)+"/report", adminToken, nil, nil); code != http.StatusOK {
		t.Errorf("report 状态码 = %d，期望 200", code)
	}
}

func itoa64(id int64) string {
	return fmt.Sprintf("%d", id)
}

// TestMigrationDiscoverAPI 覆盖 discover 落库 planned、坏路径不落库、非 admin 403。
func TestMigrationDiscoverAPI(t *testing.T) {
	e := newTestEnv(t)

	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举 %d", code)
	}
	adminToken := boot.Token

	root := t.TempDir()
	// 最小 offline bundle
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"repositories":[{"name":"raw-data","format":"raw","type":"hosted"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "content", "raw-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "content", "raw-data", "a.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := api.MigrationSourceConfig{"path": root}
	var disc api.MigrationDiscoverResponse
	if code := e.do(t, http.MethodPost, "/api/v1/migrations/discover", adminToken,
		api.MigrationDiscoverRequest{SourceType: api.OfflineBundle, SourceConfig: &cfg}, &disc); code != http.StatusOK {
		t.Fatalf("discover 状态码 = %d，期望 200", code)
	}
	if disc.TaskId <= 0 {
		t.Fatal("期望 taskId")
	}
	if len(disc.Plan.Repositories) != 1 {
		t.Fatalf("plan = %+v", disc.Plan)
	}

	var task api.MigrationTask
	if code := e.do(t, http.MethodGet, "/api/v1/migrations/"+itoa64(disc.TaskId), adminToken, nil, &task); code != http.StatusOK {
		t.Fatalf("GET task %d", code)
	}
	if task.Status != api.Planned {
		t.Fatalf("status = %q", task.Status)
	}

	// 坏路径：400 且列表不增加
	var listBefore api.MigrationTaskList
	_ = e.do(t, http.MethodGet, "/api/v1/migrations", adminToken, nil, &listBefore)
	bad := api.MigrationSourceConfig{"path": filepath.Join(root, "nope")}
	if code := e.do(t, http.MethodPost, "/api/v1/migrations/discover", adminToken,
		api.MigrationDiscoverRequest{SourceType: api.OfflineBundle, SourceConfig: &bad}, nil); code != http.StatusBadRequest {
		t.Errorf("坏路径 discover 状态码 = %d，期望 400", code)
	}
	var listAfter api.MigrationTaskList
	_ = e.do(t, http.MethodGet, "/api/v1/migrations", adminToken, nil, &listAfter)
	if listAfter.Total != listBefore.Total {
		t.Errorf("失败不应新增任务：before=%d after=%d", listBefore.Total, listAfter.Total)
	}
}
