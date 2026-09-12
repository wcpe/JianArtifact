package httpserver_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/protocol"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// protocolEnv 汇集含协议层路由的服务端句柄。
type protocolEnv struct {
	h          http.Handler
	db         *persistence.DB
	rawHandler *protocol.RawHandler
	repoRepo   *repository.RepoRepo
	assetRepo  *repository.AssetRepo
	formatMeta *repository.FormatMetadataRepo
	auditLogs  *repository.AuditLogRepo
	blobs      *blobstore.Store
	sourceNode string
}

// newProtocolEnv 装配完整服务端：契约路由 + Raw 协议路由（真实持久化 + blob 存储）。
func newProtocolEnv(t *testing.T) *protocolEnv {
	return newProtocolEnvOpts(t, "")
}

// newProtocolEnvWithPublicURL 构造带对外基础 URL（FR-87）的协议测试环境。
func newProtocolEnvWithPublicURL(t *testing.T, publicURL string) *protocolEnv {
	return newProtocolEnvOpts(t, publicURL)
}

func newProtocolEnvOpts(t *testing.T, publicURL string) *protocolEnv {
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
	formatMetadataRepo := repository.NewFormatMetadataRepo(db)
	publishPolicyRepo := repository.NewPublishPolicyRepo(db)
	settings := repository.NewSettingRepo(db)
	auditLogs := repository.NewAuditLogRepo(db)
	blobs := blobstore.NewStore(t.TempDir())

	jwtMgr := auth.NewJWTManager([]byte("integration-test-secret-key-32byte!!"))
	authStore := domain.NewAuthStore(userRepo, tokenRepo, revokedRepo)
	authenticator := auth.NewAuthenticator(jwtMgr, authStore)
	tokenSvc := domain.NewTokenService(tokenRepo, userRepo)

	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, domain.NewSettingService(settings), userRepo)
	repoSvc.SetEnabledFormats(formats.New(formats.Known...))
	repoSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, blobs, upstream.NewTestClient(5*time.Second))
	assetSvc.SetChangeRecorder(domain.NoopChangeRecorder{})
	nodeIdentity := domain.NewNodeIdentity(settings)
	assetSvc.SetNodeIdentity(nodeIdentity)
	publishPolicySvc := domain.NewPublishPolicyService(publishPolicyRepo, repoRepo, assetRepo)
	rawHandler := protocol.NewRawHandler(assetSvc, repoSvc)
	rawHandler.SetPublishPolicy(publishPolicySvc)
	mavenHandler := protocol.NewMavenHandler(rawHandler)
	dispatcher := protocol.NewDispatcher(repoSvc, rawHandler, mavenHandler)
	npmHandler := protocol.NewNpmHandler(rawHandler, authStore, tokenSvc, publicURL)
	dispatcher.SetNpm(npmHandler)
	formatMetadataSvc := domain.NewFormatMetadataService(assetSvc, repoRepo, formatMetadataRepo, upstream.NewTestClient(5*time.Second))
	cargoHandler := protocol.NewCargoHandler(rawHandler, domain.NewCargoService(assetSvc, repoSvc), publicURL)
	ociHandler := protocol.NewOCIHandler(rawHandler, domain.NewOCIService(assetSvc, repoSvc))
	pypiHandler := protocol.NewPypiHandler(rawHandler, formatMetadataSvc, publicURL)
	nugetHandler := protocol.NewNuGetHandler(rawHandler, formatMetadataSvc, publicURL)
	goProxyHandler := protocol.NewGoProxyHandler(rawHandler)

	handlers := api.NewHandlers(api.Deps{
		Version:         "test",
		Checks:          []func() error{db.Ping},
		Migration:       db.CurrentVersion,
		Auth:            domain.NewAuthService(userRepo, revokedRepo, jwtMgr),
		Users:           domain.NewUserService(userRepo),
		Tokens:          tokenSvc,
		Repos:           repoSvc,
		AuditLogs:       auditLogs,
		AuditSourceNode: nodeIdentity.NodeID(),
		PublicURL:       publicURL,
		PublishPolicies: publishPolicySvc,
	})
	rawHandler.SetAudit(handlers.AuditLog)
	rawHandler.SetOperationAudit(handlers.ProtocolAssetOperationAudit)

	srv := httpserver.New("test",
		httpserver.WithReadinessCheck(db.Ping),
		httpserver.WithHandlers(handlers),
		httpserver.WithMiddleware(api.MiddlewareFunc(authenticator.Optional())),
		httpserver.WithProtocolRoutes(func(r gin.IRouter) {
			protocolMW := authenticator.Protocol().Optional()
			cargoProtocolMW := authenticator.CargoProtocol().Optional()
			protocol.RegisterRoutes(r, dispatcher, protocolMW)
			protocol.RegisterNpmRoutes(r, npmHandler, protocolMW)
			protocol.RegisterOCIRoutes(r, ociHandler, protocolMW)
			protocol.RegisterCargoRoutes(r, cargoHandler, cargoProtocolMW)
			protocol.RegisterPypiRoutes(r, pypiHandler, protocolMW)
			protocol.RegisterGoProxyRoutes(r, goProxyHandler, protocolMW)
			protocol.RegisterNuGetRoutes(r, nugetHandler, protocolMW)
			// FR-73: Maven 网页上传（与 main.go 同款注册）
			r.POST("/api/v1/repositories/:name/maven-upload", authenticator.Optional(), mavenHandler.UploadForm)
			// 开源协议清单（admin 专属，与 main.go 同款注册）
			r.GET("/api/v1/licenses", authenticator.Optional(), handlers.GetLicenses)
		}),
	)
	return &protocolEnv{
		db: db,
		h:  srv.Handler(nil), rawHandler: rawHandler, repoRepo: repoRepo, assetRepo: assetRepo,
		formatMeta: formatMetadataRepo,
		auditLogs:  auditLogs, blobs: blobs, sourceNode: nodeIdentity.NodeID(),
	}
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
	req.Host = "127.0.0.1"
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
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
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

// externalProtocolReq 模拟经公网地址抵达协议路由的 HTTP 请求；用 URL scheme 区分
// TLS，避免 loopback 例外掩盖协议凭据传输校验。
func (e *protocolEnv) externalProtocolReq(method, target, authHeader, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.Host = "repo.example.test"
	req.RemoteAddr = "198.51.100.9:43210"
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

func basicUserPasswordHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func assertProtocolAudit(t *testing.T, e *protocolEnv, actor string, userID int64, action, result, detail string) {
	t.Helper()
	entries, err := e.auditLogs.List(repository.AuditFilter{Actor: actor, Limit: 50})
	if err != nil {
		t.Fatalf("查询协议审计：%v", err)
	}
	for _, entry := range entries {
		if entry.Action == action && entry.Result == result && entry.Detail == detail && entry.AuthSource == auth.AuthSourceBasic && entry.UserID != nil && *entry.UserID == userID && entry.SourceNode == e.sourceNode {
			return
		}
	}
	t.Fatalf("缺少协议审计 action=%q result=%q detail=%q，记录=%+v", action, result, detail, entries)
}

// TestProtocolCredentialsRequireTLSOutsideLoopback 确保携凭据的公网 HTTP
// 在协议 handler 前被拒绝，不产生资产、额度或成功审计；HTTPS 保持可发布。
func TestProtocolCredentialsRequireTLSOutsideLoopback(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-tls-policy", "private")

	const username = "tls-publisher"
	const password = "tls-publisher-password"
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: username, Password: password}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/raw-tls-policy/acl", adminToken,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予发布 ACL 状态码=%d", code)
	}
	policyPath := "/api/v1/users/" + strconv.FormatInt(user.Id, 10) + "/publish-policies/raw-tls-policy"
	if code := e.jsonReq(t, http.MethodPut, policyPath, adminToken, map[string]any{"maxAssetsHour": 1}, nil); code != http.StatusOK {
		t.Fatalf("保存发布策略状态码=%d", code)
	}

	basic := basicUserPasswordHeader(username, password)
	path := "/repository/raw-tls-policy/release/a.txt"
	if rec := e.externalProtocolReq(http.MethodPut, "http://repo.example.test"+path, basic, "text/plain", bytes.NewReader([]byte("blocked"))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("公网纯 HTTP 凭据发布状态码=%d，期望 401：%s", rec.Code, rec.Body.String())
	}
	repo, err := e.repoRepo.GetByName("raw-tls-policy")
	if err != nil {
		t.Fatalf("读取仓库：%v", err)
	}
	if _, err := e.assetRepo.GetByPath(repo.ID, "release/a.txt"); err == nil {
		t.Fatal("纯 HTTP 拒绝前不得写入制品")
	}
	entries, err := e.auditLogs.List(repository.AuditFilter{Action: "asset.put", Limit: 50})
	if err != nil {
		t.Fatalf("读取审计：%v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("纯 HTTP 拒绝必须在协议 handler 前发生，不能写协议审计：%+v", entries)
	}
	if rec := e.externalProtocolReq(http.MethodPut, "https://repo.example.test"+path, basic, "text/plain", bytes.NewReader([]byte("allowed"))); rec.Code != http.StatusCreated {
		t.Fatalf("HTTPS 凭据发布状态码=%d，期望 201：%s", rec.Code, rec.Body.String())
	}
	assertProtocolAudit(t, e, username, user.Id, "asset.put", "ok", "size=7")
}

// TestMavenPublishPolicyAndIdentityAudit 确保 Maven 协议复用发布路径限制，且发布
// 成功和拒绝均保留账号、认证来源和节点身份。
func TestMavenPublishPolicyAndIdentityAudit(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	visibility := api.CreateRepositoryRequestVisibilityPrivate
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, api.CreateRepositoryRequest{
		Name: "maven-policy-audit", Format: api.CreateRepositoryRequestFormatMaven, Type: api.CreateRepositoryRequestTypeHosted, Visibility: &visibility,
	}, nil); code != http.StatusCreated {
		t.Fatalf("创建 Maven 仓库状态码=%d", code)
	}
	const username = "maven-publisher"
	const password = "maven-publisher-password"
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: username, Password: password}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/maven-policy-audit/acl", adminToken,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予 Maven 发布 ACL 状态码=%d", code)
	}
	policyPath := "/api/v1/users/" + strconv.FormatInt(user.Id, 10) + "/publish-policies/maven-policy-audit"
	if code := e.jsonReq(t, http.MethodPut, policyPath, adminToken, map[string]any{"allowedPrefixes": []string{"com/example/release"}}, nil); code != http.StatusOK {
		t.Fatalf("保存 Maven 发布策略状态码=%d", code)
	}

	basic := basicUserPasswordHeader(username, password)
	if rec := e.externalProtocolReq(http.MethodPut, "https://repo.example.test/repository/maven-policy-audit/com/example/blocked/demo.jar", basic, "application/java-archive", bytes.NewReader([]byte("blocked"))); rec.Code != http.StatusForbidden {
		t.Fatalf("Maven 越前缀发布状态码=%d，期望 403：%s", rec.Code, rec.Body.String())
	}
	if rec := e.externalProtocolReq(http.MethodPut, "https://repo.example.test/repository/maven-policy-audit/com/example/release/demo.jar", basic, "application/java-archive", bytes.NewReader([]byte("release"))); rec.Code != http.StatusCreated {
		t.Fatalf("Maven 允许前缀发布状态码=%d，期望 201：%s", rec.Code, rec.Body.String())
	}
	assertProtocolAudit(t, e, username, user.Id, "asset.put", "rejected", "publish_path_denied")
	assertProtocolAudit(t, e, username, user.Id, "asset.put", "ok", "size=7")
}

type interruptedProtocolBody struct{ sent bool }

func (r *interruptedProtocolBody) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial"), nil
}

// TestProtocolDisconnectReleasesPublishReservation 确保上传正文断连后释放流式
// reservation，下一次发布可立即占用同一件数额度。
func TestProtocolDisconnectReleasesPublishReservation(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-disconnect-quota", "private")
	const username = "disconnect-publisher"
	const password = "disconnect-publisher-password"
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: username, Password: password}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/raw-disconnect-quota/acl", adminToken,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予发布 ACL 状态码=%d", code)
	}
	policyPath := "/api/v1/users/" + strconv.FormatInt(user.Id, 10) + "/publish-policies/raw-disconnect-quota"
	if code := e.jsonReq(t, http.MethodPut, policyPath, adminToken, map[string]any{"maxAssetsHour": 1}, nil); code != http.StatusOK {
		t.Fatalf("保存发布策略状态码=%d", code)
	}

	basic := basicUserPasswordHeader(username, password)
	broken := e.externalProtocolReq(http.MethodPut, "https://repo.example.test/repository/raw-disconnect-quota/release/broken.txt", basic, "text/plain", &interruptedProtocolBody{})
	if broken.Code < http.StatusInternalServerError {
		t.Fatalf("正文断连发布状态码=%d，期望 5xx：%s", broken.Code, broken.Body.String())
	}
	repo, err := e.repoRepo.GetByName("raw-disconnect-quota")
	if err != nil {
		t.Fatalf("读取仓库：%v", err)
	}
	if _, err := e.assetRepo.GetByPath(repo.ID, "release/broken.txt"); err == nil {
		t.Fatal("正文断连不得提交部分制品")
	}
	assertProtocolAudit(t, e, username, user.Id, "asset.put", "rejected", "write_failed")
	if rec := e.externalProtocolReq(http.MethodPut, "https://repo.example.test/repository/raw-disconnect-quota/release/retry.txt", basic, "text/plain", bytes.NewReader([]byte("retry"))); rec.Code != http.StatusCreated {
		t.Fatalf("断连后额度必须立即可重用，状态码=%d：%s", rec.Code, rec.Body.String())
	}
	assertProtocolAudit(t, e, username, user.Id, "asset.put", "ok", "size=5")
}

func TestRawPublishRejectionsRecordAuditableActor(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-policy-audit", "private")
	e.createRawRepo(t, adminToken, "raw-policy-other", "private")

	const username = "policy-audit-user"
	const password = "policy-audit-password"
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: username, Password: password}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码 = %d，期望 201", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/raw-policy-audit/acl", adminToken,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予 write ACL 状态码 = %d，期望 200", code)
	}
	policy := map[string]any{
		"allowedPrefixes": []string{"allowed"},
		"maxAssetsHour":   1,
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/users/"+strconv.FormatInt(user.Id, 10)+"/publish-policies/raw-policy-audit", adminToken, policy, nil); code != http.StatusOK {
		t.Fatalf("保存发布策略状态码 = %d，期望 200", code)
	}
	if code := e.jsonReq(t, http.MethodPatch, "/api/v1/repositories/raw-policy-audit", adminToken, map[string]any{"immutableRelease": true}, nil); code != http.StatusOK {
		t.Fatalf("设置仓库不可变 Release 状态码 = %d，期望 200", code)
	}

	basic := basicUserPasswordHeader(username, password)
	if rec := e.rawReq(http.MethodPut, "/repository/raw-policy-audit/allowed/a.txt", basic, "text/plain", []byte("ok")); rec.Code != http.StatusCreated {
		t.Fatalf("允许前缀首次发布状态码 = %d，期望 201：%s", rec.Code, rec.Body.String())
	}
	if rec := e.rawReq(http.MethodPut, "/repository/raw-policy-audit/blocked/a.txt", basic, "text/plain", []byte("blocked")); rec.Code != http.StatusForbidden {
		t.Fatalf("越前缀发布状态码 = %d，期望 403", rec.Code)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/raw-policy-audit/allowed/a.txt", basic, "text/plain", []byte("overwrite")); rec.Code != http.StatusConflict {
		t.Fatalf("不可变覆盖状态码 = %d，期望 409", rec.Code)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/raw-policy-audit/allowed/b.txt", basic, "text/plain", []byte("quota")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("额度超限状态码 = %d，期望 429", rec.Code)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/raw-policy-other/any.txt", basic, "text/plain", []byte("acl")); rec.Code != http.StatusForbidden {
		t.Fatalf("无 write ACL 发布状态码 = %d，期望 403", rec.Code)
	}
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-policy-audit/allowed/a.txt", basic, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("普通账号删除状态码 = %d，期望 403", rec.Code)
	}

	entries, err := e.auditLogs.List(repository.AuditFilter{Actor: username, Limit: 50})
	if err != nil {
		t.Fatalf("查询发布账号审计：%v", err)
	}
	wants := map[string]bool{
		"publish_path_denied":  false,
		"immutable_release":    false,
		"quota_exceeded":       false,
		"authorization_denied": false,
	}
	for _, entry := range entries {
		if entry.Result != "rejected" || entry.AuthSource != auth.AuthSourceBasic || entry.UserID == nil || *entry.UserID != user.Id || entry.SourceNode != e.sourceNode {
			continue
		}
		if _, ok := wants[entry.Detail]; ok {
			wants[entry.Detail] = true
		}
	}
	for detail, found := range wants {
		if !found {
			t.Errorf("缺少可按用户和认证方式识别的拒绝审计 detail=%q，记录=%+v", detail, entries)
		}
	}
}

func TestImmutableReleaseUsesRepositoryConfiguration(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-immutable-config", "private")

	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "immutable-config-user", Password: "immutable-config-password"}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码=%d，期望 201", code)
	}
	policyPath := "/api/v1/users/" + strconv.FormatInt(user.Id, 10) + "/publish-policies/raw-immutable-config"
	var repo map[string]any
	if code := e.jsonReq(t, http.MethodPatch, "/api/v1/repositories/raw-immutable-config", adminToken,
		map[string]any{"immutableRelease": true}, &repo); code != http.StatusOK {
		t.Fatalf("通过仓库配置启用不可变 Release 状态码=%d，期望 200", code)
	}
	if code := e.jsonReq(t, http.MethodPut, policyPath, adminToken,
		map[string]any{"immutableRelease": false}, nil); code != http.StatusBadRequest {
		t.Fatalf("旧发布策略写入 immutableRelease 状态码=%d，期望 400", code)
	}

	var policy api.PublishPolicyResponse
	if code := e.jsonReq(t, http.MethodGet, policyPath, adminToken, nil, &policy); code != http.StatusOK {
		t.Fatalf("读取兼容发布策略状态码=%d，期望 200", code)
	}
	if !policy.ImmutableRelease {
		t.Fatal("发布策略读取兼容字段必须反映仓库不可变 Release 配置")
	}
	if immutable, ok := repo["immutableRelease"].(bool); !ok || !immutable {
		t.Fatalf("仓库响应必须返回不可变 Release 配置，实际=%v", repo["immutableRelease"])
	}
}

func TestPublishPolicyPartialUpdateRetainsAllowedPrefixes(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-policy-partial", "private")

	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "partial-policy-user", Password: "partial-policy-password"}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码 = %d，期望 201", code)
	}
	policyPath := "/api/v1/users/" + strconv.FormatInt(user.Id, 10) + "/publish-policies/raw-policy-partial"
	if code := e.jsonReq(t, http.MethodPut, policyPath, adminToken,
		map[string]any{"allowedPrefixes": []string{"release", "snapshots"}}, nil); code != http.StatusOK {
		t.Fatalf("设置路径前缀状态码 = %d，期望 200", code)
	}
	if code := e.jsonReq(t, http.MethodPut, policyPath, adminToken,
		map[string]any{"maxAssetsHour": 3, "webLoginDisabled": true}, nil); code != http.StatusOK {
		t.Fatalf("部分更新策略状态码 = %d，期望 200", code)
	}
	var got api.PublishPolicyResponse
	if code := e.jsonReq(t, http.MethodGet, policyPath, adminToken, nil, &got); code != http.StatusOK {
		t.Fatalf("读取策略状态码 = %d，期望 200", code)
	}
	if len(got.AllowedPrefixes) != 2 || got.AllowedPrefixes[0] != "release" || got.AllowedPrefixes[1] != "snapshots" {
		t.Fatalf("部分更新不得丢失已有路径前缀，实际：%v", got.AllowedPrefixes)
	}
	if got.MaxAssetsHour != 3 || !got.WebLoginDisabled {
		t.Fatalf("部分更新字段未生效，实际：%+v", got)
	}
}

// bootstrapAdmin 自举首个管理员并签发协议测试用 API Token。
func (e *protocolEnv) bootstrapAdmin(t *testing.T) string {
	t.Helper()
	var boot api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", boot.Token,
		api.CreateTokenRequest{Name: "protocol-test"}, &created); code != http.StatusCreated {
		t.Fatalf("创建协议测试 Token 状态码 = %d，期望 201", code)
	}
	return created.Token
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

// TestRawBasicWriteCarriesRequestID 确保原生 Basic 发布也经全局请求标识中间件，
// 审计记录与响应头使用同一标识，便于按单次发布请求追踪。
func TestRawBasicWriteCarriesRequestID(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-request-id", "private")

	for _, tt := range []struct {
		name      string
		path      string
		requestID string
	}{
		{name: "未传入请求标识时生成", path: "generated.txt"},
		{name: "传入请求标识时保留", path: "provided.txt", requestID: "client-request-id-42"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/repository/raw-request-id/"+tt.path, bytes.NewReader([]byte(tt.name)))
			req.Host = "127.0.0.1"
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Authorization", basicUserPasswordHeader("admin", "admin-pass-123"))
			req.Header.Set("Content-Type", "text/plain")
			if tt.requestID != "" {
				req.Header.Set("X-Request-ID", tt.requestID)
			}
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("Raw Basic PUT 状态码 = %d，期望 201：%s", rec.Code, rec.Body.String())
			}

			responseID := rec.Header().Get("X-Request-ID")
			if responseID == "" {
				t.Fatal("响应缺少 X-Request-ID")
			}
			if tt.requestID != "" && responseID != tt.requestID {
				t.Fatalf("响应 X-Request-ID = %q，期望保留 %q", responseID, tt.requestID)
			}
			if tt.requestID == "" {
				if len(responseID) != 32 {
					t.Fatalf("生成的 X-Request-ID 长度 = %d，期望 32", len(responseID))
				}
				if _, err := hex.DecodeString(responseID); err != nil {
					t.Fatalf("生成的 X-Request-ID 不是十六进制随机标识 %q：%v", responseID, err)
				}
			}

			entries, err := e.auditLogs.List(repository.AuditFilter{Action: "asset.put", Repo: "raw-request-id", Limit: 10})
			if err != nil {
				t.Fatalf("查询 Raw 审计：%v", err)
			}
			for _, entry := range entries {
				if entry.EntityKey == "raw-request-id/"+tt.path && entry.Result == "ok" {
					if entry.RequestID != responseID {
						t.Fatalf("审计 requestId = %q，期望与响应 X-Request-ID 一致 %q", entry.RequestID, responseID)
					}
					return
				}
			}
			t.Fatalf("未找到 %s 的 Raw 发布审计：%+v", tt.path, entries)
		})
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

// TestRawDeleteRequiresGlobalAdmin 验证仓库 write ACL 不可替代制品删除的全局管理员权限。
func TestRawDeleteRequiresGlobalAdmin(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-delete", "private")
	if rec := e.rawReq(http.MethodPut, "/repository/raw-delete/f.txt", "Bearer "+adminToken, "text/plain", []byte("keep")); rec.Code != http.StatusCreated {
		t.Fatalf("准备制品失败：%d（体：%s）", rec.Code, rec.Body.String())
	}

	var writer api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "writer", Password: "writer-pass-123"}, &writer); code != http.StatusCreated {
		t.Fatalf("建 write 用户状态码 = %d，期望 201", code)
	}
	var writerLogin api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "writer", Password: "writer-pass-123"}, &writerLogin); code != http.StatusOK {
		t.Fatalf("write 用户登录状态码 = %d，期望 200", code)
	}
	var writerToken api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", writerLogin.Token,
		api.CreateTokenRequest{Name: "writer-protocol-test"}, &writerToken); code != http.StatusCreated {
		t.Fatalf("创建 write 用户协议 Token 状态码 = %d，期望 201", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/raw-delete/acl", adminToken,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: writer.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予 write ACL 状态码 = %d，期望 200", code)
	}

	if rec := e.rawReq(http.MethodDelete, "/repository/raw-delete/f.txt", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("未认证删除状态码 = %d，期望 401（体：%s）", rec.Code, rec.Body.String())
	} else {
		assertFailureOperationID(t, rec)
	}
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-delete/f.txt", "Bearer "+writerToken.Token, "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("拥有 write ACL 的非管理员删除状态码 = %d，期望 403（体：%s）", rec.Code, rec.Body.String())
	} else {
		assertFailureOperationID(t, rec)
	}
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-delete/missing.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("不存在制品删除状态码 = %d，期望 404（体：%s）", rec.Code, rec.Body.String())
	} else {
		assertFailureOperationID(t, rec)
	}
	if rec := e.rawReq(http.MethodGet, "/repository/raw-delete/f.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.String() != "keep" {
		t.Errorf("拒绝删除后制品应保留，状态码 = %d，内容 = %q", rec.Code, rec.Body.String())
	}

	repo, err := e.repoRepo.GetByName("raw-delete")
	if err != nil {
		t.Fatalf("读取仓库：%v", err)
	}
	asset, err := e.assetRepo.GetByPath(repo.ID, "f.txt")
	if err != nil {
		t.Fatalf("读取待删制品：%v", err)
	}
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-delete/f.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("管理员删除状态码 = %d，期望 204（体：%s）", rec.Code, rec.Body.String())
	}
	if _, err := e.assetRepo.GetByPath(repo.ID, "f.txt"); err != repository.ErrNotFound {
		t.Errorf("删除后制品元数据错误 = %v，期望 ErrNotFound", err)
	}
	if e.blobs.Exists(asset.BlobHash) {
		t.Errorf("删除最后引用后 blob 应立即离开活动存储，哈希 %s 仍存在", asset.BlobHash)
	}

	entries, err := e.auditLogs.List(repository.AuditFilter{Action: "asset.delete", Repo: "raw-delete"})
	if err != nil {
		t.Fatalf("读取删除审计：%v", err)
	}
	var success, anonymousRejected, writerRejected bool
	for _, entry := range entries {
		switch {
		case entry.EntityKey == "raw-delete/f.txt" && entry.Actor == "admin" && entry.UserID != nil && entry.AuthSource != "" && entry.Result == "ok":
			success = true
		case entry.EntityKey == "raw-delete/f.txt" && entry.Actor == "" && entry.Detail == "authorization_denied" && entry.Result == "rejected":
			anonymousRejected = true
		case entry.EntityKey == "raw-delete/f.txt" && entry.Actor == "writer" && entry.Detail == "authorization_denied" && entry.Result == "rejected":
			writerRejected = true
		}
	}
	if !success || !anonymousRejected || !writerRejected {
		t.Errorf("删除审计记录不正确：%+v", entries)
	}

	records, err := repository.NewReplicationOperationRepo(e.db).ListRecordsSince(0, 0)
	if err != nil {
		t.Fatalf("读取 v2 operation outbox：%v", err)
	}
	for _, record := range records {
		if record.Operation == nil || len(record.Operation.Items) != 1 {
			continue
		}
		item := record.Operation.Items[0]
		if item.Type == domain.EntityAsset && item.Key == domain.AssetKey("raw-delete", "f.txt") && item.Op == domain.OpDelete {
			if record.Operation.Actor.Username != "admin" || record.Operation.Actor.UserID == nil || record.Operation.Actor.AuthSource == "" {
				t.Errorf("Raw 删除 operation 必须携带主体快照：%+v", record.Operation)
			}
			return
		}
	}
	t.Errorf("删除应写入单条 v2 operation，实际记录：%+v", records)
}

// TestRawDeleteFailureResponsesIncludeOperationID 覆盖 Raw DELETE 进入领域后的冲突与内部失败响应。
func TestRawDeleteFailureResponsesIncludeOperationID(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstreamServer.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawProxyRepo(t, adminToken, "raw-delete-proxy", upstreamServer.URL)
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-delete-proxy/f.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("proxy 删除状态码 = %d，期望 409（体：%s）", rec.Code, rec.Body.String())
	} else {
		assertFailureOperationID(t, rec)
	}

	e.createRawRepo(t, adminToken, "raw-delete-failure", "private")
	if rec := e.rawReq(http.MethodPut, "/repository/raw-delete-failure/f.txt", "Bearer "+adminToken, "text/plain", []byte("keep")); rec.Code != http.StatusCreated {
		t.Fatalf("准备内部失败制品状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	repo, err := e.repoRepo.GetByName("raw-delete-failure")
	if err != nil {
		t.Fatalf("读取内部失败仓库：%v", err)
	}
	asset, err := e.assetRepo.GetByPath(repo.ID, "f.txt")
	if err != nil {
		t.Fatalf("读取内部失败制品：%v", err)
	}
	if err := e.blobs.Remove(asset.BlobHash); err != nil {
		t.Fatalf("模拟 blob 缺失：%v", err)
	}
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-delete-failure/f.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("blob 缺失删除状态码 = %d，期望 500（体：%s）", rec.Code, rec.Body.String())
	} else {
		assertFailureOperationID(t, rec)
	}
}

func TestRawDeleteAuditFailureLeavesAssetAndOutboxUntouched(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-audit-failure", "private")
	if rec := e.rawReq(http.MethodPut, "/repository/raw-audit-failure/keep.txt", "Bearer "+adminToken, "text/plain", []byte("keep")); rec.Code != http.StatusCreated {
		t.Fatalf("准备 Raw 制品状态码 = %d", rec.Code)
	}
	// 以审计失败前的水位为基线：FR-138 后 Put 自身也会写 v2 outbox，
	// 只验证失败操作不新增任何 outbox 记录（资产视图与 outbox 同事务回滚）。
	opRepo := repository.NewReplicationOperationRepo(e.db)
	base, err := opRepo.ListRecordsSince(0, 100)
	if err != nil {
		t.Fatalf("列审计失败前 operation outbox：%v", err)
	}
	var baseSeq int64
	if len(base) > 0 {
		baseSeq = base[len(base)-1].Seq
	}
	e.rawHandler.SetOperationAudit(func(_ *gin.Context, _, _ string) domain.AssetOperationAudit {
		return domain.AssetOperationAudit{Commit: func(string, []repository.AssetMutationItem) repository.MutationCompletionHook {
			return func(*sqlx.Tx) error { return errors.New("注入 Raw 审计失败") }
		}}
	})
	if rec := e.rawReq(http.MethodDelete, "/repository/raw-audit-failure/keep.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("审计失败删除状态码 = %d，期望 500", rec.Code)
	}
	if rec := e.rawReq(http.MethodGet, "/repository/raw-audit-failure/keep.txt", "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.String() != "keep" {
		t.Fatalf("审计失败不得删除 Raw 制品：状态=%d 内容=%q", rec.Code, rec.Body.String())
	}
	records, err := opRepo.ListRecordsSince(baseSeq, 10)
	if err != nil || len(records) != 0 {
		t.Fatalf("审计失败不得写 v2 outbox：records=%+v err=%v", records, err)
	}
}

// 协议端点 401 必须携带 WWW-Authenticate: Basic 质询头：Maven/Gradle 等客户端
// 默认非抢占式认证，收不到 Basic 质询就不会带凭据重试，私有仓库将无法拉取。
// API 端点（/api/*）不得携带该头，否则浏览器会弹出原生 Basic 登录框。
func TestProtocolUnauthorizedChallengesBasic(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")

	// 私有仓匿名读 → 401 + Basic 质询。
	rec := e.rawReq(http.MethodGet, "/repository/raw-hosted/f.txt", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("私有仓匿名读状态码 = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="JianArtifact"` {
		t.Errorf("协议端点 401 WWW-Authenticate = %q，期望 Basic realm=\"JianArtifact\"", got)
	}

	// 私有仓匿名写 → 401 + Basic 质询（mvn deploy 同样依赖质询）。
	rec = e.rawReq(http.MethodPut, "/repository/raw-hosted/g.txt", "", "text/plain", []byte("x"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("私有仓匿名写状态码 = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="JianArtifact"` {
		t.Errorf("匿名写 401 WWW-Authenticate = %q，期望 Basic 质询", got)
	}

	// 对照：API 端点 401 不得携带质询头（避免浏览器原生弹框）。
	rec = e.rawReq(http.MethodGet, "/api/v1/users", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API 匿名读状态码 = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("API 端点 401 不应携带 WWW-Authenticate，实得 %q", got)
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

// TestMavenHostedSnapshotLiteralFallback 验证 SNAPSHOT 字面文件在无 maven-metadata.xml
// （无顶层 <snapshot> timestamp/buildNumber）时仍可被 GET：时间戳解析失败后回退字面路径。
func TestMavenHostedSnapshotLiteralFallback(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)

	vis := api.CreateRepositoryRequestVisibility("private")
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, api.CreateRepositoryRequest{
		Name:       "maven-snapshots",
		Format:     api.CreateRepositoryRequestFormat("maven"),
		Type:       api.CreateRepositoryRequestType("hosted"),
		Visibility: &vis,
	}, nil); code != http.StatusCreated {
		t.Fatalf("建 maven 仓库状态码 = %d，期望 201", code)
	}

	// 仅 PUT 字面 -SNAPSHOT.pom，不 PUT maven-metadata.xml。
	path := "/repository/maven-snapshots/com/example/demo/1.0.0-SNAPSHOT/demo-1.0.0-SNAPSHOT.pom"
	if rec := e.rawReq(http.MethodPut, path, "Bearer "+adminToken, "application/xml", []byte("<project/>")); rec.Code != http.StatusCreated {
		t.Fatalf("PUT 状态码 = %d，期望 201", rec.Code)
	}

	// 无 metadata 时 GET -SNAPSHOT 应回退字面路径返回 200。
	if rec := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d，期望 200（SNAPSHOT 回退字面路径）", rec.Code)
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

// TestDoubleSlashPathCompat 客户端拼接 baseUrl + "/" + path 产生双斜杠（如 /repository/raw//dir/a.txt）时应正常解析。
func TestDoubleSlashPathCompat(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")

	payload := []byte("double slash compat")
	path := "/repository/raw-hosted/dir/a.txt"
	if rec := e.rawReq(http.MethodPut, path, "Bearer "+adminToken, "text/plain", payload); rec.Code != http.StatusCreated {
		t.Fatalf("PUT 状态码 = %d，期望 201", rec.Code)
	}

	// 双斜杠（baseUrl 尾 / + 路径首 /）应返回 200 而非 404。
	doublePath := "/repository/raw-hosted//dir/a.txt"
	if rec := e.rawReq(http.MethodGet, doublePath, "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("双斜杠路径 GET 状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	} else if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("双斜杠路径 GET 内容与写入不一致：%q", rec.Body.Bytes())
	}
}
