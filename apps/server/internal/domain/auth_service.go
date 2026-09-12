package domain

import (
	"errors"
	"log"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AuthService 处理自举、登录与登出。
type AuthService struct {
	users     *repository.UserRepo
	revoked   *repository.RevokedRepo
	jwt       *auth.JWTManager
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
}

// NewAuthService 构造 AuthService。
func NewAuthService(users *repository.UserRepo, revoked *repository.RevokedRepo, jwtMgr *auth.JWTManager) *AuthService {
	return &AuthService{users: users, revoked: revoked, jwt: jwtMgr}
}

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *AuthService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// SetChangeRecorder 注入首个管理员自举的复制变更记录器；nil 表示不记录。
func (s *AuthService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// Bootstrap 在 user 表为空（不含内置 anonymous）时创建首个管理员并返回会话令牌；
// 否则 ErrAlreadyInitialized。
func (s *AuthService) Bootstrap(username, password string) (token string, user *repository.User, err error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return "", nil, err
	}
	n, err := s.users.CountExcluding(AnonymousUsername)
	if err != nil {
		return "", nil, err
	}
	if n > 0 {
		return "", nil, ErrAlreadyInitialized
	}
	token, user, err = s.createSessionForNewUser(username, password, "admin")
	if err != nil {
		return "", nil, err
	}
	s.recordUserChange(user)
	return token, user, nil
}

// Login 校验口令并签发会话令牌。用户不存在 / 停用 / 口令错误统一返回 ErrInvalidCredentials。
// 内置 anonymous 主体禁止登录（FR-66）。
func (s *AuthService) Login(username, password string) (token string, user *repository.User, err error) {
	if username == AnonymousUsername {
		return "", nil, ErrInvalidCredentials
	}
	u, err := s.users.GetByUsername(username)
	if errors.Is(err, repository.ErrNotFound) {
		return "", nil, ErrInvalidCredentials
	}
	if err != nil {
		return "", nil, err
	}
	if u.Status != "active" {
		return "", nil, ErrInvalidCredentials
	}
	if u.WebLoginDisabled {
		return "", nil, ErrInvalidCredentials
	}
	if err := auth.VerifyPassword(password, u.PasswordHash); err != nil {
		return "", nil, ErrInvalidCredentials
	}
	signed, _, _, err := s.jwt.Issue(u.ID, u.Role)
	if err != nil {
		return "", nil, err
	}
	return signed, u, nil
}

// Logout 把会话 jti 记入黑名单直至过期。expiresAt 为 Unix 秒。
func (s *AuthService) Logout(jti string, expiresAt int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if jti == "" {
		return nil
	}
	return s.revoked.Revoke(jti, expiresAt)
}

func (s *AuthService) createSessionForNewUser(username, password, role string) (string, *repository.User, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", nil, err
	}
	id, err := s.users.Create(username, hash, role)
	if err != nil {
		if isUniqueViolation(err) {
			return "", nil, ErrConflict
		}
		return "", nil, err
	}
	u, err := s.users.GetByID(id)
	if err != nil {
		return "", nil, err
	}
	signed, _, _, err := s.jwt.Issue(u.ID, u.Role)
	if err != nil {
		return "", nil, err
	}
	return signed, u, nil
}

func (s *AuthService) recordUserChange(user *repository.User) {
	if s.recorder == nil {
		return
	}
	err := s.recorder.Record(EntityUser, UserKey(user.Username), OpPut, UserChangeData{
		Username: user.Username, Role: user.Role, Status: user.Status, PasswordHash: user.PasswordHash, WebLoginDisabled: user.WebLoginDisabled, CreatedAt: user.CreatedAt,
	})
	if err != nil {
		log.Printf("首个管理员复制变更记录失败：%v", err)
	}
}

// isUniqueViolation 识别 SQLite 唯一约束冲突（modernc.org/sqlite 错误文本）。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
