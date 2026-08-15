package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestSettingsEndpoints 设置端点（FR-89）：鉴权、默认生效值、部分写入回读、非法拒绝、
// 回源超时回调触发。
func TestSettingsEndpoints(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "settings-handler.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(db))
	var timeoutUpdated *time.Duration
	handlers := api.NewHandlers(api.Deps{
		Settings: settingSvc,
		OnUpstreamTimeoutChange: func(d time.Duration) {
			timeoutUpdated = &d
		},
	})

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// 测试用鉴权中间件：按请求头模拟 admin / user 主体。
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Role") == "admin" {
			c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		} else {
			c.Set("auth.principal", &auth.Principal{Role: "user", Username: "u", UserID: 2})
		}
		c.Next()
	})
	r.GET("/api/v1/settings", handlers.GetSettings)
	r.PUT("/api/v1/settings", handlers.PutSettings)

	// 非 admin GET → 403。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非 admin GET 应 403，得 %d", rec.Code)
	}

	// admin GET → 200 且缺省生效值正确（匿名开、URL 空、超时 30、间隔 5）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET 应 200，得 %d", rec.Code)
	}
	var def api.SettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &def); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if !def.AnonymousAccess || def.PublicURL != "" || def.UpstreamTimeout != 30 || def.SyncInterval != 5 {
		t.Errorf("缺省设置值不符：%+v", def)
	}

	// admin PUT 全字段 → 200 回显新值。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"anonymousAccess":false,"publicUrl":"https://cdn.example","upstreamTimeout":45,"syncInterval":10}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT 应 200，得 %d（体：%s）", rec.Code, rec.Body.String())
	}
	var after api.SettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if after.AnonymousAccess || after.PublicURL != "https://cdn.example" || after.UpstreamTimeout != 45 || after.SyncInterval != 10 {
		t.Errorf("PUT 后设置值不符：%+v", after)
	}
	// 回源超时回调已触发。
	if timeoutUpdated == nil || *timeoutUpdated != 45*time.Second {
		t.Errorf("OnUpstreamTimeoutChange 未按 45s 触发，得 %v", timeoutUpdated)
	}

	// admin GET → 写后读一致。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("X-Test-Role", "admin")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET 应 200，得 %d", rec.Code)
	}
	var reread api.SettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reread); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if reread.PublicURL != "https://cdn.example" || reread.UpstreamTimeout != 45 || reread.SyncInterval != 10 {
		t.Errorf("写后读回不符：%+v", reread)
	}

	// 非法值 → 400：超时越界、间隔越界、非 http URL。
	for _, body := range []string{
		`{"upstreamTimeout":0}`,
		`{"upstreamTimeout":3601}`,
		`{"syncInterval":-1}`,
		`{"publicUrl":"ftp://x"}`,
	} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Role", "admin")
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s 应 400，得 %d", body, rec.Code)
		}
	}

	// 非 admin PUT → 403。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(`{"syncInterval":10}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Role", "user")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非 admin PUT 应 403，得 %d", rec.Code)
	}
}

// TestSettingsPublicURLDynamicEffect 写 publicUrl 后 usage 立即用新值（FR-89 对外 URL 动态生效）。
func TestSettingsPublicURLDynamicEffect(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "settings-usage.db"))
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
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(db))
	jwtMgr := auth.NewJWTManager([]byte("integration-test-secret-key-32byte!!"))
	authenticator := auth.NewAuthenticator(jwtMgr, domain.NewAuthStore(userRepo, tokenRepo, revokedRepo))

	handlers := api.NewHandlers(api.Deps{
		Version:   "test",
		Checks:    []func() error{db.Ping},
		Migration: db.CurrentVersion,
		Auth:      domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		Users:     domain.NewUserService(userRepo),
		Tokens:    domain.NewTokenService(tokenRepo, userRepo),
		Repos:     domain.NewRepositoryService(repoRepo, aclRepo, repository.NewAssetRepo(db), settingSvc, userRepo),
		Settings:  settingSvc,
	})
	srv := httpserver.New("test",
		httpserver.WithHandlers(handlers),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
		httpserver.WithProtocolRoutes(func(r gin.IRouter) {
			authMW := authenticator.Optional()
			r.GET("/api/v1/settings", authMW, handlers.GetSettings)
			r.PUT("/api/v1/settings", authMW, handlers.PutSettings)
		}),
	)
	e := &testEnv{h: srv.Handler(nil)}

	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	adminToken := boot.Token

	vis := api.CreateRepositoryRequestVisibilityPublic
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "raw-pub", Format: "raw", Type: "hosted", Visibility: &vis}, nil); code != http.StatusCreated {
		t.Fatalf("建仓库状态码 = %d，期望 201", code)
	}

	// 未配置 publicUrl：usage 用请求 Host 推断（httptest 请求 Host 为 example.com）。
	var usage api.UsageInfo
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/raw-pub/usage", adminToken, nil, &usage); code != http.StatusOK {
		t.Fatalf("usage 状态码 = %d，期望 200", code)
	}
	if strings.Contains(usage.Snippets[0].Code, "https://cdn.example") {
		t.Errorf("未配置 URL 时不应使用 https://cdn.example，得：%s", usage.Snippets[0].Code)
	}

	// PUT publicUrl 后：usage 立即用新值。
	if code := e.do(t, http.MethodPut, "/api/v1/settings", adminToken,
		map[string]any{"publicUrl": "https://cdn.example"}, nil); code != http.StatusOK {
		t.Fatalf("PUT settings 状态码 = %d，期望 200", code)
	}
	if code := e.do(t, http.MethodGet, "/api/v1/repositories/raw-pub/usage", adminToken, nil, &usage); code != http.StatusOK {
		t.Fatalf("usage 状态码 = %d，期望 200", code)
	}
	if !strings.Contains(usage.Snippets[0].Code, "https://cdn.example") {
		t.Errorf("写 publicUrl 后 usage 应使用新值，得：%s", usage.Snippets[0].Code)
	}
}
