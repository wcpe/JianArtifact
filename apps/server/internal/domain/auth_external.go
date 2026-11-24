package domain

import (
	"context"
	"errors"
	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

var ErrExternalIdentityInvalid = errors.New("外部身份不可用")

// ExternalIdentity 是外部身份源已经核验的主体，不包含凭据原文。
type ExternalIdentity struct{ Source, Subject, Username, Email string }

// LoginExternal 为本地测试和调用方的即时、已核验身份适配；外部 HTTP 流程使用开始时的水位。
func (s *AuthService) LoginExternal(identity ExternalIdentity) (string, *repository.User, error) {
	ctx := context.Background()
	started, err := s.CredentialRevision(ctx)
	if err != nil {
		return "", nil, err
	}
	return s.LoginExternalContext(ctx, identity, started)
}

// LoginExternalContext 按预先观察的水位完成外部登录，不将过时认证重新绑定到当前代次。
func (s *AuthService) LoginExternalContext(ctx context.Context, identity ExternalIdentity, started int64) (string, *repository.User, error) {
	outcome, err := s.LoginExternalOutcome(ctx, identity, started, "")
	if errors.Is(err, ErrMfaRequired) {
		// 外部登录命中强因子门：旧签名拿不到挑战，按受限结果处理。
		return "", nil, err
	}
	var user *repository.User
	var token string
	if outcome != nil {
		user, token = outcome.User, outcome.Token
	}
	return token, user, err
}

// LoginExternalOutcome 与 LoginExternalContext 同路径但返回结构化裁决；
// 已启用强因子的账号得到 mfa_required + 受限挑战而非会话令牌。
func (s *AuthService) LoginExternalOutcome(ctx context.Context, identity ExternalIdentity, started int64, sessionBinding string) (*AuthOutcome, error) {
	method, ok := externalAuthAuditMethod(identity.Source)
	if !ok || identity.Subject == "" || identity.Username == "" || started < 0 {
		return nil, ErrInvalidCredentials
	}
	if identity.Username == AnonymousUsername {
		return nil, ErrExternalIdentityInvalid
	}
	outcome, err := s.issueCredentialOutcome(ctx, func(tx *sqlx.Tx) (repository.CredentialAccount, error) {
		u, err := repository.ExternalCredentialAccountTx(ctx, tx, identity.Source, identity.Subject, identity.Username, identity.Email, started)
		if errors.Is(err, repository.ErrExternalCredentialBinding) {
			err = ErrExternalIdentityInvalid
		}
		return u, err
	}, sessionBinding, true, false)
	var user *repository.User
	if outcome != nil {
		user = outcome.User
	}
	if err == nil && outcome.Next != AuthNextAuthenticated {
		err = ErrMfaRequired
	}
	s.recordAuthAttempt(ctx, method, user, err)
	return outcome, err
}

// RecordExternalAuthRejection 记录在外部身份绑定之前结束的尝试，调用方只传固定裁决码。
func (s *AuthService) RecordExternalAuthRejection(ctx context.Context, source, code string, outcome auth.ProviderAuthOutcome) {
	method, ok := externalAuthAuditMethod(source)
	if !ok || s.securityAudit == nil {
		return
	}
	switch outcome {
	case auth.ProviderAuthFailed:
		s.securityAudit.RecordAuthAttemptFailed(ctx, method, 0)
	case auth.ProviderAuthDenied:
		s.securityAudit.RecordDenied(ctx, SecurityAuditDenied{EventType: "auth.attempt", ErrorCode: code, AuthMethod: method})
	default:
		s.securityAudit.RecordAuthAttemptUnknown(ctx, method, 0)
	}
}

func externalAuthAuditMethod(source string) (string, bool) {
	switch source {
	case "ldap":
		return "ldap_bind", true
	case "oidc":
		return "oidc_callback", true
	}
	return "", false
}
