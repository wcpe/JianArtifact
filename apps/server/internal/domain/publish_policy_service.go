package domain

import (
	"errors"
	"fmt"
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

// PublishPolicyBatchError 表示批量保存的**预校验**失败，Repository 指出问题仓库。
// 它包装用于状态码映射的哨兵错误（ErrNotFound / ErrValidation），而 Error() 文案会点名
// 仓库——批量保存「整体拒绝」时必须让调用方知道是哪一个仓库不合法。
type PublishPolicyBatchError struct {
	Repository string
	Reason     string
	Err        error
}

// Error 返回面向调用方的说明，始终包含仓库名。
func (e *PublishPolicyBatchError) Error() string {
	return fmt.Sprintf("仓库 %s：%s", e.Repository, e.Reason)
}

// Unwrap 暴露哨兵错误，供调用方用 errors.Is 映射 HTTP 状态码。
func (e *PublishPolicyBatchError) Unwrap() error { return e.Err }

// PublishPolicyPatch 是对既有发布策略的局部覆盖；nil 字段表示**保留目标仓库的现有值**。
// 批量保存对每个仓库分别与现有策略合并，因此「省略字段」不会被误当成「清空」——
// 清空允许前缀等于放开全部路径，静默放宽发布范围是危险的。
type PublishPolicyPatch struct {
	// PathPrefixes 非 nil（含空切片）时覆盖路径前缀；nil 保留现有值。
	PathPrefixes  *[]string
	MaxAssetsHour *int64
	MaxBytesDay   *int64
	MaxFileBytes  *int64
}

// Apply 把补丁合并到现有策略上，返回可交给 Save 的策略。空前缀归一为非 nil 空切片，
// 保证契约的 required 数组字段不会序列化成 null（与仓储层的归一策略一致）。
func (p PublishPolicyPatch) Apply(current repository.PublishPolicy, userID int64) repository.PublishPolicy {
	out := current
	out.UserID = userID
	if out.PathPrefixes == nil {
		out.PathPrefixes = []string{}
	}
	if p.PathPrefixes != nil {
		out.PathPrefixes = *p.PathPrefixes
	}
	if p.MaxAssetsHour != nil {
		out.MaxAssetsHour = *p.MaxAssetsHour
	}
	if p.MaxBytesDay != nil {
		out.MaxBytesDay = *p.MaxBytesDay
	}
	if p.MaxFileBytes != nil {
		out.MaxFileBytes = *p.MaxFileBytes
	}
	return out
}

// PublishPolicySaveResult 是单仓库的批量保存结果；Error 非空表示该仓库失败。
type PublishPolicySaveResult struct {
	Repository string
	OK         bool
	Error      string
}

// SaveMany 把同一份策略补丁批量应用到多个 Hosted 仓库。
//
// 原子性做法与取舍：**先对全部仓库做统一预校验**（存在且为 hosted），任一不合法即整体拒绝
// 并点明问题仓库，从而排除「前几个成功、后面报错」的半成品；预校验通过后再逐仓库与现有
// 策略合并落库（复用 Save 的校验规则，不复制一份），并按仓库逐条报告结果。
// 没有引入跨仓库事务：预校验已消除全部确定性失败，此后残留的只可能是单仓库的存储层错误
// （锁/磁盘），把它作为该仓库的失败结果上报比整体回滚更可诊断；而给 PublishPolicyRepo
// 增加 UpsertTx 需要复制一份 upsert SQL（或改造 Upsert 接收可复用 querier），
// 为这类罕见失败引入规则漂移的成本并不划算。
func (s *PublishPolicyService) SaveMany(userID int64, patch PublishPolicyPatch, repoNames []string) ([]PublishPolicySaveResult, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	names := normalizeRepoNames(repoNames)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w：至少指定一个仓库", ErrValidation)
	}
	// 统一预校验：任一仓库不存在或不是 hosted 都整体拒绝，且错误必须点名该仓库。
	repoIDs := make([]int64, len(names))
	for i, name := range names {
		repo, err := s.repos.GetByName(name)
		if err != nil {
			return nil, &PublishPolicyBatchError{Repository: name, Reason: "仓库不存在", Err: mapNotFound(err)}
		}
		if repo.Type != "hosted" {
			return nil, &PublishPolicyBatchError{
				Repository: name,
				Reason:     "不是 Hosted 仓库，无法配置发布策略",
				Err:        ErrValidation,
			}
		}
		repoIDs[i] = repo.ID
	}
	results := make([]PublishPolicySaveResult, 0, len(names))
	for i, name := range names {
		current := repository.PublishPolicy{UserID: userID, RepositoryID: repoIDs[i], PathPrefixes: []string{}}
		if existing, err := s.policies.Get(userID, repoIDs[i]); err == nil {
			current = *existing
		} else if !errors.Is(err, repository.ErrNotFound) {
			results = append(results, PublishPolicySaveResult{Repository: name, Error: err.Error()})
			continue
		}
		if _, err := s.Save(patch.Apply(current, userID), name); err != nil {
			results = append(results, PublishPolicySaveResult{Repository: name, Error: err.Error()})
			continue
		}
		results = append(results, PublishPolicySaveResult{Repository: name, OK: true})
	}
	return results, nil
}

// normalizeRepoNames 去掉空白与重复仓库名，保留首次出现的顺序；去重是为了避免同一仓库被
// 重复写入（结果列表也会出现重复行）。
func normalizeRepoNames(repoNames []string) []string {
	out := make([]string, 0, len(repoNames))
	seen := make(map[string]struct{}, len(repoNames))
	for _, name := range repoNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
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
