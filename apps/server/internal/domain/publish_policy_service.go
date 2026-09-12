package domain

import (
	"errors"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 发布策略校验错误，供协议和管理 API 映射稳定错误码。
var (
	ErrPublishPathDenied = errors.New("发布路径不在允许前缀内")
	ErrImmutableRelease  = errors.New("不可变 Release 不允许覆盖")
	ErrQuotaExceeded     = repository.ErrQuotaExceeded
)

// PublishPolicyService 处理发布账号的仓库策略、不可变 Release 与持久化额度。
type PublishPolicyService struct {
	policies  *repository.PublishPolicyRepo
	repos     *repository.RepoRepo
	assets    *repository.AssetRepo
	writeGate BusinessWriteGate
}

// NewPublishPolicyService 构造发布策略服务。
func NewPublishPolicyService(policies *repository.PublishPolicyRepo, repos *repository.RepoRepo, assets *repository.AssetRepo) *PublishPolicyService {
	return &PublishPolicyService{policies: policies, repos: repos, assets: assets}
}

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *PublishPolicyService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// Get 返回用户×仓库策略。
func (s *PublishPolicyService) Get(userID int64, repoName string) (*repository.PublishPolicy, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	p, err := s.policies.Get(userID, repo.ID)
	if errors.Is(err, repository.ErrNotFound) {
		return &repository.PublishPolicy{UserID: userID, RepositoryID: repo.ID, PathPrefixes: []string{}}, nil
	}
	return p, err
}

// Save 校验并保存用户×仓库策略。
func (s *PublishPolicyService) Save(p repository.PublishPolicy, repoName string) (*repository.PublishPolicy, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" || p.UserID <= 0 || p.MaxAssetsHour < 0 || p.MaxBytesDay < 0 || p.MaxFileBytes < 0 {
		return nil, ErrValidation
	}
	p.RepositoryID = repo.ID
	for i, prefix := range p.PathPrefixes {
		prefix = strings.Trim(prefix, "/")
		if prefix == "" || strings.Contains(prefix, "..") {
			return nil, ErrValidation
		}
		p.PathPrefixes[i] = prefix
	}
	if err := s.policies.Upsert(p); err != nil {
		return nil, err
	}
	return s.Get(p.UserID, repoName)
}

// Begin 校验一次协议发布并创建持久化额度预留。管理员不受用户策略限制，
// 但仍由仓库 hosted 与不可变策略约束；返回 0 表示没有配置额度。
func (s *PublishPolicyService) Begin(userID int64, repoName, path string, size int64) (int64, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return 0, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return 0, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return 0, ErrConflict
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return 0, err
	}
	if cfg.ImmutableRelease {
		if _, err := s.assets.GetByPath(repo.ID, path); err == nil {
			return 0, ErrImmutableRelease
		}
	}
	p, err := s.policies.Get(userID, repo.ID)
	if errors.Is(err, repository.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(p.PathPrefixes) > 0 && !matchesPrefix(path, p.PathPrefixes) {
		return 0, ErrPublishPathDenied
	}
	if p.MaxFileBytes > 0 && size > p.MaxFileBytes {
		return 0, ErrQuotaExceeded
	}
	if p.MaxAssetsHour == 0 && p.MaxBytesDay == 0 {
		return 0, nil
	}
	return s.policies.Reserve(*p, 1, size)
}

// BeginStreaming 为未知 Content-Length 的协议上传建立预留，并返回可读入的最大字节数。
// 返回 0 表示没有字节上限；预留仍按一件制品计数，避免流式上传绕过件数额度。
func (s *PublishPolicyService) BeginStreaming(userID int64, repoName, path string) (int64, int64, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return 0, 0, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return 0, 0, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return 0, 0, ErrConflict
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return 0, 0, err
	}
	if cfg.ImmutableRelease {
		if _, err := s.assets.GetByPath(repo.ID, path); err == nil {
			return 0, 0, ErrImmutableRelease
		}
	}
	p, err := s.policies.Get(userID, repo.ID)
	if errors.Is(err, repository.ErrNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if len(p.PathPrefixes) > 0 && !matchesPrefix(path, p.PathPrefixes) {
		return 0, 0, ErrPublishPathDenied
	}
	limit := p.MaxFileBytes
	if p.MaxBytesDay > 0 {
		remaining, err := s.policies.RemainingBytes(*p)
		if err != nil {
			return 0, 0, err
		}
		if limit == 0 || remaining < limit {
			limit = remaining
		}
	}
	if p.MaxAssetsHour == 0 && p.MaxBytesDay == 0 && p.MaxFileBytes == 0 {
		return 0, 0, nil
	}
	id, err := s.policies.Reserve(*p, 1, limit)
	return id, limit, err
}

// BeginUnresolvedStreaming 在协议尚未解析出制品路径时预留上传额度，避免先落大临时文件
// 再被额度拒绝。调用方在取得路径后必须调用 ValidateUnresolvedPublish，并以 SettleSize 结算。
func (s *PublishPolicyService) BeginUnresolvedStreaming(userID int64, repoName string) (int64, int64, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return 0, 0, err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return 0, 0, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return 0, 0, ErrConflict
	}
	p, err := s.policies.Get(userID, repo.ID)
	if errors.Is(err, repository.ErrNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	limit := p.MaxFileBytes
	if p.MaxBytesDay > 0 {
		remaining, remainingErr := s.policies.RemainingBytes(*p)
		if remainingErr != nil {
			return 0, 0, remainingErr
		}
		if limit == 0 || remaining < limit {
			limit = remaining
		}
	}
	if p.MaxAssetsHour == 0 && p.MaxBytesDay == 0 && p.MaxFileBytes == 0 {
		return 0, 0, nil
	}
	id, reserveErr := s.policies.Reserve(*p, 1, limit)
	return id, limit, reserveErr
}

// ValidateUnresolvedPublish 在制品路径解析完成后补齐路径和不可变发布校验，不重复预留额度。
func (s *PublishPolicyService) ValidateUnresolvedPublish(userID int64, repoName, path string) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return ErrConflict
	}
	cfg, err := repo.DecodeConfig()
	if err != nil {
		return err
	}
	if cfg.ImmutableRelease {
		if _, err := s.assets.GetByPath(repo.ID, path); err == nil {
			return ErrImmutableRelease
		}
	}
	p, err := s.policies.Get(userID, repo.ID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(p.PathPrefixes) > 0 && !matchesPrefix(path, p.PathPrefixes) {
		return ErrPublishPathDenied
	}
	return nil
}

// Settle 完成或释放一次发布预留。
func (s *PublishPolicyService) Settle(id int64, success bool) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if id == 0 {
		return nil
	}
	return s.policies.Settle(id, success)
}

// SettleSize 结算未知长度上传，成功时按实际大小计入字节额度。
func (s *PublishPolicyService) SettleSize(id int64, success bool, size int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if id == 0 {
		return nil
	}
	return s.policies.SettleBytes(id, success, size)
}

func matchesPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/") {
			return true
		}
	}
	return false
}
