package domain

import (
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// RepositoryService 处理仓库管理、ACL 与授权判定。
type RepositoryService struct {
	repos *repository.RepoRepo
	acls  *repository.AclRepo
}

// NewRepositoryService 构造 RepositoryService。
func NewRepositoryService(repos *repository.RepoRepo, acls *repository.AclRepo) *RepositoryService {
	return &RepositoryService{repos: repos, acls: acls}
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

// Create 创建仓库（默认可见性 private）。
func (s *RepositoryService) Create(name, format, typ, visibility string) (*repository.Repository, error) {
	if visibility == "" {
		visibility = "private"
	}
	id, err := s.repos.Create(name, format, typ, visibility)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	_ = id
	return s.repos.GetByName(name)
}

// Update 更新仓库可见性。
func (s *RepositoryService) Update(name, visibility string) (*repository.Repository, error) {
	if err := s.repos.UpdateVisibility(name, visibility); err != nil {
		return nil, mapNotFound(err)
	}
	return s.repos.GetByName(name)
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
