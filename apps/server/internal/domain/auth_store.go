package domain

import (
	"context"
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type authStore struct {
	users   *repository.UserRepo
	revoked *repository.RevokedRepo
}

func NewAuthStore(users *repository.UserRepo, _ *repository.TokenRepo, revoked *repository.RevokedRepo) auth.Store {
	return &authStore{users: users, revoked: revoked}
}

func (s *authStore) IsTokenRevoked(jti string) (bool, error) { return s.revoked.IsRevoked(jti) }

func (s *authStore) PrincipalByID(id int64) (*auth.Principal, error) {
	u, err := s.users.GetByID(id)
	if err != nil {
		return nil, err
	}
	if u.Status != "active" {
		return nil, auth.ErrUnauthenticated
	}
	return principalOf(u), nil
}

func (s *authStore) PrincipalBySession(proof auth.CredentialProof) (*auth.Principal, error) {
	u, err := s.users.CredentialSessionAccount(context.Background(), proof)
	if err != nil {
		return nil, credentialResolutionError(err)
	}
	p := principalOf(&u.User)
	p.AccountEpoch, p.SessionID = u.Epoch, proof.SessionID
	return p, nil
}

func (s *authStore) PrincipalByTokenDigest(digest string) (*auth.Principal, error) {
	u, token, err := s.users.CredentialTokenAccount(context.Background(), digest)
	if err != nil {
		return nil, credentialResolutionError(err)
	}
	p := principalOf(&u.User)
	p.AccountEpoch, p.TokenID, p.TokenName = u.Epoch, token.ID, token.Name
	p.TokenPurpose = token.Purpose
	if scopes, err := auth.ParseMachineScopes(token.ScopesJSON); err == nil {
		p.TokenScopes = scopes
	} else {
		// 存储内容损坏不降级放行：凭据按失效拒绝。
		return nil, auth.ErrUnauthenticated
	}
	return p, nil
}

func (s *authStore) PrincipalByPassword(username, password string) (*auth.Principal, error) {
	if username == AnonymousUsername {
		return nil, auth.ErrUnauthenticated
	}
	u, err := s.users.CredentialAccountByUsername(context.Background(), username)
	if err != nil {
		return nil, credentialResolutionError(err)
	}
	if u.Status != "active" || auth.VerifyPassword(password, u.PasswordHash) != nil {
		return nil, auth.ErrUnauthenticated
	}
	current, err := s.users.CredentialAccountByUsername(context.Background(), username)
	if err != nil {
		return nil, credentialResolutionError(err)
	}
	if current.ID != u.ID || current.Epoch != u.Epoch || current.PasswordHash != u.PasswordHash || current.Status != "active" {
		return nil, auth.ErrUnauthenticated
	}
	u = current
	p := principalOf(&u.User)
	p.AccountEpoch, p.AuthSource = u.Epoch, auth.AuthSourceBasic
	return p, nil
}

func credentialResolutionError(err error) error {
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrCredentialRejected) {
		return auth.ErrUnauthenticated
	}
	return auth.ErrCredentialStateUnavailable
}

func principalOf(u *repository.User) *auth.Principal {
	return &auth.Principal{UserID: u.ID, Username: u.Username, Role: u.Role, Email: u.Email, WebLoginDisabled: u.WebLoginDisabled}
}
