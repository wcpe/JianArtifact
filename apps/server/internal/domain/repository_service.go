package domain

import (
	"errors"
	"net/url"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// RepositoryService 处理仓库管理、ACL 与授权判定。
type RepositoryService struct {
	repos  *repository.RepoRepo
	acls   *repository.AclRepo
	assets *repository.AssetRepo
}

// NewRepositoryService 构造 RepositoryService。
func NewRepositoryService(repos *repository.RepoRepo, acls *repository.AclRepo, assets *repository.AssetRepo) *RepositoryService {
	return &RepositoryService{repos: repos, acls: acls, assets: assets}
}

// List 返回分页仓库与总数。
func (s *RepositoryService) List(limit, offset int) ([]repository.Repository, int, error) {
	total, err := s.repos.Count()
	if err != nil {
		return nil, 0, err
	}
	items, err := s.repos.List(limit, offset)
	return items, total, err
}

// Get 按名取仓库。
func (s *RepositoryService) Get(name string) (*repository.Repository, error) {
	r, err := s.repos.GetByName(name)
	return r, mapNotFound(err)
}

// ListAssets 返回仓库内制品的分页列表与总数；prefix 非空时按路径前缀过滤。
// 仓库不存在返回 ErrNotFound。
func (s *RepositoryService) ListAssets(name, prefix string, limit, offset int) ([]repository.Asset, int, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, 0, mapNotFound(err)
	}
	total, err := s.assets.CountByRepo(repo.ID, prefix)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.assets.ListByRepo(repo.ID, prefix, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Usage 返回仓库及其客户端接入片段（据 format/type 与对外基址组装）。
// 仓库不存在返回 ErrNotFound。
func (s *RepositoryService) Usage(name, baseURL string) (*repository.Repository, []UsageSnippet, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	return repo, buildUsage(repo, baseURL), nil
}

// Create 创建仓库（默认可见性 private）。cfg 为结构化配置：
// proxy 必填合法 remoteUrl；group 必填 members（均存在且同 format、禁止自引用）。
// 校验不过返回 ErrValidation；仓库名重复返回 ErrConflict。
func (s *RepositoryService) Create(name, format, typ, visibility string, cfg repository.RepositoryConfig) (*repository.Repository, error) {
	if visibility == "" {
		visibility = "private"
	}
	if err := s.validateConfig(name, format, typ, cfg); err != nil {
		return nil, err
	}
	configJSON, err := repository.EncodeRepositoryConfig(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := s.repos.Create(name, format, typ, visibility, configJSON); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.repos.GetByName(name)
}

// Update 更新仓库可见性与/或结构化配置。visibility 为空表示不改；cfg 非 nil 时
// 按仓库当前 format/type 重新校验并覆盖写 config。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) Update(name, visibility string, cfg *repository.RepositoryConfig) (*repository.Repository, error) {
	repo, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if cfg != nil {
		if err := s.validateConfig(name, repo.Format, repo.Type, *cfg); err != nil {
			return nil, err
		}
		configJSON, err := repository.EncodeRepositoryConfig(*cfg)
		if err != nil {
			return nil, err
		}
		if err := s.repos.UpdateConfig(name, configJSON); err != nil {
			return nil, mapNotFound(err)
		}
	}
	if visibility != "" {
		if err := s.repos.UpdateVisibility(name, visibility); err != nil {
			return nil, mapNotFound(err)
		}
	}
	return s.repos.GetByName(name)
}

// validateConfig 按仓库类型校验结构化配置：
//   - hosted：remoteUrl 与 members 均须为空；
//   - proxy：remoteUrl 必填且为合法 http/https 绝对地址，members 须为空；
//   - group：members 必填（≥1），每个成员须存在、与本仓 format 一致且非自引用，remoteUrl 须为空。
//
// 违规返回 ErrValidation。
func (s *RepositoryService) validateConfig(name, format, typ string, cfg repository.RepositoryConfig) error {
	switch typ {
	case "hosted":
		if cfg.RemoteURL != "" || len(cfg.Members) > 0 {
			return ErrValidation
		}
	case "proxy":
		if len(cfg.Members) > 0 || cfg.RemoteURL == "" {
			return ErrValidation
		}
		u, err := url.Parse(cfg.RemoteURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return ErrValidation
		}
	case "group":
		if cfg.RemoteURL != "" || len(cfg.Members) == 0 {
			return ErrValidation
		}
		for _, m := range cfg.Members {
			if m == "" || m == name {
				return ErrValidation
			}
			member, err := s.repos.GetByName(m)
			if err != nil {
				return ErrValidation
			}
			if member.Format != format {
				return ErrValidation
			}
		}
	default:
		// 未知类型交由持久化层的 CHECK 约束拒绝（返回后映射为内部错误）。
	}
	return nil
}

// Delete 删除仓库。
func (s *RepositoryService) Delete(name string) error {
	return mapNotFound(s.repos.Delete(name))
}

// GetAcl 返回仓库 ACL；仓库不存在返回 ErrNotFound。
func (s *RepositoryService) GetAcl(name string) ([]repository.Acl, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return s.acls.ListByRepo(r.ID)
}

// SetAcl 覆盖写入仓库 ACL；仓库不存在返回 ErrNotFound。
func (s *RepositoryService) SetAcl(name string, entries []repository.Acl) ([]repository.Acl, error) {
	r, err := s.repos.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if err := s.acls.Replace(r.ID, entries); err != nil {
		return nil, err
	}
	return s.acls.ListByRepo(r.ID)
}

// CanAccess 判定主体对仓库是否可执行动作（read/write/admin）。
// public 仓库对 read 放行；其余按 ACL 判定。仓库不存在返回 ErrNotFound。
func (s *RepositoryService) CanAccess(name string, subjectID int64, action string) (bool, error) {
	r, err := s.repos.GetByName(name)
	if errors.Is(err, repository.ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if action == "read" && r.Visibility == "public" {
		return true, nil
	}
	return s.acls.HasPermission(r.ID, subjectID, action)
}
