package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
)

// stubStore 是 Store 的测试替身：按摘要匹配返回预置的 API Token 主体。
type stubStore struct {
	tokenDigest    string
	tokenPrincipal *Principal
	// 口令认证测试用
	passUser string
	passPass string
	passPrin *Principal
}

func (s *stubStore) IsTokenRevoked(string) (bool, error) { return false, nil }
func (s *stubStore) PrincipalByID(int64) (*Principal, error) {
	return nil, errors.New("未实现")
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
