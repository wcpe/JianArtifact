package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// OIDCFlowCookieName 是授权码流程状态的 Cookie 名（HttpOnly；回调时校验并清除）。
const OIDCFlowCookieName = "ja_oidc_flow"

// oidcFlowPurpose 是流程签名密钥的派生用途串，避免与其它派生密钥同源复用。
const oidcFlowPurpose = "jianartifact/oidc-flow/v1"

// OIDCFlowLifetime 是一次授权码流程的最长有效期（跨重定向的短时状态）。
const OIDCFlowLifetime = 10 * time.Minute

// OIDCFlow 是授权码流程中必须跨重定向保存的短时数据。
// 它只含随机数与 PKCE 校验串，不含任何令牌；以签名 Cookie 形式保存在浏览器，不落库。
type OIDCFlow struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
	ExpiresAt    int64  `json:"expires_at"` // Unix 秒
}

// OIDCFlowSigner 用启动密钥派生的 HMAC 对流程数据做完整性签名（无服务端状态）。
type OIDCFlowSigner struct {
	key []byte
}

// NewOIDCFlowSigner 由启动密钥派生流程签名密钥。
func NewOIDCFlowSigner(secret []byte) *OIDCFlowSigner {
	sum := sha256.Sum256(append(append([]byte(nil), secret...), []byte(oidcFlowPurpose)...))
	return &OIDCFlowSigner{key: sum[:]}
}

// Encode 编码流程数据：base64url(JSON) + "." + base64url(HMAC-SHA256)。
func (s *OIDCFlowSigner) Encode(flow OIDCFlow) (string, error) {
	payload, err := json.Marshal(flow)
	if err != nil {
		return "", fmt.Errorf("编码 OIDC 流程状态：%w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Decode 校验签名与有效期并还原流程数据；签名不符、格式错误或已过期均返回错误。
func (s *OIDCFlowSigner) Decode(value string, now time.Time) (OIDCFlow, error) {
	body, sig, ok := strings.Cut(value, ".")
	if !ok {
		return OIDCFlow{}, errors.New("OIDC 流程状态格式非法")
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return OIDCFlow{}, errors.New("OIDC 流程状态签名非法")
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return OIDCFlow{}, errors.New("OIDC 流程状态签名不符")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return OIDCFlow{}, errors.New("OIDC 流程状态载荷非法")
	}
	var flow OIDCFlow
	if err := json.Unmarshal(payload, &flow); err != nil {
		return OIDCFlow{}, errors.New("OIDC 流程状态载荷非法")
	}
	if flow.ExpiresAt <= now.Unix() {
		return OIDCFlow{}, errors.New("OIDC 流程状态已过期")
	}
	return flow, nil
}

// OIDCClaims 是登录所需的最小声明集合（不含任何令牌原文）。
type OIDCClaims struct {
	Subject  string
	Username string
	Email    string
}

// OIDCVerifier 执行 OIDC 协议侧：discovery（惰性）、PKCE 与 ID Token 验签。
type OIDCVerifier struct {
	cfg config.OIDCConfig

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDCVerifier 构造协议侧校验器；调用方保证 cfg.Enabled() 为真。
func NewOIDCVerifier(cfg config.OIDCConfig) *OIDCVerifier {
	return &OIDCVerifier{cfg: cfg}
}

// endpoints 惰性完成 discovery 并缓存：IdP 在启动时不可达不应阻止本服务启动
// （本地管理员必须始终可登录，见 ADR-0029），因此探测失败只在登录时暴露。
func (v *OIDCVerifier) endpoints(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.oauth != nil && v.verifier != nil {
		return v.oauth, v.verifier, nil
	}
	provider, err := oidc.NewProvider(ctx, v.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("发现 OIDC 提供方：%w", err)
	}
	v.oauth = &oauth2.Config{
		ClientID:     v.cfg.ClientID,
		ClientSecret: v.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  v.cfg.RedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	v.verifier = provider.Verifier(&oidc.Config{ClientID: v.cfg.ClientID})
	return v.oauth, v.verifier, nil
}

// NewFlow 生成一次授权码流程的短时状态（state / nonce / PKCE 校验串）。
func (v *OIDCVerifier) NewFlow() OIDCFlow {
	return OIDCFlow{
		State:        randomHex(32),
		Nonce:        randomHex(32),
		CodeVerifier: oauth2.GenerateVerifier(),
		ExpiresAt:    time.Now().Add(OIDCFlowLifetime).Unix(),
	}
}

// AuthCodeURL 构造 IdP 授权地址（PKCE S256 + nonce）。
func (v *OIDCVerifier) AuthCodeURL(ctx context.Context, flow OIDCFlow) (string, error) {
	oauthCfg, _, err := v.endpoints(ctx)
	if err != nil {
		return "", err
	}
	return oauthCfg.AuthCodeURL(
		flow.State,
		oidc.Nonce(flow.Nonce),
		oauth2.S256ChallengeOption(flow.CodeVerifier),
	), nil
}

// Exchange 用授权码换取并校验身份：验证 ID Token 签名与声明，校验 nonce，
// 返回最小声明集合；任何一步失败都返回错误（调用方不得据此放行登录）。
func (v *OIDCVerifier) Exchange(ctx context.Context, code string, flow OIDCFlow) (OIDCClaims, error) {
	oauthCfg, verifier, err := v.endpoints(ctx)
	if err != nil {
		return OIDCClaims{}, err
	}
	token, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(flow.CodeVerifier))
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("兑换授权码：%w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return OIDCClaims{}, errors.New("令牌响应缺少 id_token")
	}
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("校验 ID Token：%w", err)
	}
	if idToken.Nonce != flow.Nonce {
		return OIDCClaims{}, errors.New("ID Token 的 nonce 与流程状态不符")
	}
	return v.claimsFrom(idToken)
}

// claimsFrom 从已验签的 ID Token 中提取登录所需声明。
func (v *OIDCVerifier) claimsFrom(idToken *oidc.IDToken) (OIDCClaims, error) {
	var raw map[string]any
	if err := idToken.Claims(&raw); err != nil {
		return OIDCClaims{}, fmt.Errorf("解析 ID Token 声明：%w", err)
	}
	claims := OIDCClaims{
		Subject:  idToken.Subject,
		Username: stringClaim(raw, v.cfg.UsernameClaim),
		Email:    stringClaim(raw, "email"),
	}
	if claims.Subject == "" {
		return OIDCClaims{}, errors.New("ID Token 缺少 sub")
	}
	if claims.Username == "" {
		return OIDCClaims{}, fmt.Errorf("ID Token 缺少用户名字段（%s）", v.cfg.UsernameClaim)
	}
	// 邮箱仅作审计与白名单输入：IdP 明确标记未验证时一律拒绝，避免用他人邮箱冒充。
	if verified, ok := raw["email_verified"].(bool); ok && !verified && claims.Email != "" {
		return OIDCClaims{}, errors.New("ID Token 的邮箱未通过验证")
	}
	return claims, nil
}

// stringClaim 取字符串声明；非字符串与缺失都视为空串。
func stringClaim(raw map[string]any, name string) string {
	value, _ := raw[name].(string)
	return strings.TrimSpace(value)
}
