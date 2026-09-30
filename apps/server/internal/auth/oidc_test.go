package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// fakeIdP 是最小 OIDC 提供方：discovery、JWKS 与令牌端点全部本地，
// 用于真实走完「PKCE → 授权码兑换 → ID Token 验签 → 声明提取」的协议路径。
type fakeIdP struct {
	t        *testing.T
	server   *httptest.Server
	signKey  *rsa.PrivateKey // 实际签 ID Token 的密钥
	pubKey   *rsa.PublicKey  // JWKS 中公布的公钥（默认同 signKey；可设为另一把以制造验签失败）
	kid      string
	clientID string
	// nonce 由 /token 写回 ID Token，供用例制造 nonce 不匹配的负例。
	nonce string
	// extra 覆盖/补充 ID Token 声明（如 email_verified=false）。
	extra map[string]any
	// codeChallenge 记录 /authorize 里出现的 PKCE challenge，供 /token 真校验 verifier。
	codeChallenge string
}

func newFakeIdP(t *testing.T, clientID string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试密钥：%v", err)
	}
	idp := &fakeIdP{t: t, signKey: key, pubKey: &key.PublicKey, kid: "test-kid-1", clientID: clientID}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/authorize",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": idp.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(idp.pubKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(idp.pubKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		idp.codeChallenge = r.URL.Query().Get("code_challenge")
		if got := r.URL.Query().Get("code_challenge_method"); got != "S256" {
			t.Errorf("授权请求必须使用 PKCE S256，实际 %q", got)
		}
		if r.URL.Query().Get("nonce") == "" {
			t.Error("授权请求必须携带 nonce")
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// 真实校验 PKCE：收到的 verifier 必须与授权请求里的 challenge 匹配。
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != idp.codeChallenge {
			http.Error(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		now := time.Now()
		claims := map[string]any{
			"iss": idp.server.URL, "aud": idp.clientID, "sub": "subject-1",
			"preferred_username": "alice", "email": "alice@example.com",
			"email_verified": true, "nonce": idp.nonce,
			"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		}
		for k, v := range idp.extra {
			claims[k] = v
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
		token.Header["kid"] = idp.kid
		signed, err := token.SignedString(idp.signKey)
		if err != nil {
			t.Errorf("签名 ID Token：%v", err)
			http.Error(w, "sign failed", http.StatusInternalServerError)
			return
		}
		writeJSON(t, w, map[string]any{
			"access_token": "test-access-token", "token_type": "Bearer",
			"expires_in": 300, "id_token": signed,
		})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("写响应：%v", err)
	}
}

// newVerifier 构造指向假 IdP 的校验器。
func newVerifier(idp *fakeIdP, mutate func(*config.OIDCConfig)) *OIDCVerifier {
	cfg := config.OIDCConfig{
		Issuer:        idp.server.URL,
		ClientID:      idp.clientID,
		ClientSecret:  "test-secret",
		RedirectURL:   "http://localhost/api/v1/auth/oidc/callback",
		UsernameClaim: "preferred_username",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewOIDCVerifier(cfg)
}

// startFlow 生成流程状态并把 challenge / nonce 交给假 IdP，返回流程与授权地址。
func startFlow(t *testing.T, v *OIDCVerifier, idp *fakeIdP) (OIDCFlow, *url.URL) {
	t.Helper()
	flow := v.NewFlow()
	raw, err := v.AuthCodeURL(context.Background(), flow)
	if err != nil {
		t.Fatalf("构造授权地址：%v", err)
	}
	authURL, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析授权地址：%v", err)
	}
	if got := authURL.Query().Get("state"); got != flow.State {
		t.Fatalf("授权地址应带 state：%q", got)
	}
	// 假 IdP 的 /authorize 记录了 challenge；这里直接复刻其口径。
	sum := sha256.Sum256([]byte(flow.CodeVerifier))
	idp.codeChallenge = base64.RawURLEncoding.EncodeToString(sum[:])
	idp.nonce = flow.Nonce
	return flow, authURL
}

func TestOIDCFlowSignerRejectsTamperAndExpiry(t *testing.T) {
	signer := NewOIDCFlowSigner([]byte("test-secret"))
	flow := OIDCFlow{State: "s", Nonce: "n", CodeVerifier: "v", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	encoded, err := signer.Encode(flow)
	if err != nil {
		t.Fatalf("编码流程状态：%v", err)
	}
	got, err := signer.Decode(encoded, time.Now())
	if err != nil || got.State != "s" || got.Nonce != "n" || got.CodeVerifier != "v" {
		t.Fatalf("往返解码不符：%+v err=%v", got, err)
	}

	// 篡改载荷：签名校验必须失败。
	if _, err := signer.Decode("x"+encoded, time.Now()); err == nil {
		t.Fatal("篡改后的流程状态不应通过校验")
	}
	// 他人密钥：不得通过校验。
	other := NewOIDCFlowSigner([]byte("another-secret"))
	if _, err := other.Decode(encoded, time.Now()); err == nil {
		t.Fatal("异密钥签名不应通过校验")
	}
	// 过期：即使签名正确也必须拒绝。
	expired := OIDCFlow{State: "s", ExpiresAt: time.Now().Add(-time.Second).Unix()}
	encodedExpired, err := signer.Encode(expired)
	if err != nil {
		t.Fatalf("编码过期流程：%v", err)
	}
	if _, err := signer.Decode(encodedExpired, time.Now()); err == nil {
		t.Fatal("过期流程状态不应通过校验")
	}
}

func TestOIDCVerifierExchangesCodeAndVerifiesClaims(t *testing.T) {
	idp := newFakeIdP(t, "jianartifact")
	v := newVerifier(idp, nil)
	flow, authURL := startFlow(t, v, idp)
	if got := authURL.Query().Get("code_challenge_method"); got != "S256" {
		t.Fatalf("授权地址应使用 PKCE S256：%q", got)
	}

	claims, err := v.Exchange(context.Background(), "test-code", flow)
	if err != nil {
		t.Fatalf("兑换并校验：%v", err)
	}
	if claims.Subject != "subject-1" || claims.Username != "alice" || claims.Email != "alice@example.com" {
		t.Fatalf("声明提取不符：%+v", claims)
	}
}

func TestOIDCVerifierRejectsNonceMismatch(t *testing.T) {
	idp := newFakeIdP(t, "jianartifact")
	v := newVerifier(idp, nil)
	flow, _ := startFlow(t, v, idp)
	idp.nonce = "other-nonce" // IdP 返回的 ID Token 携带了不匹配的 nonce

	if _, err := v.Exchange(context.Background(), "test-code", flow); err == nil {
		t.Fatal("nonce 不匹配必须拒绝")
	}
}

func TestOIDCVerifierRejectsUnknownSigningKey(t *testing.T) {
	idp := newFakeIdP(t, "jianartifact")
	// JWKS 公布一把与签名密钥不同的公钥：验签必须失败。
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成另一把密钥：%v", err)
	}
	idp.pubKey = &other.PublicKey
	v := newVerifier(idp, nil)
	flow, _ := startFlow(t, v, idp)

	if _, err := v.Exchange(context.Background(), "test-code", flow); err == nil {
		t.Fatal("签名密钥与 JWKS 不符必须拒绝")
	}
}

func TestOIDCVerifierRejectsUnverifiedEmail(t *testing.T) {
	idp := newFakeIdP(t, "jianartifact")
	idp.extra = map[string]any{"email_verified": false}
	v := newVerifier(idp, nil)
	flow, _ := startFlow(t, v, idp)

	if _, err := v.Exchange(context.Background(), "test-code", flow); err == nil {
		t.Fatal("邮箱未验证时必须拒绝")
	}
}

func TestOIDCVerifierRejectsMissingUsernameClaim(t *testing.T) {
	idp := newFakeIdP(t, "jianartifact")
	v := newVerifier(idp, func(c *config.OIDCConfig) { c.UsernameClaim = "employee_id" })
	flow, _ := startFlow(t, v, idp)

	_, err := v.Exchange(context.Background(), "test-code", flow)
	if err == nil || !strings.Contains(err.Error(), "employee_id") {
		t.Fatalf("缺少配置的用户名 claim 时必须报错并点明字段：%v", err)
	}
}

func TestOIDCVerifierDiscoveryIsLazyAndFailureIsNotFatal(t *testing.T) {
	// IdP 不可达：构造不得失败（启动不受影响），失败只在具体调用时暴露。
	v := NewOIDCVerifier(config.OIDCConfig{
		Issuer: "http://127.0.0.1:1", ClientID: "c", ClientSecret: "s", RedirectURL: "http://localhost/cb",
	})
	flow := v.NewFlow()
	if _, err := v.AuthCodeURL(context.Background(), flow); err == nil {
		t.Fatal("IdP 不可达时构造授权地址应报错")
	}
}
