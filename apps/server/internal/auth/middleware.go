package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// principalKey 是 principal 在 gin.Context 中的键。
const principalKey = "auth.principal"

// Kind 区分呈递凭据的类型。
type Kind string

const (
	KindSession Kind = "session" // JWT 会话
	KindToken   Kind = "token"   // API Token
)

// 认证来源用于审计和管理面策略判断，不包含任何凭据内容。
const (
	AuthSourceWebJWT      = "web_jwt"
	AuthSourceBearerToken = "bearer_token"
	AuthSourceBasicToken  = "basic_token"
	AuthSourceCargoToken  = "cargo_token"
	AuthSourceBasic       = "basic"
)

// Principal 是通过认证的调用方主体。
type Principal struct {
	UserID     int64
	Username   string
	Role       string
	Kind       Kind
	AuthSource string
	TokenID    int64
	TokenName  string
	// Email 是账号绑定邮箱（可空）；供审计身份快照使用。
	Email            string
	WebLoginDisabled bool
	JTI              string    // 会话 jti（仅 KindSession），登出黑名单键
	ExpiresAt        time.Time // 会话过期时间（仅 KindSession）
}

// IsAdmin 判断主体是否管理员。
func (p *Principal) IsAdmin() bool { return p.Role == "admin" }

// Store 为中间件提供鉴权所需的持久化查询。由 repository 层实现（repository -> persistence）。
type Store interface {
	// IsTokenRevoked 判断会话 jti 是否已登出（在黑名单中）。
	IsTokenRevoked(jti string) (bool, error)
	// PrincipalByID 按用户 ID 载入主体；用户不存在或被停用返回错误。
	PrincipalByID(id int64) (*Principal, error)
	// PrincipalByTokenDigest 按 API Token 摘要载入主体；无匹配或已吊销返回错误。
	PrincipalByTokenDigest(digest string) (*Principal, error)
	// PrincipalByPassword 按用户名+口令验证并载入主体；验证失败返回错误。
	PrincipalByPassword(username, password string) (*Principal, error)
}

// ErrUnauthenticated 表示凭据缺失或无效。
var ErrUnauthenticated = errors.New("未认证")

// Authenticator 解析 Authorization: Bearer 凭据并注入 principal。
type Authenticator struct {
	jwt          *JWTManager
	store        Store
	protocol     bool
	cargo        bool
	allowedHosts func() []string // 允许访问的域名白名单（nil 表示不启用限制）
}

// Option 配置 Authenticator。
type Option func(*Authenticator)

// WithAllowedHosts 注入允许访问的域名白名单读取器（运行时读取最新值，如后台设置）。
// 非 nil 时，原生协议凭据在非 TLS、非回环来源下仍要求 Host 在白名单内才放行；
// 返回空列表或 nil 均不开放该兼容路径，非 TLS 外部请求保持拒绝。
func WithAllowedHosts(provider func() []string) Option {
	return func(a *Authenticator) { a.allowedHosts = provider }
}

// NewAuthenticator 构造中间件依赖。
func NewAuthenticator(jwtMgr *JWTManager, store Store, opts ...Option) *Authenticator {
	a := &Authenticator{jwt: jwtMgr, store: store}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Protocol 返回仅用于原生协议的认证中间件。协议 Bearer 只接受 jat_ API Token，
// 避免把 Web 会话 JWT 当作发布凭据使用；Basic 仍可使用账号密码。
func (a *Authenticator) Protocol() *Authenticator {
	copy := *a
	copy.protocol = true
	return &copy
}

// CargoProtocol 返回仅用于 Cargo 路由的认证中间件。
// Cargo registry token 使用不带认证方案的 Authorization 头。
func (a *Authenticator) CargoProtocol() *Authenticator {
	copy := *a
	copy.protocol = true
	copy.cargo = true
	return &copy
}

// Require 是强制认证的 Gin 中间件：解析失败即 401 并中止。
func (a *Authenticator) Require() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, err := a.resolveFor(c.Request, a.protocol)
		if err != nil {
			WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
			c.Abort()
			return
		}
		if a.rejectDisabledManagementPrincipal(c, principal) {
			return
		}
		c.Set(principalKey, principal)
		c.Next()
	}
}

// Optional 尝试解析凭据并注入 principal；匿名或无效凭据继续由 handler 决定是否放行。
// 管理面中被禁止 Web 登录的已认证主体必须在此集中拒绝，避免可选鉴权路由回落匿名权限。
func (a *Authenticator) Optional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if principal, err := a.resolveFor(c.Request, a.protocol); err == nil {
			if a.rejectDisabledManagementPrincipal(c, principal) {
				return
			}
			c.Set(principalKey, principal)
		} else if a.protocol && protocolCredentialsPresented(c.Request) {
			// 原生协议携带了凭据但认证失败时，不能回落为匿名请求进入 handler，
			// 否则 HTTP 传输限制会在鉴权后才被拒绝并留下无主体审计。
			WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
			c.Abort()
			return
		}
		c.Next()
	}
}

// protocolCredentialsPresented 判断请求是否显式携带原生协议可识别的凭据。
// 无凭据的公共读请求仍必须允许进入协议 handler 按仓库可见性处理。
func protocolCredentialsPresented(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("Authorization")) != "" ||
		strings.TrimSpace(r.Header.Get("X-NuGet-ApiKey")) != ""
}

func (a *Authenticator) rejectDisabledManagementPrincipal(c *gin.Context, principal *Principal) bool {
	if a.protocol || !principal.WebLoginDisabled {
		return false
	}
	WriteError(c, http.StatusForbidden, "web_login_disabled", "该账号已禁止登录管理端")
	c.Abort()
	return true
}

// resolve 从请求头解析出主体：优先 Authorization: Bearer（jat_ 前缀走 API Token，
// 否则按 JWT 会话处理）；无 Bearer 时回落 Authorization: Basic，先尝试 API Token，
// 若不以 jat_ 开头则尝试用户名+口令验证。
func (a *Authenticator) resolve(r *http.Request) (*Principal, error) {
	return a.resolveFor(r, false)
}

func (a *Authenticator) resolveFor(r *http.Request, protocol bool) (*Principal, error) {
	if protocol && a.cargo {
		if raw := cargoRegistryToken(r); raw != "" {
			if !a.protocolCredentialsAllowed(r) {
				return nil, ErrUnauthenticated
			}
			return a.resolveToken(raw, AuthSourceCargoToken)
		}
	}
	if raw := bearerToken(r); raw != "" {
		if strings.HasPrefix(raw, tokenPrefix) {
			if protocol && !a.protocolCredentialsAllowed(r) {
				return nil, ErrUnauthenticated
			}
			return a.resolveToken(raw, AuthSourceBearerToken)
		}
		if protocol {
			return nil, ErrUnauthenticated
		}
		return a.resolveSession(raw)
	}
	if protocol {
		if raw := strings.TrimSpace(r.Header.Get("X-NuGet-ApiKey")); raw != "" {
			if !a.protocolCredentialsAllowed(r) || !strings.HasPrefix(raw, tokenPrefix) {
				return nil, ErrUnauthenticated
			}
			return a.resolveToken(raw, AuthSourceBasicToken)
		}
	}
	user, pass := basicCredentials(r)
	if pass != "" {
		if strings.HasPrefix(pass, tokenPrefix) {
			if protocol && !a.protocolCredentialsAllowed(r) {
				return nil, ErrUnauthenticated
			}
			return a.resolveToken(pass, AuthSourceBasicToken)
		}
		// 尝试用户名+口令认证
		if user != "" {
			if protocol && !a.protocolCredentialsAllowed(r) {
				return nil, ErrUnauthenticated
			}
			p, err := a.store.PrincipalByPassword(user, pass)
			if err != nil {
				return nil, err
			}
			p.AuthSource = AuthSourceBasic
			return p, nil
		}
	}
	// 回落：token 放在 username 字段（如 curl -u jat_xxx:）
	if user != "" && strings.HasPrefix(user, tokenPrefix) {
		if protocol && !a.protocolCredentialsAllowed(r) {
			return nil, ErrUnauthenticated
		}
		return a.resolveToken(user, AuthSourceBasicToken)
	}
	return nil, ErrUnauthenticated
}

// resolveToken 按 API Token 明文载入主体；缺 jat_ 前缀或摘要无匹配返回 ErrUnauthenticated。
// Basic 凭据经此进入，因此不启用口令登录：只有合法 API Token 才被接受。
func (a *Authenticator) resolveToken(raw, source string) (*Principal, error) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return nil, ErrUnauthenticated
	}
	p, err := a.store.PrincipalByTokenDigest(DigestToken(raw))
	if err != nil {
		return nil, ErrUnauthenticated
	}
	p.Kind = KindToken
	p.AuthSource = source
	return p, nil
}

// resolveSession 按 JWT 会话令牌载入主体，并校验未登出、未过期、账号有效。
func (a *Authenticator) resolveSession(raw string) (*Principal, error) {
	claims, err := a.jwt.Parse(raw)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	revoked, err := a.store.IsTokenRevoked(claims.ID)
	if err != nil || revoked {
		return nil, ErrUnauthenticated
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	p, err := a.store.PrincipalByID(id)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	p.Kind = KindSession
	p.AuthSource = AuthSourceWebJWT
	p.JTI = claims.ID
	if claims.ExpiresAt != nil {
		p.ExpiresAt = claims.ExpiresAt.Time
	}
	return p, nil
}

// protocolCredentialsAllowed 限制原生协议凭据必须经 TLS、实际本机回环，或
// Host 白名单明确开放的 CDN HTTP 回源兼容链路传输。未配置或空白名单时，
// 非 TLS、非回环请求保持拒绝；Host 不能用于声明回环来源。
func (a *Authenticator) protocolCredentialsAllowed(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if isLoopbackRequest(r) {
		return true
	}
	if a.allowedHosts == nil {
		return false
	}
	list := a.allowedHosts()
	if len(list) == 0 {
		return false
	}
	return a.hostAllowed(r)
}

// isLoopbackRequest 只按服务端看到的 TCP 对端地址判定回环，不信任可伪造的 Host。
func isLoopbackRequest(r *http.Request) bool {
	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().Unmap().IsLoopback()
	}
	addr, err := netip.ParseAddr(strings.Trim(remoteAddr, "[]"))
	return err == nil && addr.Unmap().IsLoopback()
}

// hostAllowed 判断请求 Host 是否命中允许访问的域名白名单（纯匹配，不含回环放行）。
func (a *Authenticator) hostAllowed(r *http.Request) bool {
	host := strings.ToLower(r.Host)
	if i := strings.LastIndex(host, ":"); i > -1 {
		host = strings.Trim(host[:i], "[]")
	}
	for _, allowed := range a.allowedHosts() {
		if host == strings.ToLower(strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

// bearerToken 抽取 Authorization: Bearer <token> 中的凭据；不存在返回空串。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// cargoRegistryToken 抽取 Cargo 裸 registry token，仅由 Cargo 路由调用。
func cargoRegistryToken(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if raw == "" || strings.ContainsAny(raw, " \t") || !strings.HasPrefix(raw, tokenPrefix) {
		return ""
	}
	return raw
}

// basicCredentials 抽取 Authorization: Basic 中的 username 和 password。
// 解析失败返回空串。
func basicCredentials(r *http.Request) (user, pass string) {
	h := r.Header.Get("Authorization")
	const prefix = "Basic "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", ""
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", ""
	}
	return user, pass
}

// PrincipalFrom 从 gin.Context 取回已注入的主体；不存在返回 nil, false。
func PrincipalFrom(c *gin.Context) (*Principal, bool) {
	v, ok := c.Get(principalKey)
	if !ok {
		return nil, false
	}
	p, ok := v.(*Principal)
	return p, ok
}

// WriteError 以 docs/API.md 约定的嵌套信封写出错误响应。
func WriteError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
