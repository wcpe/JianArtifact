package httpserver_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	jwtMgr := auth.NewJWTManager([]byte("integration-test-secret-key-32byte!!"))
	authenticator := auth.NewAuthenticator(jwtMgr, domain.NewAuthStore(userRepo, tokenRepo, revokedRepo))

	handlers := api.NewHandlers(api.Deps{
		Version:   "test",
		Checks:    []func() error{db.Ping},
		Migration: db.CurrentVersion,
		Auth:      domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		Users:     domain.NewUserService(userRepo),
		Tokens:    domain.NewTokenService(tokenRepo),
		Repos:     domain.NewRepositoryService(repoRepo, aclRepo),
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

// TestStatusReportsInitialized 自举后 /api/v1/status 反映已初始化与用户数。
func TestStatusReportsInitialized(t *testing.T) {
	e := newTestEnv(t)

	var before api.StatusInfo
	if code := e.do(t, http.MethodGet, "/api/v1/status", "", nil, &before); code != http.StatusOK {
		t.Fatalf("status 状态码 = %d，期望 200", code)
	}
	if before.Initialized || before.UserCount != 0 {
		t.Errorf("空库 status 应未初始化且用户数 0，实得 %+v", before)
	}
	if before.MigrationVersion == "" {
		t.Error("status 应报告迁移版本")
	}

	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, nil); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}

	var after api.StatusInfo
	if code := e.do(t, http.MethodGet, "/api/v1/status", "", nil, &after); code != http.StatusOK {
		t.Fatalf("status 状态码 = %d，期望 200", code)
	}
	if !after.Initialized || after.UserCount != 1 {
		t.Errorf("自举后 status 应已初始化且用户数 1，实得 %+v", after)
	}
}
