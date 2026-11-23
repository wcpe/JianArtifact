package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ErrCredentialScopeDenied 表示机器凭据的用途或作用域不合法或超出签发者可授范围。
var ErrCredentialScopeDenied = errors.New("机器凭据用途或作用域不允许")

// TokenService 处理 API Token 的签发、列表与吊销。
type TokenService struct {
	tokens    *repository.TokenRepo
	users     *repository.UserRepo
	repos     *RepositoryService
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
}

// NewTokenService 构造 TokenService。
func NewTokenService(tokens *repository.TokenRepo, users *repository.UserRepo) *TokenService {
	return &TokenService{tokens: tokens, users: users}
}

// SetRepositoryService 注入仓库服务，用于签发受限机器凭据时解析仓库与签发者可授动作。
func (s *TokenService) SetRepositoryService(r *RepositoryService) { s.repos = r }

// SetChangeRecorder 注入复制变更日志记录器；nil 表示不记录。
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

const (
	// MachineTokenScopeLimit 是单枚机器凭据的作用域条数上限。
	MachineTokenScopeLimit = 16
	// machineTokenDefaultDays 与 machineTokenMaxDays 是机器凭据默认与最长有效期。
	machineTokenDefaultDays = 30
	machineTokenMaxDays     = 90
)

// CreateTokenInput 是受限机器凭据签发请求；Purpose 为空时按旧式未限用途凭据签发（过渡）。
type CreateTokenInput struct {
	Name        string
	Purpose     string
	Scopes      []MachineTokenScopeInput
	ExpiresDays int
}

// MachineTokenScopeInput 是一条作用域请求：仓库名在签发时解析为稳定 ID 持久化。
type MachineTokenScopeInput struct {
	Repository string
	PathPrefix string
	Actions    []string
}

// Create 生成一枚 Token，仅存摘要，返回明文（仅此次）与记录。
func (s *TokenService) Create(userID int64, name string) (string, *repository.Token, error) {
	return s.createCredential(context.Background(), userID, repository.CredentialTokenIssue{Name: name}, nil)
}

// CreateForPrincipal 沿用旧式签发路径（npm login 等协议登录、管理端仅给名称）。
func (s *TokenService) CreateForPrincipal(ctx context.Context, p *auth.Principal, name string) (string, *repository.Token, error) {
	return s.CreateScopedForPrincipal(ctx, p, CreateTokenInput{Name: name})
}

// CreateScopedForPrincipal 签发机器凭据；用途受限时校验作用域、期限与签发者可授动作。
// 机器 Token 不是交互会话，不能以此再签发任何凭据。
func (s *TokenService) CreateScopedForPrincipal(ctx context.Context, p *auth.Principal, in CreateTokenInput) (string, *repository.Token, error) {
	if p == nil || p.UserID <= 0 {
		return "", nil, ErrCredentialVersionConflict
	}
	issue, err := s.resolveIssue(ctx, p, in)
	if err != nil {
		return "", nil, err
	}
	proof := p.CredentialProof()
	// Token 通道发证受约束：受限机器凭据不得再发任何凭据（杜绝提权回链），
	// 旧式 Token 保留互发旧式 Token 的过渡兼容，但任何 Token 都不能签发受限凭据。
	if proof.Kind == auth.KindToken && (p.TokenPurpose != auth.TokenPurposeLegacy || in.Purpose != auth.TokenPurposeLegacy) {
		return "", nil, ErrCredentialScopeDenied
	}
	return s.createCredential(ctx, p.UserID, issue, &proof)
}

// resolveIssue 把签发输入定形为持久化字段；任何校验失败都不落库。
func (s *TokenService) resolveIssue(ctx context.Context, p *auth.Principal, in CreateTokenInput) (repository.CredentialTokenIssue, error) {
	issue := repository.CredentialTokenIssue{Name: in.Name, Purpose: in.Purpose}
	switch in.Purpose {
	case auth.TokenPurposeLegacy:
		if len(in.Scopes) != 0 {
			return issue, fmt.Errorf("%w：未限用途凭据不能携带作用域", ErrCredentialScopeDenied)
		}
	case auth.TokenPurposeArtifactRead, auth.TokenPurposeArtifactPublish:
		if len(in.Scopes) == 0 || len(in.Scopes) > MachineTokenScopeLimit {
			return issue, fmt.Errorf("%w：作用域需 1-%d 条", ErrCredentialScopeDenied, MachineTokenScopeLimit)
		}
	default:
		return issue, fmt.Errorf("%w：未知用途 %q", ErrCredentialScopeDenied, in.Purpose)
	}
	days := in.ExpiresDays
	if days <= 0 {
		days = machineTokenDefaultDays
	}
	if days > machineTokenMaxDays {
		return issue, fmt.Errorf("%w：期限不能超过 %d 天", ErrCredentialScopeDenied, machineTokenMaxDays)
	}
	if in.Purpose == auth.TokenPurposeLegacy {
		return issue, nil
	}
	if s.repos == nil {
		// 受限签发需要仓库/ACL 权威；未装配时不降级为未校验签发。
		return issue, fmt.Errorf("%w：凭据授权服务未装配", ErrCredentialScopeDenied)
	}
	subject, err := s.repos.SubjectFor(p.UserID)
	if err != nil {
		return issue, err
	}
	scopes := make([]auth.MachineTokenScope, 0, len(in.Scopes))
	for i, sc := range in.Scopes {
		prefix, err := canonicalScopePrefix(sc.PathPrefix)
		if err != nil {
			return issue, fmt.Errorf("%w：作用域 %d 路径前缀不合法", ErrCredentialScopeDenied, i)
		}
		if len(sc.Actions) == 0 || len(sc.Actions) > 2 {
			return issue, fmt.Errorf("%w：作用域 %d 动作集合不合法", ErrCredentialScopeDenied, i)
		}
		seen := map[string]bool{}
		for _, a := range sc.Actions {
			if seen[a] || (in.Purpose == auth.TokenPurposeArtifactRead && a != auth.MachineActionRead) ||
				(in.Purpose == auth.TokenPurposeArtifactPublish && a != auth.MachineActionRead && a != auth.MachineActionPublish) {
				return issue, fmt.Errorf("%w：作用域 %d 动作 %q 超出用途", ErrCredentialScopeDenied, i, a)
			}
			seen[a] = true
		}
		repo, err := s.repos.Get(sc.Repository)
		if err != nil {
			return issue, fmt.Errorf("%w：作用域 %d 仓库不存在", ErrCredentialScopeDenied, i)
		}
		for _, a := range sc.Actions {
			// 颁发时校验欲发动作属于签发者可授实际权限；权限后续收窄即时生效。
			grantable := p.IsAdmin()
			if !grantable {
				grantable, err = s.repos.CanAccessResolved(repo, subject, a)
				if err != nil {
					return issue, err
				}
			}
			if !grantable {
				return issue, fmt.Errorf("%w：作用域 %d 动作 %q 超出签发者权限", ErrCredentialScopeDenied, i, a)
			}
		}
		scopes = append(scopes, auth.MachineTokenScope{RepositoryID: repo.ID, PathPrefix: prefix, Actions: append([]string{}, sc.Actions...)})
	}
	raw, err := json.Marshal(scopes)
	if err != nil {
		return issue, err
	}
	issue.ScopesJSON = string(raw)
	issue.ExpiresAt = time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	return issue, nil
}

// canonicalScopePrefix 校验作用域路径前缀是规范相对路径：无盘符、无 ..、
// 无反斜杠、无百分号编码、无控制字符；仓库根用空前缀表示。
func canonicalScopePrefix(raw string) (string, error) {
	s := strings.TrimLeft(raw, "/")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return "", nil
	}
	if strings.ContainsAny(s, "\\%") || !utf8.ValidString(s) {
		return "", errors.New("路径前缀含危险编码")
	}
	for _, ch := range s {
		if unicode.IsControl(ch) {
			return "", errors.New("路径前缀含控制字符")
		}
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New("路径前缀含非常规范段")
		}
	}
	return s, nil
}

func (s *TokenService) createCredential(ctx context.Context, userID int64, issue repository.CredentialTokenIssue, proof *auth.CredentialProof) (string, *repository.Token, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return "", nil, err
	}
	plain, digest := auth.GenerateToken()
	issue.Digest = digest
	id, err := s.users.IssueCredentialToken(ctx, userID, issue, proof)
	if errors.Is(err, repository.ErrCredentialRejected) {
		return "", nil, ErrCredentialVersionConflict
	}
	if err != nil {
		return "", nil, ErrSecurityAuditUnavailable
	}
	u, err := s.users.GetByID(userID)
	if err == nil {
		s.recordChange(EntityToken, TokenKey(digest), OpPut, TokenChangeData{
			Hash: digest, Username: u.Username, Name: issue.Name,
		})
	}
	return plain, &repository.Token{ID: id, UserID: userID, Name: issue.Name, Purpose: issue.Purpose, ScopesJSON: issue.ScopesJSON, ExpiresAt: &issue.ExpiresAt}, nil
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
