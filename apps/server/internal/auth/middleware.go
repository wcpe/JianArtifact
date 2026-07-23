package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
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

// Principal 是通过认证的调用方主体。
type Principal struct {
	UserID    int64
	Username  string
	Role      string
	Kind      Kind
	JTI       string    // 会话 jti（仅 KindSession），登出黑名单键
	ExpiresAt time.Time // 会话过期时间（仅 KindSession）
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
}

// ErrUnauthenticated 表示凭据缺失或无效。
var ErrUnauthenticated = errors.New("未认证")

// Authenticator 解析 Authorization: Bearer 凭据并注入 principal。
type Authenticator struct {
	jwt   *JWTManager
	store Store
}

// NewAuthenticator 构造中间件依赖。
func NewAuthenticator(jwtMgr *JWTManager, store Store) *Authenticator {
	return &Authenticator{jwt: jwtMgr, store: store}
}

// Require 是强制认证的 Gin 中间件：解析失败即 401 并中止。
func (a *Authenticator) Require() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, err := a.resolve(c.Request)
		if err != nil {
			WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
			c.Abort()
			return
		}
		c.Set(principalKey, principal)
		c.Next()
	}
}

// Optional 尝试解析凭据并注入 principal，但无论成功与否都放行；
// 由各 handler 自行按需强制认证 / 鉴权。用于公开与受保护端点共存的路由集。
func (a *Authenticator) Optional() gin.HandlerFunc {
	return func(c *gin.Context) {
		if principal, err := a.resolve(c.Request); err == nil {
			c.Set(principalKey, principal)
		}
		c.Next()
	}
}

// resolve 从请求头解析出主体：优先 Authorization: Bearer（jat_ 前缀走 API Token，
// 否则按 JWT 会话处理）；无 Bearer 时回落 Authorization: Basic，其凭据仅接受 API Token。
func (a *Authenticator) resolve(r *http.Request) (*Principal, error) {
	if raw := bearerToken(r); raw != "" {
		if strings.HasPrefix(raw, tokenPrefix) {
			return a.resolveToken(raw)
		}
		return a.resolveSession(raw)
	}
	if cred := basicCredential(r); cred != "" {
		return a.resolveToken(cred)
	}
	return nil, ErrUnauthenticated
}

// resolveToken 按 API Token 明文载入主体；缺 jat_ 前缀或摘要无匹配返回 ErrUnauthenticated。
// Basic 凭据经此进入，因此不启用口令登录：只有合法 API Token 才被接受。
func (a *Authenticator) resolveToken(raw string) (*Principal, error) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return nil, ErrUnauthenticated
	}
	p, err := a.store.PrincipalByTokenDigest(DigestToken(raw))
	if err != nil {
		return nil, ErrUnauthenticated
	}
	p.Kind = KindToken
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
	p.JTI = claims.ID
	if claims.ExpiresAt != nil {
		p.ExpiresAt = claims.ExpiresAt.Time
	}
	return p, nil
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

// basicCredential 抽取 Authorization: Basic 中的候选凭据：base64 解码 "user:pass" 后
// 取 password；password 为空则回落取 username。仅用于承载 API Token，便于 curl 等
// 原生客户端以 `-u <token>:` 或 `-u :<token>` 呈递。解析失败返回空串。
func basicCredential(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Basic "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return ""
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return ""
	}
	if pass != "" {
		return pass
	}
	return user
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
