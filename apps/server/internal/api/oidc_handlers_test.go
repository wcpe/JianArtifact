package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// oidcTestSecret 是端点测试的会话/流程签名密钥（与生产无关，仅测试内使用）。
const oidcTestSecret = "api-oidc-test-secret-32-bytes!!"

// fakeOIDC 实现 OIDCLogin，在不接触真实 IdP 的前提下驱动端点分支。
type fakeOIDC struct {
	claims      auth.OIDCClaims
	exchangeErr error
	authURL     string
	lastCode    string
}

func (f *fakeOIDC) NewFlow() auth.OIDCFlow {
	return auth.OIDCFlow{State: "state-1", Nonce: "nonce-1", CodeVerifier: "verifier-1", ExpiresAt: time.Now().Add(time.Minute).Unix()}
}

func (f *fakeOIDC) AuthCodeURL(context.Context, auth.OIDCFlow) (string, error) {
	return f.authURL, nil
}

func (f *fakeOIDC) Exchange(_ context.Context, code string, _ auth.OIDCFlow) (auth.OIDCClaims, error) {
	f.lastCode = code
	if f.exchangeErr != nil {
		return auth.OIDCClaims{}, f.exchangeErr
	}
	return f.claims, nil
}

// newOIDCHandlers 装配带真实 AuthService（临时库）与假协议侧的 Handlers。
func newOIDCHandlers(t *testing.T, deps *OIDCDeps) *Handlers {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "api-oidc.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	svc := domain.NewAuthService(
		repository.NewUserRepo(db),
		repository.NewRevokedRepo(db),
		auth.NewJWTManager([]byte(oidcTestSecret)),
	)
	return NewHandlers(Deps{Auth: svc, Users: domain.NewUserService(repository.NewUserRepo(db)), OIDC: deps})
}

func oidcDepsForTest(fake *fakeOIDC, domains []string) *OIDCDeps {
	return &OIDCDeps{
		Verifier:       fake,
		FlowSigner:     auth.NewOIDCFlowSigner([]byte(oidcTestSecret)),
		AllowedDomains: domains,
		RedirectAfter:  "/login",
	}
}

// startFlowViaEndpoint 走一次起跳并取出流程 Cookie。
func startFlowViaEndpoint(t *testing.T, h *Handlers) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/start", nil)
	h.StartOidcLogin(c)
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.OIDCFlowCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("起跳响应未写入流程 Cookie（code=%d）", rec.Code)
	return nil
}

// callbackViaEndpoint 走一次回调并返回重定向地址。
func callbackViaEndpoint(t *testing.T, h *Handlers, cookie *http.Cookie, code, state string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback", nil)
	if cookie != nil {
		c.Request.AddCookie(cookie)
	}
	h.CompleteOidcLogin(c, CompleteOidcLoginParams{Code: &code, State: &state})
	return rec.Header().Get("Location")
}

// TestOIDCEndpointsDisabledReturn404 未启用 OIDC 时两个端点都必须 404，而不是暴露半成品。
func TestOIDCEndpointsDisabledReturn404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlers(Deps{})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/start", nil)
	h.StartOidcLogin(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未启用时起跳应 404，实际 %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback", nil)
	h.CompleteOidcLogin(c2, CompleteOidcLoginParams{})
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("未启用时回调应 404，实际 %d", rec2.Code)
	}
}

// TestOIDCLoginFlowIssuesSessionThroughFragment 起跳写 Cookie 并跳 IdP；回调成功时
// 以 URL 片段携令牌回前端（片段不进服务端访问日志）。
func TestOIDCLoginFlowIssuesSessionThroughFragment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeOIDC{
		authURL: "https://idp.example.com/authorize?client_id=x",
		claims:  auth.OIDCClaims{Subject: "sub-alice", Username: "alice", Email: "alice@example.com"},
	}
	h := newOIDCHandlers(t, oidcDepsForTest(fake, nil))

	cookie := startFlowViaEndpoint(t, h)
	location := callbackViaEndpoint(t, h, cookie, "auth-code-1", "state-1")
	if !strings.HasPrefix(location, "/login#token=") {
		t.Fatalf("成功回调应以片段携令牌：%q", location)
	}
	if fake.lastCode != "auth-code-1" {
		t.Fatalf("应把授权码交给协议侧：%q", fake.lastCode)
	}
}

// TestOIDCCallbackRejectsStateMismatchAndTamperedFlow state 不符与流程 Cookie 被篡改都必须拒绝。
func TestOIDCCallbackRejectsStateMismatchAndTamperedFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeOIDC{claims: auth.OIDCClaims{Subject: "s", Username: "u", Email: "u@example.com"}}
	h := newOIDCHandlers(t, oidcDepsForTest(fake, nil))
	cookie := startFlowViaEndpoint(t, h)

	if location := callbackViaEndpoint(t, h, cookie, "c", "other-state"); !strings.Contains(location, "#error=state_mismatch") {
		t.Fatalf("state 不符应回错误片段：%q", location)
	}

	tampered := *cookie
	tampered.Value = "x" + cookie.Value
	if location := callbackViaEndpoint(t, h, &tampered, "c", "state-1"); !strings.Contains(location, "#error=flow_expired") {
		t.Fatalf("篡改的流程状态应回 flow_expired：%q", location)
	}
}

// TestOIDCCallbackRejectsDomainOutsideAllowlist 配置白名单时，名单外邮箱不得建号或登录。
func TestOIDCCallbackRejectsDomainOutsideAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fake := &fakeOIDC{claims: auth.OIDCClaims{Subject: "s", Username: "bob", Email: "bob@other.example"}}
	h := newOIDCHandlers(t, oidcDepsForTest(fake, []string{"example.com"}))
	cookie := startFlowViaEndpoint(t, h)

	if location := callbackViaEndpoint(t, h, cookie, "c", "state-1"); !strings.Contains(location, "#error=not_allowed") {
		t.Fatalf("白名单外域名应回 not_allowed：%q", location)
	}
}

// TestGetStatusReportsOIDCEnabled 状态端点如实报告 OIDC 是否启用（前端据此决定是否展示入口）。
func TestGetStatusReportsOIDCEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	disabled := NewHandlers(Deps{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	disabled.GetStatus(c)
	if !strings.Contains(rec.Body.String(), `"oidcEnabled":false`) {
		t.Fatalf("未启用时应报告 false：%s", rec.Body.String())
	}

	enabled := newOIDCHandlers(t, oidcDepsForTest(&fakeOIDC{}, nil))
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	enabled.GetStatus(c2)
	if !strings.Contains(rec2.Body.String(), `"oidcEnabled":true`) {
		t.Fatalf("启用时应报告 true：%s", rec2.Body.String())
	}
}

// TestGetCurrentUserReturnsSessionOwner 会话可经 /auth/me 取回身份快照（OIDC 回调链路依赖它）。
func TestGetCurrentUserReturnsSessionOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newOIDCHandlers(t, nil)

	u, err := h.users.Create("alice", "secret123", "user")
	if err != nil {
		t.Fatalf("预建用户：%v", err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	c.Set("auth.principal", &auth.Principal{UserID: u.ID, Username: "alice", Role: "user"})
	h.GetCurrentUser(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("已认证应返回 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Fatalf("应返回会话归属用户：%s", rec.Body.String())
	}
}
