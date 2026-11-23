package domain

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AuthService 处理自举、登录与登出。
type AuthService struct {
	users         *repository.UserRepo
	revoked       *repository.RevokedRepo
	jwt           *auth.JWTManager
	recorder      ChangeRecorder
	writeGate     BusinessWriteGate
	securityAudit *SecurityAuditService
}

// SetSecurityAudit 在装配阶段注入实例安全审计服务。
func (s *AuthService) SetSecurityAudit(audit *SecurityAuditService) { s.securityAudit = audit }

// LoginContext 只记录实际口令核验，不声明已建立持久会话或已写出 HTTP 响应。
func (s *AuthService) LoginContext(ctx context.Context, username, password string) (string, *repository.User, error) {
	token, user, err := s.Login(username, password)
	s.recordAuthAttempt(ctx, "password", user, err)
	return token, user, err
}

func (s *AuthService) recordAuthAttempt(ctx context.Context, method string, user *repository.User, err error) {
	if s.securityAudit == nil {
		return
	}
	switch {
	case err == nil && user != nil:
		s.securityAudit.RecordAuthAttemptSucceeded(ctx, user.ID, method, 0)
	case errors.Is(err, ErrInvalidCredentials), errors.Is(err, ErrExternalIdentityInvalid):
		s.securityAudit.RecordDenied(ctx, SecurityAuditDenied{EventType: "auth.attempt", ErrorCode: "invalid_credentials", AuthMethod: method})
	default:
		s.securityAudit.RecordAuthAttemptFailed(ctx, method, 0)
	}
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
	if username == AnonymousUsername {
		return "", nil, ErrValidation
	}
	token, user, err = s.createSessionForNewUser(username, password)
	if err != nil {
		return "", nil, err
	}
	s.recordUserChange(user)
	return token, user, nil
}

// Login 校验口令并签发会话令牌。用户不存在 / 停用 / 口令错误统一返回 ErrInvalidCredentials。
// 内置 anonymous 主体禁止登录。
func (s *AuthService) Login(username, password string) (token string, user *repository.User, err error) {
	if username == AnonymousUsername {
		return "", nil, ErrInvalidCredentials
	}
	ctx := context.Background()
	u, err := s.users.CredentialAccountByUsername(ctx, username)
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
	return s.issueCredentialSession(ctx, func(tx *sqlx.Tx) (repository.CredentialAccount, error) {
		return s.users.LoginCredentialAccountTx(ctx, tx, u)
	})
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

func (s *AuthService) createSessionForNewUser(username, password string) (string, *repository.User, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", nil, err
	}
	ctx := context.Background()
	return s.issueCredentialSession(ctx, func(tx *sqlx.Tx) (repository.CredentialAccount, error) {
		u, err := repository.BootstrapCredentialAccountTx(ctx, tx, username, hash, AnonymousUsername)
		if errors.Is(err, repository.ErrCredentialRejected) {
			err = ErrAlreadyInitialized
		}
		return u, err
	})
}

func (s *AuthService) issueCredentialSession(ctx context.Context, account func(*sqlx.Tx) (repository.CredentialAccount, error)) (string, *repository.User, error) {
	var token string
	var user repository.CredentialAccount
	err := s.users.CredentialWrite(ctx, func(tx *sqlx.Tx) error {
		current, err := account(tx)
		if err != nil {
			return err
		}
		signed, claims, err := s.jwt.IssueBound(current.ID, current.Role, current.Epoch)
		if err != nil {
			return err
		}
		if err := repository.InsertCredentialSessionTx(ctx, tx, current, claims); err != nil {
			return err
		}
		user, token = current, signed
		return nil
	})
	if errors.Is(err, repository.ErrCredentialRejected) {
		err = ErrInvalidCredentials
	}
	if err != nil {
		return "", nil, err
	}
	return token, &user.User, nil
}

// CredentialRevision 固定外部认证开始时的库内签发水位。
func (s *AuthService) CredentialRevision(ctx context.Context) (int64, error) {
	return s.users.CredentialRevision(ctx)
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
