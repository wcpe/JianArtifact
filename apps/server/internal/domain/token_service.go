package domain

import (
	"log"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TokenService 处理 API Token 的签发、列表与吊销。
type TokenService struct {
	tokens    *repository.TokenRepo
	users     *repository.UserRepo
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
}

// NewTokenService 构造 TokenService。
func NewTokenService(tokens *repository.TokenRepo, users *repository.UserRepo) *TokenService {
	return &TokenService{tokens: tokens, users: users}
}

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
func (s *TokenService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *TokenService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// recordChange 记录复制变更日志；记录失败不阻断业务写（对账兜底，见 ADR-0013）。
func (s *TokenService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
}

// List 返回用户名下未吊销的 Token（不含明文 / 摘要）。
func (s *TokenService) List(userID int64) ([]repository.Token, error) {
	return s.tokens.ListByUser(userID)
}

// Create 生成一枚 Token，仅存摘要，返回明文（仅此次）与记录。
func (s *TokenService) Create(userID int64, name string) (plaintext string, tok *repository.Token, err error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return "", nil, err
	}
	plain, digest := auth.GenerateToken()
	id, err := s.tokens.Create(userID, name, digest)
	if err != nil {
		return "", nil, err
	}
	u, err := s.users.GetByID(userID)
	if err == nil {
		s.recordChange(EntityToken, TokenKey(digest), OpPut, TokenChangeData{
			Hash: digest, Username: u.Username, Name: name,
		})
	}
	return plain, &repository.Token{ID: id, UserID: userID, Name: name}, nil
}

// Delete 吊销用户名下的 Token。
func (s *TokenService) Delete(id, userID int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	stored, err := s.tokens.GetByIDAndUser(id, userID)
	if err != nil {
		return mapNotFound(err)
	}
	if err := s.tokens.Delete(id, userID); err != nil {
		return mapNotFound(err)
	}
	s.recordChange(EntityToken, TokenKey(stored.Digest), OpDelete, TombstoneData{Deleted: true})
	return nil
}
