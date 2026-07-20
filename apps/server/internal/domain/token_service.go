package domain

import (
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TokenService 处理 API Token 的签发、列表与吊销。
type TokenService struct {
	tokens *repository.TokenRepo
}

// NewTokenService 构造 TokenService。
func NewTokenService(tokens *repository.TokenRepo) *TokenService {
	return &TokenService{tokens: tokens}
}

// List 返回用户名下未吊销的 Token（不含明文 / 摘要）。
func (s *TokenService) List(userID int64) ([]repository.Token, error) {
	return s.tokens.ListByUser(userID)
}

// Create 生成一枚 Token，仅存摘要，返回明文（仅此次）与记录。
func (s *TokenService) Create(userID int64, name string) (plaintext string, tok *repository.Token, err error) {
	plain, digest := auth.GenerateToken()
	id, err := s.tokens.Create(userID, name, digest)
	if err != nil {
		return "", nil, err
	}
	return plain, &repository.Token{ID: id, UserID: userID, Name: name}, nil
}

// Delete 吊销用户名下的 Token。
func (s *TokenService) Delete(id, userID int64) error {
	return mapNotFound(s.tokens.Delete(id, userID))
}
