package domain

import (
	"context"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ErrExternalIdentityInvalid 表示外部身份与本地账号的绑定不可用
// （用户名占用了内置主体、目标是管理员账号、或该账号已绑定其它外部身份）。
var ErrExternalIdentityInvalid = errors.New("外部身份不可用")

// ExternalIdentity 是外部身份源（OIDC / LDAP）校验通过后的身份信息，不含任何令牌原文。
type ExternalIdentity struct {
	// Source 是身份来源：oidc / ldap；写入 user.auth_source 并进入审计。
	Source string
	// Subject 是身份源内的稳定标识（OIDC sub / LDAP DN）。
	Subject string
	// Username 是建议用户名，仅在首次建号时使用。
	Username string
	// Email 用于审计与白名单判定，可为空。
	Email string
}

// LoginExternal 按外部身份绑定既有账号或建号，并签发与本地登录同构的会话
// （/35，见 ADR-0029）。绑定优先顺序：
// external_subject 命中 → 用户名命中且未绑定 → 新建账号（角色固定 user）。
//
// 一律拒绝：停用、禁止网页登录、内置 anonymous 主体、管理员账号的自动绑定
// （否则任何 IdP 用户只要用户名与管理员相同即可接管）、已绑定其它外部身份的账号。
func (s *AuthService) LoginExternal(identity ExternalIdentity) (token string, user *repository.User, err error) {
	if identity.Source == "" || identity.Subject == "" || identity.Username == "" {
		return "", nil, ErrInvalidCredentials
	}
	if identity.Username == AnonymousUsername {
		return "", nil, ErrExternalIdentityInvalid
	}

	u, err := s.users.GetByExternalSubject(identity.Subject)
	switch {
	case err == nil:
		if u.AuthSource != identity.Source {
			// 同一标识出现在不同来源：视为身份不可信，不静默沿用。
			return "", nil, ErrExternalIdentityInvalid
		}
	case errors.Is(err, repository.ErrNotFound):
		u, err = s.bindOrCreateExternal(identity)
		if err != nil {
			return "", nil, err
		}
	default:
		return "", nil, err
	}

	if u.Status != "active" || u.WebLoginDisabled {
		return "", nil, ErrInvalidCredentials
	}
	signed, _, _, err := s.jwt.Issue(u.ID, u.Role)
	if err != nil {
		return "", nil, err
	}
	return signed, u, nil
}

// LoginExternalContext 记录已核验的外部身份在本实例的登录结果，不记录外部标识。
func (s *AuthService) LoginExternalContext(ctx context.Context, identity ExternalIdentity) (string, *repository.User, error) {
	method, ok := externalAuthAuditMethod(identity.Source)
	token, user, err := s.LoginExternal(identity)
	if ok {
		s.recordAuthAttempt(ctx, method, user, err)
	}
	return token, user, err
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

func externalAuthAuditMethod(source string) (method string, ok bool) {
	switch source {
	case "ldap":
		return "ldap_bind", true
	case "oidc":
		return "oidc_callback", true
	default:
		return "", false
	}
}

// bindOrCreateExternal 在外部标识未绑定时：按用户名绑定本地普通账号，否则新建外部来源账号。
func (s *AuthService) bindOrCreateExternal(identity ExternalIdentity) (*repository.User, error) {
	existing, err := s.users.GetByUsername(identity.Username)
	switch {
	case err == nil:
		if existing.ExternalSubject != "" || existing.Role != "user" {
			return nil, ErrExternalIdentityInvalid
		}
		if err := s.users.BindExternalSubject(existing.ID, identity.Source, identity.Subject); err != nil {
			return nil, err
		}
		existing.AuthSource = identity.Source
		existing.ExternalSubject = identity.Subject
		return existing, nil
	case errors.Is(err, repository.ErrNotFound):
		id, err := s.users.CreateExternal(identity.Username, identity.Source, identity.Subject, identity.Email, "user")
		if err != nil {
			return nil, err
		}
		return s.users.GetByID(id)
	default:
		return nil, err
	}
}
