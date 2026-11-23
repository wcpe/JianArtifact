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
	Role         string `json:"role"`
	SessionID    string `json:"sid,omitempty"`
	AccountEpoch int64  `json:"ae,omitempty"`
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
	signed, claims, err := m.issue(userID, role, "", 0)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return signed, claims.ID, claims.ExpiresAt.Time, nil
}

// IssueBound 签发需在发布响应前持久化的会话载体。
func (m *JWTManager) IssueBound(userID int64, role string, epoch int64) (string, *Claims, error) {
	if userID <= 0 || epoch <= 0 {
		return "", nil, ErrInvalidToken
	}
	return m.issue(userID, role, newJTI(), epoch)
}

func (m *JWTManager) issue(userID int64, role, sessionID string, epoch int64) (string, *Claims, error) {
	now := m.now()
	claims := &Claims{
		Role: role, SessionID: sessionID, AccountEpoch: epoch,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: fmt.Sprintf("%d", userID), ID: newJTI(), IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", nil, fmt.Errorf("签发会话令牌：%w", err)
	}
	return signed, claims, nil
}

// Parse 校验令牌签名与有效期，返回解析后的 Claims。
func (m *JWTManager) Parse(token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非预期的签名算法：%v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithTimeFunc(m.now), jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// newJTI 生成随机 jti（会话唯一标识，登出黑名单以此为键）。
func newJTI() string {
	return randomHex(16)
}
