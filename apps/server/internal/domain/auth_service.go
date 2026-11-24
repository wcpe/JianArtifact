package domain

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

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
	mfa           *SecurityMfaService
}

// AuthNext 是登录签发的下一步裁决。
const (
	AuthNextAuthenticated = "authenticated"
	AuthNextMfaRequired   = "mfa_required"
	AuthNextClaimRequired = "claim_required"
)

// AuthOutcome 是登录签发的结构化结果：完整会话或受限挑战，二者互斥。
type AuthOutcome struct {
	Next           string
	Token          string
	User           *repository.User
	ChallengeID    string
	ChallengeToken string
	ExpiresAt      time.Time
}

// SetMfaService 注入强认证服务；nil 保持既有单因素行为（未装配环境兼容）。
func (s *AuthService) SetMfaService(m *SecurityMfaService) { s.mfa = m }

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
// 内置 anonymous 主体禁止登录。已启用强因子的账号返回 ErrMfaRequired——
// 旧签名的调用方拿不到挑战明细，受限结果的签发走 LoginOutcome。
func (s *AuthService) Login(username, password string) (token string, user *repository.User, err error) {
	outcome, err := s.LoginOutcome(context.Background(), username, password, "")
	if err != nil {
		return "", nil, err
	}
	if outcome.Next != AuthNextAuthenticated {
		return "", nil, ErrMfaRequired
	}
	return outcome.Token, outcome.User, nil
}

// LoginOutcome 校验口令后统一走签发裁决：已启用强因子签发 login_mfa 受限挑战而非会话；
// 第一因素（口令）失败一律 ErrInvalidCredentials，不区分后续裁决。
// attempt 记录包在本层：成功、受限成功与各类拒绝都要异步入库。
func (s *AuthService) LoginOutcome(ctx context.Context, username, password, sessionBinding string) (*AuthOutcome, error) {
	outcome, user, err := s.loginOutcome(ctx, username, password, sessionBinding)
	s.recordAuthAttempt(ctx, "password", user, err)
	return outcome, err
}

func (s *AuthService) loginOutcome(ctx context.Context, username, password, sessionBinding string) (*AuthOutcome, *repository.User, error) {
	if username == AnonymousUsername {
		return nil, nil, ErrInvalidCredentials
	}
	u, err := s.users.CredentialAccountByUsername(ctx, username)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, nil, err
	}
	if u.Status != "active" {
		return nil, &u.User, ErrInvalidCredentials
	}
	if u.WebLoginDisabled {
		return nil, &u.User, ErrInvalidCredentials
	}
	if err := auth.VerifyPassword(password, u.PasswordHash); err != nil {
		return nil, &u.User, ErrInvalidCredentials
	}
	outcome, err := s.issueCredentialOutcome(ctx, func(tx *sqlx.Tx) (repository.CredentialAccount, error) {
		return s.users.LoginCredentialAccountTx(ctx, tx, u)
	}, sessionBinding, true)
	// 第一因素已核验即返回用户供 attempt 记录；mfa_required 是受限成功。
	var user *repository.User
	if outcome != nil {
		user = outcome.User
	} else {
		user = &u.User
	}
	return outcome, user, err
}

// CompleteLoginMfa 兑现 login_mfa 挑战：OTP 未命中先独立事务扣尝试额再拒绝，
// 命中后消费挑战再签会话；挑战消费成功而会话失败属可重试降级，不会双发会话。
func (s *AuthService) CompleteLoginMfa(ctx context.Context, challengeToken, otp, sessionBinding string) (string, *repository.User, error) {
	if s.mfa == nil {
		return "", nil, ErrMfaFactorUnavailable
	}
	now := time.Now().Unix()
	tokenHash := hashChallengeToken(challengeToken)
	var challenge repository.SecurityAuthChallenge
	err := s.mfa.factors.Write(ctx, func(tx *sqlx.Tx) error {
		c, err := repository.TouchChallengeTx(ctx, tx, tokenHash, repository.ChallengePurposeLoginMFA, now)
		if err != nil {
			return err
		}
		if c.SessionBinding != "" && c.SessionBinding != sessionBinding {
			return repository.ErrSecurityChallengeRejected
		}
		challenge = c
		return nil
	})
	if err != nil {
		return "", nil, mapChallengeErr(err)
	}
	account, err := s.users.CredentialAccountByID(ctx, challenge.UserID)
	if err != nil || account.Epoch != challenge.AccountEpoch || account.Status != "active" || account.WebLoginDisabled {
		return "", nil, ErrInvalidAuthChallenge
	}
	if err := s.mfa.VerifyTotp(ctx, challenge.UserID, otp); err != nil {
		if errors.Is(err, ErrMfaFactorUnavailable) {
			return "", nil, err
		}
		// 口令未命中消耗尝试额度（独立事务，不随拒绝回滚）。
		_ = s.mfa.factors.Write(ctx, func(tx *sqlx.Tx) error {
			_, werr := repository.FailChallengeAttemptTx(ctx, tx, challenge.ChallengeID, now)
			return werr
		})
		return "", nil, ErrInvalidAuthChallenge
	}
	if err := s.mfa.factors.Write(ctx, func(tx *sqlx.Tx) error {
		return repository.ConsumeChallengeTx(ctx, tx, challenge.ChallengeID, now)
	}); err != nil {
		return "", nil, ErrInvalidAuthChallenge
	}
	outcome, err := s.issueCredentialOutcome(ctx, func(tx *sqlx.Tx) (repository.CredentialAccount, error) {
		return s.users.LoginCredentialAccountTx(ctx, tx, account)
	}, sessionBinding, false)
	if err != nil {
		return "", nil, err
	}
	if outcome.Next != AuthNextAuthenticated {
		return "", nil, ErrInvalidAuthChallenge
	}
	return outcome.Token, outcome.User, nil
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
	outcome, err := s.issueCredentialOutcome(ctx, func(tx *sqlx.Tx) (repository.CredentialAccount, error) {
		u, err := repository.BootstrapCredentialAccountTx(ctx, tx, username, hash, AnonymousUsername)
		if errors.Is(err, repository.ErrCredentialRejected) {
			err = ErrAlreadyInitialized
		}
		return u, err
	}, "", true)
	if err != nil {
		return "", nil, err
	}
	return outcome.Token, outcome.User, nil
}

// issueCredentialOutcome 是唯一的会话签发入口。已启用强因子的账号在同一事务内
// 改写为签发 login_mfa 受限挑战——挑战与会话互斥签发，不存在二者同时成立的中间态。。mfaGate 为真时先查活跃 TOTP 因子：
// 有则转 mfa_required 而非直签会话；CompleteLoginMfa 传 false（挑战已兑现，不再绕回门）。
func (s *AuthService) issueCredentialOutcome(ctx context.Context, account func(*sqlx.Tx) (repository.CredentialAccount, error), sessionBinding string, mfaGate bool) (*AuthOutcome, error) {
	var outcome *AuthOutcome
	err := s.users.CredentialWrite(ctx, func(tx *sqlx.Tx) error {
		current, err := account(tx)
		if err != nil {
			return err
		}
		o := &AuthOutcome{Next: AuthNextAuthenticated, User: &current.User}
		if mfaGate && s.mfa != nil {
			has, ferr := repository.HasActiveTotpFactorTx(ctx, tx, current.ID)
			if ferr != nil {
				return ferr
			}
			if has {
				ch, challengeToken, cerr := s.mfa.issueChallenge(ctx, tx, current.ID, current.Epoch, repository.ChallengePurposeLoginMFA, "login", sessionBinding)
				if cerr != nil {
					return cerr
				}
				o.Next = AuthNextMfaRequired
				o.ChallengeID = ch.ChallengeID
				o.ChallengeToken = challengeToken
				o.ExpiresAt = time.Unix(ch.ExpiresAt, 0)
				outcome = o
				return nil
			}
		}
		signed, claims, err := s.jwt.IssueBound(current.ID, current.Role, current.Epoch)
		if err != nil {
			return err
		}
		if err := repository.InsertCredentialSessionTx(ctx, tx, current, claims); err != nil {
			return err
		}
		o.Token = signed
		outcome = o
		return nil
	})
	if errors.Is(err, repository.ErrCredentialRejected) {
		err = ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return outcome, nil
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
