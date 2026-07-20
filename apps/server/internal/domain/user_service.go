package domain

import (
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// UserService 处理用户管理与口令。
type UserService struct {
	users *repository.UserRepo
}

// NewUserService 构造 UserService。
func NewUserService(users *repository.UserRepo) *UserService {
	return &UserService{users: users}
}

// Count 返回用户总数。
func (s *UserService) Count() (int, error) { return s.users.Count() }

// List 返回分页用户与总数。
func (s *UserService) List(limit, offset int) ([]repository.User, int, error) {
	total, err := s.users.Count()
	if err != nil {
		return nil, 0, err
	}
	items, err := s.users.List(limit, offset)
	return items, total, err
}

// Create 创建用户（默认角色 user）。role 为空则取 user。
func (s *UserService) Create(username, password, role string) (*repository.User, error) {
	if role == "" {
		role = "user"
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	id, err := s.users.Create(username, hash, role)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.users.GetByID(id)
}

// Update 更新角色 / 状态（空串不改）。
func (s *UserService) Update(id int64, role, status string) (*repository.User, error) {
	if err := s.users.Update(id, role, status); err != nil {
		return nil, mapNotFound(err)
	}
	return s.users.GetByID(id)
}

// Delete 删除用户。
func (s *UserService) Delete(id int64) error {
	return mapNotFound(s.users.Delete(id))
}

// ChangePassword 重置用户口令。
func (s *UserService) ChangePassword(id int64, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return mapNotFound(s.users.UpdatePassword(id, hash))
}

// mapNotFound 把 repository.ErrNotFound 转为 domain.ErrNotFound。
func mapNotFound(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
