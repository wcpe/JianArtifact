package domain

import (
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// authStore 用 repository 适配 auth.Store，供鉴权中间件解析主体。
type authStore struct {
	users   *repository.UserRepo
	tokens  *repository.TokenRepo
	revoked *repository.RevokedRepo
}

// NewAuthStore 构造 auth.Store 实现。
func NewAuthStore(users *repository.UserRepo, tokens *repository.TokenRepo, revoked *repository.RevokedRepo) auth.Store {
	return &authStore{users: users, tokens: tokens, revoked: revoked}
}

func (s *authStore) IsTokenRevoked(jti string) (bool, error) {
	return s.revoked.IsRevoked(jti)
}

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

func (s *authStore) PrincipalByTokenDigest(digest string) (*auth.Principal, error) {
	uid, err := s.tokens.UserIDByDigest(digest)
	if err != nil {
		return nil, err
	}
	u, err := s.users.GetByID(uid)
	if err != nil {
		return nil, err
	}
	if u.Status != "active" {
		return nil, auth.ErrUnauthenticated
	}
	return principalOf(u), nil
}

func (s *authStore) PrincipalByPassword(username, password string) (*auth.Principal, error) {
	u, err := s.users.GetByUsername(username)
	if err != nil {
		return nil, auth.ErrUnauthenticated
	}
	if u.Status != "active" {
		return nil, auth.ErrUnauthenticated
	}
	if err := auth.VerifyPassword(password, u.PasswordHash); err != nil {
		return nil, auth.ErrUnauthenticated
	}
	return principalOf(u), nil
}

func principalOf(u *repository.User) *auth.Principal {
	return &auth.Principal{UserID: u.ID, Username: u.Username, Role: u.Role}
}
