package httpserver_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/protocol"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// protocolEnv 汇集含协议层路由的服务端句柄。
type protocolEnv struct {
	h http.Handler
}

// newProtocolEnv 装配完整服务端：契约路由 + Raw 协议路由（真实持久化 + blob 存储）。
func newProtocolEnv(t *testing.T) *protocolEnv {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "proto.db"))
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
	assetRepo := repository.NewAssetRepo(db)

	jwtMgr := auth.NewJWTManager([]byte("integration-test-secret-key-32byte!!"))
	authenticator := auth.NewAuthenticator(jwtMgr, domain.NewAuthStore(userRepo, tokenRepo, revokedRepo))

	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobstore.NewStore(t.TempDir()), upstream.NewClient(5*time.Second))
	rawHandler := protocol.NewRawHandler(assetSvc, repoSvc)
	mavenHandler := protocol.NewMavenHandler(rawHandler)
	dispatcher := protocol.NewDispatcher(repoSvc, rawHandler, mavenHandler)
	npmHandler := protocol.NewNpmHandler(rawHandler)

	handlers := api.NewHandlers(api.Deps{
		Version:   "test",
		Checks:    []func() error{db.Ping},
		Migration: db.CurrentVersion,
		Auth:      domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		Users:     domain.NewUserService(userRepo),
		Tokens:    domain.NewTokenService(tokenRepo),
		Repos:     repoSvc,
	})

	srv := httpserver.New("test",
		httpserver.WithReadinessCheck(db.Ping),
		httpserver.WithHandlers(handlers),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
		httpserver.WithProtocolRoutes(func(r gin.IRouter) {
			protocol.RegisterRoutes(r, dispatcher, authenticator.Optional())
			protocol.RegisterNpmRoutes(r, npmHandler, authenticator.Optional())
		}),
	)
	return &protocolEnv{h: srv.Handler(nil)}
}

// jsonReq 发起契约 API 请求（JSON）；token 非空带 Bearer 头。返回状态码。
func (e *protocolEnv) jsonReq(t *testing.T, method, path, token string, body, out any) int {
	t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体：%v", err)
		}
		reader = bytes.NewReader(raw)
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

// rawReq 发起协议层请求（任意字节）；auth 为完整 Authorization 头值（空则不带）。
func (e *protocolEnv) rawReq(method, path, authHeader, contentType string, body []byte) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// basicHeader 构造 Authorization: Basic（token 作密码，用户名任意）。
func basicHeader(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("token:"+token))
}

// bootstrapAdmin 自举首个管理员并返回其会话令牌。
func (e *protocolEnv) bootstrapAdmin(t *testing.T) string {
	t.Helper()
	var boot api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	return boot.Token
}

// createRawRepo 以管理员身份创建 raw hosted 仓库（visibility 为 private/public）。
func (e *protocolEnv) createRawRepo(t *testing.T, adminToken, name, visibility string) {
	t.Helper()
	vis := api.CreateRepositoryRequestVisibility(visibility)
	req := api.CreateRepositoryRequest{
		Name:       name,
		Format:     api.CreateRepositoryRequestFormat("raw"),
		Type:       api.CreateRepositoryRequestType("hosted"),
		Visibility: &vis,
	}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, req, nil); code != http.StatusCreated {
		t.Fatalf("建仓库 %s 状态码 = %d，期望 201", name, code)
	}
}

func TestRawHostedRoundtripBearerAndBasic(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")

	// 管理员签发 API Token（jat_），用于 Basic 鉴权。
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", adminToken,
		api.CreateTokenRequest{Name: "curl"}, &created); code != http.StatusCreated {
		t.Fatalf("签发 Token 状态码 = %d，期望 201", code)
	}
	apiToken := created.Token

	payload := []byte("raw hosted payload via bearer")

	// 经 Bearer PUT。
	rec := e.rawReq(http.MethodPut, "/repository/raw-hosted/dir/a.txt", "Bearer "+adminToken, "text/plain", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Bearer PUT 状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
	}

	// 经 Bearer GET，字节级一致 + ETag + Content-Type。
	rec = e.rawReq(http.MethodGet, "/repository/raw-hosted/dir/a.txt", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d，期望 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("GET 内容与写入不一致：%q", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q，期望 text/plain", ct)
	}
	if etag := rec.Header().Get("ETag"); etag == "" || etag == `""` {
		t.Errorf("ETag 应为非空 blob 摘要，实得 %q", etag)
	}

	// 经 Basic（API Token 作密码）PUT 到另一路径。
	payload2 := []byte("raw hosted payload via basic auth")
	rec = e.rawReq(http.MethodPut, "/repository/raw-hosted/dir/b.bin", basicHeader(apiToken), "application/octet-stream", payload2)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Basic PUT 状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
	}
	rec = e.rawReq(http.MethodGet, "/repository/raw-hosted/dir/b.bin", basicHeader(apiToken), "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload2) {
		t.Fatalf("Basic GET 状态码 = %d 或内容不符：%q", rec.Code, rec.Body.Bytes())
	}

	// HEAD：头齐全，无 body。
	rec = e.rawReq(http.MethodHead, "/repository/raw-hosted/dir/a.txt", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d，期望 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD 不应有 body，实得 %d 字节", rec.Body.Len())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("HEAD 应设置 ETag")
	}

	// 未知路径 → 404。
	rec = e.rawReq(http.MethodGet, "/repository/raw-hosted/does/not/exist", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知路径 GET 状态码 = %d，期望 404", rec.Code)
	}
}

func TestRawHostedAccessControl(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")

	// 先由管理员放入一件制品，供后续读权限测试。
	if rec := e.rawReq(http.MethodPut, "/repository/raw-hosted/f.txt", "Bearer "+adminToken, "text/plain", []byte("secret")); rec.Code != http.StatusCreated {
		t.Fatalf("准备制品失败：%d", rec.Code)
	}

	// 私有仓匿名读 → 401。
	if rec := e.rawReq(http.MethodGet, "/repository/raw-hosted/f.txt", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("私有仓匿名读状态码 = %d，期望 401", rec.Code)
	}

	// 建 alice（无 ACL），其 API Token 无 write 权 → PUT 403。
	var alice api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "alice", Password: "alice-pass-123"}, &alice); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d", code)
	}
	var aliceLogin api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "alice", Password: "alice-pass-123"}, &aliceLogin); code != http.StatusOK {
		t.Fatalf("alice 登录状态码 = %d", code)
	}
	var aliceTok api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", aliceLogin.Token,
		api.CreateTokenRequest{Name: "ci"}, &aliceTok); code != http.StatusCreated {
		t.Fatalf("alice 签发 Token 状态码 = %d", code)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/raw-hosted/x.txt", "Bearer "+aliceTok.Token, "text/plain", []byte("nope")); rec.Code != http.StatusForbidden {
		t.Errorf("无 write 权 PUT 状态码 = %d，期望 403", rec.Code)
	}

	// public 仓匿名可读。
	e.createRawRepo(t, adminToken, "raw-public", "public")
	if rec := e.rawReq(http.MethodPut, "/repository/raw-public/p.txt", "Bearer "+adminToken, "text/plain", []byte("hello public")); rec.Code != http.StatusCreated {
		t.Fatalf("public 仓 PUT 状态码 = %d", rec.Code)
	}
	rec := e.rawReq(http.MethodGet, "/repository/raw-public/p.txt", "", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello public" {
		t.Errorf("public 仓匿名读状态码 = %d，内容 = %q", rec.Code, rec.Body.String())
	}
}

func TestMavenHostedDispatchedNotRejected(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)

	// maven hosted 仓库现由 Dispatcher 按 format 分派到 MavenHandler，PUT 应 201（非再 409）。
	vis := api.CreateRepositoryRequestVisibility("private")
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, api.CreateRepositoryRequest{
		Name:       "maven-releases",
		Format:     api.CreateRepositoryRequestFormat("maven"),
		Type:       api.CreateRepositoryRequestType("hosted"),
		Visibility: &vis,
	}, nil); code != http.StatusCreated {
		t.Fatalf("建 maven 仓库状态码 = %d", code)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/maven-releases/a.jar", "Bearer "+adminToken, "application/java-archive", []byte("x")); rec.Code != http.StatusCreated {
		t.Errorf("maven-hosted PUT 状态码 = %d，期望 201", rec.Code)
	}
}

// createRawProxyRepo 以管理员身份创建指向 remoteURL 的 raw proxy 仓库。
func (e *protocolEnv) createRawProxyRepo(t *testing.T, adminToken, name, remoteURL string) {
	t.Helper()
	vis := api.CreateRepositoryRequestVisibility("public")
	req := api.CreateRepositoryRequest{
		Name:       name,
		Format:     api.CreateRepositoryRequestFormat("raw"),
		Type:       api.CreateRepositoryRequestType("proxy"),
		Visibility: &vis,
		RemoteUrl:  &remoteURL,
	}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, req, nil); code != http.StatusCreated {
		t.Fatalf("建 proxy 仓库 %s 状态码 = %d，期望 201", name, code)
	}
}

// createRawGroupRepo 以管理员身份创建含有序 members 的 raw group 仓库。
func (e *protocolEnv) createRawGroupRepo(t *testing.T, adminToken, name string, members ...string) {
	t.Helper()
	vis := api.CreateRepositoryRequestVisibility("public")
	req := api.CreateRepositoryRequest{
		Name:       name,
		Format:     api.CreateRepositoryRequestFormat("raw"),
		Type:       api.CreateRepositoryRequestType("group"),
		Visibility: &vis,
		Members:    &members,
	}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, req, nil); code != http.StatusCreated {
		t.Fatalf("建 group 仓库 %s 状态码 = %d，期望 201", name, code)
	}
}

func TestRawProxyGetFetchesUpstream(t *testing.T) {
	var hits int32
	upstreamBody := []byte("bytes from upstream registry")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(upstreamBody)
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawProxyRepo(t, adminToken, "raw-proxy", srv.URL)

	// 首次 GET：回源并缓存。
	rec := e.rawReq(http.MethodGet, "/repository/raw-proxy/vendor/lib.jar", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy GET 状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), upstreamBody) {
		t.Fatalf("proxy GET 内容与上游不一致：%q", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/java-archive" {
		t.Errorf("proxy GET Content-Type = %q，期望透传上游值", ct)
	}

	// 二次 GET：命中本地缓存，不再回源。
	rec = e.rawReq(http.MethodGet, "/repository/raw-proxy/vendor/lib.jar", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), upstreamBody) {
		t.Fatalf("proxy 缓存 GET 状态码 = %d 或内容不符", rec.Code)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("期望仅回源 1 次，实际 %d 次", n)
	}

	// 上游 404 的路径 → 协议层 404。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nf", http.StatusNotFound)
	}))
	defer srv2.Close()
	e.createRawProxyRepo(t, adminToken, "raw-proxy-nf", srv2.URL)
	if rec := e.rawReq(http.MethodGet, "/repository/raw-proxy-nf/x.jar", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("上游 404 GET 状态码 = %d，期望 404", rec.Code)
	}
}

func TestRawGroupAggregatesReads(t *testing.T) {
	upstreamBody := []byte("upstream-only artifact")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/remote.txt" {
			http.Error(w, "nf", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(upstreamBody)
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-store", "public")
	e.createRawProxyRepo(t, adminToken, "raw-remote", srv.URL)
	e.createRawGroupRepo(t, adminToken, "raw-all", "raw-store", "raw-remote")

	// 放一件仅存在于 hosted 成员的制品。
	if rec := e.rawReq(http.MethodPut, "/repository/raw-store/local.txt", "Bearer "+adminToken, "text/plain", []byte("local hit")); rec.Code != http.StatusCreated {
		t.Fatalf("准备 hosted 制品失败：%d", rec.Code)
	}

	// group 命中 hosted 成员。
	rec := e.rawReq(http.MethodGet, "/repository/raw-all/local.txt", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "local hit" {
		t.Fatalf("group 读 hosted 成员状态码 = %d，内容 = %q", rec.Code, rec.Body.String())
	}

	// group 回退命中 proxy 成员（经回源）。
	rec = e.rawReq(http.MethodGet, "/repository/raw-all/remote.txt", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), upstreamBody) {
		t.Fatalf("group 读 proxy 成员状态码 = %d，内容 = %q", rec.Code, rec.Body.Bytes())
	}

	// 全成员皆无 → 404。
	if rec := e.rawReq(http.MethodGet, "/repository/raw-all/none.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("group 全未命中 GET 状态码 = %d，期望 404", rec.Code)
	}
}

func TestRawWriteRejectedOnProxyAndGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawProxyRepo(t, adminToken, "raw-proxy", srv.URL)
	e.createRawRepo(t, adminToken, "raw-store", "public")
	e.createRawGroupRepo(t, adminToken, "raw-group", "raw-store")

	// 写 proxy → 409。
	if rec := e.rawReq(http.MethodPut, "/repository/raw-proxy/w.txt", "Bearer "+adminToken, "text/plain", []byte("nope")); rec.Code != http.StatusConflict {
		t.Errorf("写 proxy 状态码 = %d，期望 409", rec.Code)
	}
	// 写 group → 409。
	if rec := e.rawReq(http.MethodPut, "/repository/raw-group/w.txt", "Bearer "+adminToken, "text/plain", []byte("nope")); rec.Code != http.StatusConflict {
		t.Errorf("写 group 状态码 = %d，期望 409", rec.Code)
	}
}
