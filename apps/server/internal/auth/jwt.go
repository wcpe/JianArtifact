package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenTTL 是签发的会话 JWT 有效期。
const TokenTTL = 12 * time.Hour

// ErrInvalidToken 表示 JWT 校验失败（签名不符、过期或格式非法）。
var ErrInvalidToken = errors.New("无效的会话令牌")

// Claims 是会话 JWT 的载荷：subject 存用户 ID，附带角色与标准注册声明。
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// JWTManager 用 HS256 签发与校验会话令牌。密钥来自 config（不入库、不打印）。
type JWTManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // 便于测试注入
}

// NewJWTManager 构造 JWTManager。
func NewJWTManager(secret []byte) *JWTManager {
	return &JWTManager{secret: secret, ttl: TokenTTL, now: time.Now}
}

// Issue 为指定用户签发会话令牌，返回签名串、jti 与过期时间。
func (m *JWTManager) Issue(userID int64, role string) (token, jti string, expiresAt time.Time, err error) {
	now := m.now()
	expiresAt = now.Add(m.ttl)
	jti = newJTI()
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", userID),
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("签发会话令牌：%w", err)
	}
	return signed, jti, expiresAt, nil
}

// Parse 校验令牌签名与有效期，返回解析后的 Claims。
func (m *JWTManager) Parse(token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非预期的签名算法：%v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithTimeFunc(m.now))
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// newJTI 生成随机 jti（会话唯一标识，登出黑名单以此为键）。
func newJTI() string {
	return randomHex(16)
}
