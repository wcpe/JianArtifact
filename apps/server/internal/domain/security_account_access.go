package domain

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// UpdateForPrincipal 更新账号资格，并在最终事务重验原认证证明与管理员资格。
func (s *UserService) UpdateForPrincipal(ctx context.Context, p *auth.Principal, id int64, role, status string, disabled *bool) (*repository.User, error) {
	if p == nil || p.UserID <= 0 || p.Protocol || !p.IsAdmin() {
		return nil, ErrCredentialVersionConflict
	}
	return s.updateAccountAccess(ctx, p, id, role, status, disabled)
}

func (s *UserService) updateAccountAccess(ctx context.Context, p *auth.Principal, id int64, role, status string, disabled *bool) (*repository.User, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	e := repository.SecurityEvent{ActorKind: "local_operator", ProxyTrust: "unknown"}
	var proof *auth.CredentialProof
	if p != nil {
		value := p.CredentialProof()
		proof = &value
		e = securityAuditEvent(ctx, p.UserID)
		e.SessionID, e.CredentialID, e.AuthStrength = "", "", ""
		e.AuthMethod = p.AuthSource
	}
	e.EventID, e.OperationID = repository.NewSecurityEventID(), NewOperationID()
	e.OccurredAt = time.Now().UTC()
	e.EventType, e.Outcome, e.Committed = "auth.account_access_changed", "succeeded", "committed"
	u, changed, err := s.users.ApplyAccountAccess(ctx, repository.AccountAccessChange{ID: id, Role: role, Status: status, WebLoginDisabled: disabled, Proof: proof, Event: e, ReservedUsername: AnonymousUsername})
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return nil, ErrNotFound
	case errors.Is(err, repository.ErrAccountAccessInvalid):
		return nil, ErrValidation
	case errors.Is(err, repository.ErrCredentialRejected), errors.Is(err, repository.ErrPasswordVersionConflict):
		return nil, ErrCredentialVersionConflict
	case errors.Is(err, repository.ErrAccountAccessUnconfirmed):
		log.Printf("账号资格提交结果未确认，操作号=%s", e.OperationID)
		return nil, ErrSecurityAuditUnavailable
	case err != nil:
		return nil, ErrSecurityAuditUnavailable
	}
	if changed {
		s.recordChange(EntityUser, UserKey(u.Username), OpPut, UserChangeData{Username: u.Username, Role: u.Role, Status: u.Status, PasswordHash: u.PasswordHash, WebLoginDisabled: u.WebLoginDisabled, CreatedAt: u.CreatedAt})
	}
	return u, nil
}
