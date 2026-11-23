package domain

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ErrCredentialVersionConflict 拒绝基于陈旧观察值或已变化身份作出的改密。
var ErrCredentialVersionConflict = errors.New("账号或口令状态已变更")

// ChangePassword 供本机管理员命令使用；不从 HTTP 上下文继承来源。
func (s *UserService) ChangePassword(id int64, password string) error {
	e := repository.SecurityEvent{ActorKind: "local_operator", ProxyTrust: "unknown"}
	return s.changePassword(context.Background(), id, password, e)
}

// ChangePasswordForPrincipal 使用入口已验证的真实操作者，而非被改密的目标。
func (s *UserService) ChangePasswordForPrincipal(ctx context.Context, principal *auth.Principal, id int64, password string) error {
	if principal == nil || principal.UserID <= 0 || (!principal.IsAdmin() && principal.UserID != id) || principal.WebLoginDisabled {
		return ErrCredentialVersionConflict
	}
	e := securityAuditEvent(ctx, principal.UserID)
	// 旧 JTI 和令牌均不是新模型的安全关联标识，不复制到事件。
	e.SessionID, e.CredentialID, e.AuthStrength = "", "", ""
	e.AuthMethod = principal.AuthSource
	return s.changePassword(ctx, id, password, e)
}

func (s *UserService) changePassword(ctx context.Context, id int64, password string, event repository.SecurityEvent) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if password == "" {
		return ErrValidation
	}
	target, err := s.users.ReadCredentialPassword(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return ErrSecurityAuditUnavailable
	}
	if target.Username == AnonymousUsername {
		return ErrValidation
	}
	hash, err := compareAndHashPassword(password, target.PasswordHash)
	if err != nil {
		return ErrSecurityAuditUnavailable
	}
	event.EventID, event.OperationID = repository.NewSecurityEventID(), NewOperationID()
	event.EventType, event.Outcome, event.Committed = repository.SecurityEventPasswordChanged, "succeeded", "committed"
	event.OccurredAt = time.Now().UTC()
	changed, err := s.users.ApplyCredentialPassword(ctx, repository.CredentialPasswordChange{Event: event, Target: target, NewHash: hash})
	switch {
	case errors.Is(err, repository.ErrPasswordVersionConflict):
		return ErrCredentialVersionConflict
	case errors.Is(err, repository.ErrPasswordCommitUnknown):
		log.Printf("改密提交结果未确认，操作号=%s", event.OperationID)
		return ErrSecurityAuditUnavailable
	case err != nil:
		return ErrSecurityAuditUnavailable
	}
	if changed {
		s.recordChange(EntityUser, UserKey(target.Username), OpPut, UserChangeData{
			Username: target.Username, Role: target.Role, Status: target.Status, PasswordHash: hash,
			WebLoginDisabled: target.WebLoginDisabled, CreatedAt: target.CreatedAt,
		})
	}
	return nil
}

func compareAndHashPassword(password, oldHash string) (string, error) {
	// 外部身份可没有本地口令。对于已设置口令的账号，实际比对而非比较随机盐哈希。
	if oldHash != "" {
		err := auth.VerifyPassword(password, oldHash)
		if err == nil {
			return "", nil
		}
		if !errors.Is(err, auth.ErrPasswordMismatch) {
			return "", err
		}
	}
	return auth.HashPassword(password)
}
