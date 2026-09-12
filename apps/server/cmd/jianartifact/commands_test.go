package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestAdminResetCreatesThenResets 覆盖 admin reset 两条路径：
// 无该管理员则创建；已存在则改密。改密后旧口令失效、新口令可登录。
func TestAdminResetCreatesThenResets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "admin-reset-test-secret-key-32byte!!")

	// 首次：无管理员 → 创建。
	if err := adminReset([]string{"--username", "admin", "--password", "first-pass-123"}); err != nil {
		t.Fatalf("首次 reset（创建）：%v", err)
	}
	if !canLogin(t, "admin", "first-pass-123") {
		t.Fatal("创建后应能以初始口令登录")
	}

	// 再次：已存在 → 改密。
	if err := adminReset([]string{"--username", "admin", "--password", "second-pass-456"}); err != nil {
		t.Fatalf("二次 reset（改密）：%v", err)
	}
	if canLogin(t, "admin", "first-pass-123") {
		t.Error("改密后旧口令不应再登录")
	}
	if !canLogin(t, "admin", "second-pass-456") {
		t.Error("改密后应能以新口令登录")
	}
}

// TestAdminResetChangesPasswordForAlreadyOpenedHTTPService 模拟服务已打开 SQLite，
// 管理员离线重置后服务进程继续处理登录请求的实际运维顺序。
func TestAdminResetChangesPasswordForAlreadyOpenedHTTPService(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "reset-open-http-service-test-secret-key")
	if err := adminReset([]string{"--username", "t1accept", "--password", "before-reset-123"}); err != nil {
		t.Fatalf("创建管理员：%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配 HTTP 服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	h := newApplicationHandler(cfg, svc, fstest.MapFS{})

	login := func(password string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"t1accept","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := login("before-reset-123"); got != http.StatusOK {
		t.Fatalf("重置前登录状态 = %d，期望 200", got)
	}

	if err := adminReset([]string{"--username", "t1accept", "--password", "after-reset-456"}); err != nil {
		t.Fatalf("离线重置管理员：%v", err)
	}
	if got := login("before-reset-123"); got != http.StatusUnauthorized {
		t.Fatalf("旧口令登录状态 = %d，期望 401", got)
	}
	if got := login("after-reset-456"); got != http.StatusOK {
		t.Fatalf("同一已打开 HTTP 服务的新口令登录状态 = %d，期望 200", got)
	}
}

func TestAdminResetReenablesWebLoginForExistingAdmin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "admin-reset-web-login-test-secret-key")

	if err := adminReset([]string{"--username", "admin", "--password", "before-reset"}); err != nil {
		t.Fatalf("创建管理员：%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	admin, err := svc.users.GetByUsername("admin")
	if err != nil {
		_ = svc.db.Close()
		t.Fatalf("读取管理员：%v", err)
	}
	disabled := true
	if err := svc.users.Update(admin.ID, "", "", &disabled); err != nil {
		_ = svc.db.Close()
		t.Fatalf("禁用网页登录：%v", err)
	}
	if err := svc.db.Close(); err != nil {
		t.Fatalf("关闭服务：%v", err)
	}

	if err := adminReset([]string{"--username", "admin", "--password", "after-reset"}); err != nil {
		t.Fatalf("重置管理员：%v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("重载配置：%v", err)
	}
	svc, err = openServices(cfg)
	if err != nil {
		t.Fatalf("重装服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	h := newApplicationHandler(cfg, svc, fstest.MapFS{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"after-reset"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员 reset 后 HTTP 登录状态=%d，响应=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminResetRejectsEmptyPassword 非交互环境下缺省口令应报错（不静默创建空口令账号）。
func TestAdminResetRejectsEmptyPassword(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "admin-reset-test-secret-key-32byte!!")

	// 未提供 --password 且 stdin 非终端：readPasswordTwice 返回错误。
	if err := adminReset([]string{"--username", "admin"}); err == nil {
		t.Fatal("非交互式且无 --password 时应报错")
	}
}

// canLogin 用离线服务校验给定口令能否登录。
func canLogin(t *testing.T, username, password string) bool {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	defer func() { _ = svc.db.Close() }()
	_, _, err = svc.authSvc.Login(username, password)
	return err == nil
}

// TestUsageListsCommands usage 输出应列出全部子命令，供无参数 / help / 未知命令时展示。
func TestUsageListsCommands(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	out := buf.String()
	for _, want := range []string{"run", "status", "admin reset", "healthcheck", "help", "JIAN_HTTP_ADDR"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage 输出应包含 %q，实际：\n%s", want, out)
		}
	}
}

// TestOpenServicesAppliesPersistedSettings 启动装配应优先应用持久化设置：
// 回源超时覆盖环境默认，public URL 显式清空后重启仍保持清空。
func TestOpenServicesAppliesPersistedSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "settings-startup-test-secret-32bytes!")
	t.Setenv(config.EnvUpstreamTimeout, "30")
	t.Setenv(config.EnvPublicURL, "https://env.example")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	first, err := openServices(cfg)
	if err != nil {
		t.Fatalf("首次装配服务：%v", err)
	}
	if err := first.settingSvc.SetUpstreamTimeout(1); err != nil {
		t.Fatalf("持久化回源超时：%v", err)
	}
	if err := first.settingSvc.SetPublicURL(""); err != nil {
		t.Fatalf("清空 public URL：%v", err)
	}
	if err := first.db.Close(); err != nil {
		t.Fatalf("关闭首次数据库：%v", err)
	}

	second, err := openServices(cfg)
	if err != nil {
		t.Fatalf("再次装配服务：%v", err)
	}
	t.Cleanup(func() { _ = second.db.Close() })
	if got := second.upstreamClient.Timeout(); got != time.Second {
		t.Errorf("启动后回源超时 = %s，期望 1s", got)
	}
	if got := second.settingSvc.PublicURL(); got != "" {
		t.Errorf("显式清空的 public URL 重启后应保持空串，得 %q", got)
	}
	if second.publicURL != "" {
		t.Errorf("handler 静态 public URL 应为空以允许请求 Host 回退，得 %q", second.publicURL)
	}
}

// TestReplicationCredentialMiddleware 复制端点只接受节点专属凭据和严格 GET 白名单。
func TestApplicationHandlerHonorsEnabledFormats(t *testing.T) {
	newHandler := func(t *testing.T, enabled string) http.Handler {
		t.Helper()
		t.Setenv(config.EnvDataDir, t.TempDir())
		t.Setenv(config.EnvJWTSecret, "format-activation-test-secret-key")
		t.Setenv(config.EnvEnabledFormats, enabled)
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("加载配置：%v", err)
		}
		svc, err := openServices(cfg)
		if err != nil {
			t.Fatalf("装配服务：%v", err)
		}
		t.Cleanup(func() { _ = svc.db.Close() })
		if _, err := repository.NewRepoRepo(svc.db).Create("maven-disabled", "maven", "hosted", "public", ""); err != nil {
			t.Fatalf("写入已存在的禁用 Maven 仓库：%v", err)
		}
		if enabled == "raw" {
			if _, err := svc.repoSvc.Create("raw-live", "raw", "hosted", "public", "", repository.RepositoryConfig{}); err != nil {
				t.Fatalf("创建启用的 Raw 仓库：%v", err)
			}
			if _, err := svc.assetSvc.Put("raw-live", "f.txt", strings.NewReader("raw"), "text/plain"); err != nil {
				t.Fatalf("写入 Raw 制品：%v", err)
			}
		}
		return newApplicationHandler(cfg, svc, fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte("index")},
		})
	}
	request := func(t *testing.T, handler http.Handler, method, path string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != want {
			t.Fatalf("%s %s 状态 = %d，期望 %d（体：%s）", method, path, rec.Code, want, rec.Body.String())
		}
		if want == http.StatusNotFound && rec.Body.String() == "index" {
			t.Fatalf("%s %s 不得回退到 SPA", method, path)
		}
	}

	t.Run("仅 raw 时共享前缀隔离 Maven 且不注册 npm", func(t *testing.T) {
		handler := newHandler(t, "raw")
		request(t, handler, http.MethodGet, "/repository/raw-live/f.txt", http.StatusOK)
		request(t, handler, http.MethodGet, "/repository/maven-disabled/f.jar", http.StatusNotFound)
		request(t, handler, http.MethodGet, "/npm/npm-disabled/pkg", http.StatusNotFound)
		request(t, handler, http.MethodPost, "/api/v1/repositories/maven-disabled/maven-upload", http.StatusNotFound)
	})

	t.Run("空格式列表时不注册任何协议路由", func(t *testing.T) {
		handler := newHandler(t, "")
		request(t, handler, http.MethodGet, "/repository/raw-disabled/f.txt", http.StatusNotFound)
		request(t, handler, http.MethodGet, "/npm/npm-disabled/pkg", http.StatusNotFound)
		request(t, handler, http.MethodGet, "/pypi/pypi-disabled/simple/pkg/", http.StatusNotFound)
	})
}

func TestApplicationHandlerRejectsDisabledFormatUsageAndMavenCleanup(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "disabled-format-management-test-secret-key")
	t.Setenv(config.EnvEnabledFormats, "raw")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	if _, err := repository.NewRepoRepo(svc.db).Create("maven-disabled", "maven", "hosted", "private", "{}"); err != nil {
		t.Fatalf("写入已存在的禁用 Maven 仓库：%v", err)
	}
	token, _, err := svc.authSvc.Bootstrap("admin", "admin-pass-123")
	if err != nil {
		t.Fatalf("自举管理员：%v", err)
	}
	handler := newApplicationHandler(cfg, svc, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}})

	for _, request := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/repositories/maven-disabled/usage"},
		{method: http.MethodPost, path: "/api/v1/repositories/maven-disabled/cleanup"},
	} {
		t.Run(request.method+request.path, func(t *testing.T) {
			req := httptest.NewRequest(request.method, request.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("状态码 = %d，期望 409：%s", rec.Code, rec.Body.String())
			}
			var body api.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析错误响应：%v", err)
			}
			if body.Error.Code != "format_disabled" {
				t.Fatalf("错误码 = %q，期望 format_disabled", body.Error.Code)
			}
		})
	}
}

func TestApplicationHandlerEnabledFormatsRequiresAdminAndReturnsConfiguredSet(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "enabled-formats-api-test-secret-key")
	t.Setenv(config.EnvEnabledFormats, "npm,raw")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	adminToken, _, err := svc.authSvc.Bootstrap("admin", "admin-pass-123")
	if err != nil {
		t.Fatalf("自举管理员：%v", err)
	}
	if _, err := svc.userSvc.Create("reader", "reader-pass-123", "user"); err != nil {
		t.Fatalf("创建普通用户：%v", err)
	}
	userToken, _, err := svc.authSvc.Login("reader", "reader-pass-123")
	if err != nil {
		t.Fatalf("普通用户登录：%v", err)
	}
	handler := newApplicationHandler(cfg, svc, nil)

	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/formats/enabled", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名读取状态码 = %d，期望 401", rec.Code)
	}
	if rec := request(userToken); rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户读取状态码 = %d，期望 403", rec.Code)
	}
	rec := request(adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员读取状态码 = %d，期望 200：%s", rec.Code, rec.Body.String())
	}
	var body api.EnabledFormats
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析启用格式响应：%v", err)
	}
	if got := []api.EnabledFormatsFormats(body.Formats); len(got) != 2 || got[0] != "npm" || got[1] != "raw" {
		t.Fatalf("启用格式 = %v，期望 [npm raw]", got)
	}
}

func TestApplicationHandlerDisabledProtocolPrefixesReturn404WithoutUpstreamAccess(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "disabled-prefixes-test-secret-key")
	t.Setenv(config.EnvEnabledFormats, "raw")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	for _, item := range []struct {
		name   string
		format string
	}{
		{name: "docker-disabled", format: "docker"},
		{name: "cargo-disabled", format: "cargo"},
		{name: "gomod-disabled", format: "gomod"},
		{name: "nuget-disabled", format: "nuget"},
	} {
		if _, err := repository.NewRepoRepo(svc.db).Create(item.name, item.format, "proxy", "public", `{"remoteUrl":"`+upstream.URL+`"}`); err != nil {
			t.Fatalf("写入禁用 %s 仓库：%v", item.format, err)
		}
	}
	handler := newApplicationHandler(cfg, svc, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}})
	for _, path := range []string{
		"/v2/docker-disabled/manifests/latest",
		"/cargo/cargo-disabled/index/demo",
		"/go/gomod-disabled/example.com/demo/@v/list",
		"/nuget/nuget-disabled/v3/index.json",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound || rec.Body.String() == "index" {
				t.Fatalf("%s 状态=%d body=%q，期望非 HTML 404", path, rec.Code, rec.Body.String())
			}
		})
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("禁用协议前缀不得访问上游，实际请求数=%d", got)
	}
}

// TestApplicationHandlerLoginJWTAuthenticatesManagementWrite 覆盖真实启动装配：
// HTTP 登录签发的 JWT 必须立即可用于管理写入，不能只允许 Basic 账号口令。
func TestApplicationHandlerLoginJWTAuthenticatesManagementWrite(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "login-jwt-management-write-test-secret-key")
	t.Setenv(config.EnvEnabledFormats, "raw")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	server := httptest.NewServer(newApplicationHandler(cfg, svc, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}))
	t.Cleanup(server.Close)

	request := func(method, path, token string, body, out any) int {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求：%v", err)
		}
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("构造 %s %s 请求：%v", method, path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("执行 %s %s 请求：%v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取 %s %s 响应：%v", method, path, err)
		}
		if out != nil && len(responseBody) > 0 {
			if err := json.Unmarshal(responseBody, out); err != nil {
				t.Fatalf("解析 %s %s 响应：%v（体：%s）", method, path, err, responseBody)
			}
		}
		return resp.StatusCode
	}

	var bootstrap api.LoginResponse
	if code := request(http.MethodPost, "/api/v1/auth/bootstrap", "", api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &bootstrap); code != http.StatusCreated {
		t.Fatalf("HTTP 自举状态 = %d", code)
	}
	var login api.LoginResponse
	if code := request(http.MethodPost, "/api/v1/auth/login", "", api.LoginRequest{Username: "admin", Password: "admin-pass-123"}, &login); code != http.StatusOK || login.Token == "" {
		t.Fatalf("HTTP 登录状态 = %d，token 长度=%d", code, len(login.Token))
	}
	format := api.CreateRepositoryRequestFormat("raw")
	repoType := api.CreateRepositoryRequestType("hosted")
	if code := request(http.MethodPost, "/api/v1/repositories", login.Token, api.CreateRepositoryRequest{
		Name: "jwt-created", Format: format, Type: repoType,
	}, nil); code != http.StatusCreated {
		t.Fatalf("登录 JWT 创建仓库状态 = %d，期望 201", code)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道：%v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
		_ = w.Close()
		_ = r.Close()
	}()
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	fn()
	_ = w.Close()
	return <-done
}
