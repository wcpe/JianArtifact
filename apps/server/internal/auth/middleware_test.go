package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// stubStore 是 Store 的测试替身：按摘要匹配返回预置的 API Token 主体。
type stubStore struct {
	tokenDigest    string
	tokenPrincipal *Principal
	idPrincipal    *Principal
	// 口令认证测试用
	passUser string
	passPass string
	passPrin *Principal
}

func (s *stubStore) IsTokenRevoked(string) (bool, error) { return false, nil }
func (s *stubStore) PrincipalByID(id int64) (*Principal, error) {
	if s.idPrincipal != nil && s.idPrincipal.UserID == id {
		return s.idPrincipal, nil
	}
	return nil, errors.New("用户不存在")
}
func (s *stubStore) PrincipalByTokenDigest(digest string) (*Principal, error) {
	if s.tokenPrincipal != nil && digest == s.tokenDigest {
		return s.tokenPrincipal, nil
	}
	return nil, errors.New("无匹配 token")
}
func (s *stubStore) PrincipalByPassword(username, password string) (*Principal, error) {
	if s.passPrin != nil && username == s.passUser && password == s.passPass {
		return s.passPrin, nil
	}
	return nil, ErrUnauthenticated
}

// newTestAuthenticator 构造一个仅支持指定 API Token 的 Authenticator。
func newTestAuthenticator(plaintext string, principal *Principal) *Authenticator {
	store := &stubStore{tokenDigest: DigestToken(plaintext), tokenPrincipal: principal}
	return NewAuthenticator(NewJWTManager([]byte("test-secret")), store)
}

// basicHeader 构造 Authorization: Basic 头值。
func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestResolveBasicTokenAsPassword(t *testing.T) {
	token := "jat_basicpass"
	want := &Principal{UserID: 7, Username: "svc", Role: "user"}
	a := newTestAuthenticator(token, want)

	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", basicHeader("anyuser", token))

	got, err := a.resolve(r)
	if err != nil {
		t.Fatalf("Basic（token 作密码）应通过：%v", err)
	}
	if got.UserID != 7 || got.Kind != KindToken {
		t.Fatalf("主体不符：%+v", got)
	}
}

func TestResolveBasicTokenAsUsername(t *testing.T) {
	token := "jat_basicuser"
	want := &Principal{UserID: 8, Username: "svc", Role: "user"}
	a := newTestAuthenticator(token, want)

	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	// password 为空 → 回落取 username。
	r.Header.Set("Authorization", basicHeader(token, ""))

	got, err := a.resolve(r)
	if err != nil {
		t.Fatalf("Basic（token 作用户名）应通过：%v", err)
	}
	if got.UserID != 8 || got.Kind != KindToken {
		t.Fatalf("主体不符：%+v", got)
	}
}

func TestResolveBearerToken(t *testing.T) {
	token := "jat_bearer"
	want := &Principal{UserID: 9, Username: "svc", Role: "admin"}
	a := newTestAuthenticator(token, want)

	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	got, err := a.resolve(r)
	if err != nil {
		t.Fatalf("Bearer（token）应通过：%v", err)
	}
	if got.UserID != 9 || got.Kind != KindToken {
		t.Fatalf("主体不符：%+v", got)
	}
}

func TestResolveNuGetAPIKeyInProtocolMode(t *testing.T) {
	token := "jat_nuget"
	want := &Principal{UserID: 10, Username: "svc", Role: "user"}
	a := newTestAuthenticator(token, want).Protocol()
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-NuGet-ApiKey", token)

	got, err := a.resolveFor(r, true)
	if err != nil {
		t.Fatalf("NuGet API Key 应通过协议认证：%v", err)
	}
	if got.UserID != want.UserID || got.AuthSource != AuthSourceBasicToken {
		t.Fatalf("主体不符：%+v", got)
	}
}

func TestProtocolNuGetAPIKeyRejectsSpoofedLoopbackHostFromExternalPeer(t *testing.T) {
	token := "jat_nuget_external_plaintext"
	a := newTestAuthenticator(token, &Principal{UserID: 15, Username: "nuget-publisher", Role: "user"}).Protocol()
	r, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1/nuget/hosted/api/v2/package", nil)
	r.Host = "127.0.0.1"
	r.RemoteAddr = "198.51.100.9:43210"
	r.Header.Set("X-NuGet-ApiKey", token)

	if _, err := a.resolveFor(r, true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("外部来源伪造回环 Host 必须拒绝明文 NuGet API Key，实际：%v", err)
	}
}

func TestResolveNoCredentials(t *testing.T) {
	a := newTestAuthenticator("jat_x", &Principal{UserID: 1})
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	if _, err := a.resolve(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("无凭据应返回 ErrUnauthenticated，实际：%v", err)
	}
}

func TestResolveBasicWrongToken(t *testing.T) {
	a := newTestAuthenticator("jat_right", &Principal{UserID: 1})
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", basicHeader("", "jat_wrong"))
	if _, err := a.resolve(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("错误 token 应返回 ErrUnauthenticated，实际：%v", err)
	}
}

func TestResolveBasicNonTokenRejected(t *testing.T) {
	// Basic 凭据不以 jat_ 开头且口令验证失败应被拒绝。
	a := newTestAuthenticator("jat_x", &Principal{UserID: 1})
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", basicHeader("alice", "plaintext-password"))
	if _, err := a.resolve(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("口令凭据应被拒绝，实际：%v", err)
	}
}

func TestResolveBasicPasswordAuth(t *testing.T) {
	// 用户名+口令认证成功场景。
	store := &stubStore{
		tokenDigest:    DigestToken("jat_x"),
		tokenPrincipal: &Principal{UserID: 1},
		passUser:       "release",
		passPass:       "releasereleaserelease",
		passPrin:       &Principal{UserID: 2, Username: "release", Role: "user"},
	}
	a := NewAuthenticator(NewJWTManager([]byte("test-secret")), store)
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", basicHeader("release", "releasereleaserelease"))
	p, err := a.resolve(r)
	if err != nil {
		t.Fatalf("口令认证应成功：%v", err)
	}
	if p.Username != "release" || p.UserID != 2 {
		t.Fatalf("主体不匹配，实际：%+v", p)
	}
}

func TestOptionalRejectsWebLoginDisabledManagementCredentials(t *testing.T) {
	jwtManager := NewJWTManager([]byte("test-secret"))
	disabled := &Principal{UserID: 42, Username: "release", Role: "user", WebLoginDisabled: true}
	apiToken := "jat_disabled"
	a := NewAuthenticator(jwtManager, &stubStore{
		tokenDigest:    DigestToken(apiToken),
		tokenPrincipal: disabled,
		idPrincipal:    disabled,
	})
	session, _, _, err := jwtManager.Issue(disabled.UserID, disabled.Role)
	if err != nil {
		t.Fatalf("签发会话：%v", err)
	}

	router := gin.New()
	router.Use(a.Optional())
	for _, path := range []string{
		"/api/v1/repositories",
		"/api/v1/repositories/raw/assets",
		"/api/v1/repositories/raw",
	} {
		router.GET(path, func(c *gin.Context) { c.Status(http.StatusOK) })
	}
	for name, credential := range map[string]string{
		"既有 JWT":       "Bearer " + session,
		"既有 API Token": "Bearer " + apiToken,
	} {
		for _, path := range []string{
			"/api/v1/repositories",
			"/api/v1/repositories/raw/assets",
			"/api/v1/repositories/raw",
		} {
			t.Run(name+path, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Authorization", credential)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("状态码 = %d，期望 403，响应 = %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestProtocolOptionalAllowsWebLoginDisabledBasicAccount(t *testing.T) {
	principal := &Principal{UserID: 7, Username: "release", Role: "user", WebLoginDisabled: true}
	a := NewAuthenticator(NewJWTManager([]byte("test-secret")), &stubStore{
		passUser: "release",
		passPass: "release-password",
		passPrin: principal,
	}).Protocol()
	router := gin.New()
	router.Use(a.Optional())
	router.GET("/repository/raw/file", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/repository/raw/file", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", basicHeader("release", "release-password"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("协议 Basic 认证状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
}

func TestProtocolCredentialsRejectUntrustedForwardedHTTPS(t *testing.T) {
	token := "jat_forwarded_proto"
	a := newTestAuthenticator(token, &Principal{UserID: 11, Username: "publisher", Role: "user"}).Protocol()
	r, _ := http.NewRequest(http.MethodPut, "http://repo.example.test/repository/raw/a.jar", nil)
	r.Host = "repo.example.test"
	r.RemoteAddr = "198.51.100.9:43210"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Forwarded-Proto", "https")

	if _, err := a.resolveFor(r, true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("公网直连伪造 X-Forwarded-Proto 必须拒绝协议凭据，实际：%v", err)
	}
}

func TestProtocolCredentialsAllowedByHostAllowlist(t *testing.T) {
	token := "jat_host_allowlist"
	store := &stubStore{tokenDigest: DigestToken(token), tokenPrincipal: &Principal{UserID: 13, Username: "publisher", Role: "user"}}
	newReq := func(host string) *http.Request {
		r, _ := http.NewRequest(http.MethodPut, "http://repo.example.test/repository/raw/a.jar", nil)
		r.Host = host
		r.RemoteAddr = "198.51.100.9:43210"
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}

	// 未配置白名单：维持默认安全行为，非 TLS 非回环拒绝。
	noLimit := NewAuthenticator(NewJWTManager([]byte("test-secret")), store)
	if _, err := noLimit.Protocol().resolveFor(newReq("repo.example.test"), true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("未配置白名单时非 TLS 非回环应拒绝，实际：%v", err)
	}

	// 配置白名单：命中放行，未命中拒绝。
	allowed := []string{"maven.wcpe.top", "repo.wcpe.top"}
	withLimit := NewAuthenticator(NewJWTManager([]byte("test-secret")), store,
		WithAllowedHosts(func() []string { return allowed }))
	if _, err := withLimit.Protocol().resolveFor(newReq("maven.wcpe.top"), true); err != nil {
		t.Fatalf("白名单命中应放行：%v", err)
	}
	if _, err := withLimit.Protocol().resolveFor(newReq("maven.wcpe.top:443"), true); err != nil {
		t.Fatalf("白名单命中（带端口）应放行：%v", err)
	}
	if _, err := withLimit.Protocol().resolveFor(newReq("evil.example.com"), true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("白名单未命中应拒绝，实际：%v", err)
	}
	if _, err := withLimit.Protocol().resolveFor(newReq("198.51.100.9:50020"), true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("IP 直连 Host 应拒绝，实际：%v", err)
	}

	// 白名单返回空列表：同样维持默认拒绝。
	emptyList := NewAuthenticator(NewJWTManager([]byte("test-secret")), store,
		WithAllowedHosts(func() []string { return nil }))
	if _, err := emptyList.Protocol().resolveFor(newReq("repo.example.test"), true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("空白名单时非 TLS 非回环应拒绝，实际：%v", err)
	}
}

func TestProtocolCredentialsLoopbackAllowedEvenWhenHostBlocked(t *testing.T) {
	a := NewAuthenticator(NewJWTManager([]byte("test-secret")), &stubStore{},
		WithAllowedHosts(func() []string { return []string{"maven.wcpe.top"} }))
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Host = "127.0.0.1:50020"
	r.RemoteAddr = "127.0.0.1:1234"
	if !a.protocolCredentialsAllowed(r) {
		t.Fatal("回环来源应放行，不受白名单限制")
	}
}

func TestProtocolCredentialsRejectSpoofedLoopbackHostFromExternalPeer(t *testing.T) {
	token := "jat_spoofed_loopback_host"
	a := newTestAuthenticator(token, &Principal{UserID: 14, Username: "publisher", Role: "user"}).Protocol()
	r, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1/repository/raw/a.jar", nil)
	r.Host = "127.0.0.1"
	r.RemoteAddr = "198.51.100.9:43210"
	r.Header.Set("Authorization", "Bearer "+token)

	if _, err := a.resolveFor(r, true); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("外部来源伪造回环 Host 必须拒绝明文协议凭据，实际：%v", err)
	}
}

func TestIsLoopbackRequestUsesRemoteAddress(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:1234":              true,
		"[::1]:1234":                  true,
		"[::ffff:127.0.0.1]:1234":     true,
		"127.0.0.1":                   true,
		"::1":                         true,
		"198.51.100.9:43210":          false,
		"[::ffff:198.51.100.9]:43210": false,
		"localhost:1234":              false,
		"":                            false,
	}
	for remoteAddr, want := range tests {
		t.Run(remoteAddr, func(t *testing.T) {
			r := &http.Request{Host: "127.0.0.1", RemoteAddr: remoteAddr}
			if got := isLoopbackRequest(r); got != want {
				t.Fatalf("isLoopbackRequest(%q) = %v，期望 %v", remoteAddr, got, want)
			}
		})
	}
}
