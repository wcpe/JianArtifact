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

// Count 返回用户总数（不含内置 anonymous，供初始化判断与状态统计）。
func (s *UserService) Count() (int, error) { return s.users.CountExcluding(AnonymousUsername) }

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

// Update 更新角色 / 状态（空串不改）。内置 anonymous 用户不可修改。
func (s *UserService) Update(id int64, role, status string) (*repository.User, error) {
	if err := s.rejectAnonymous(id); err != nil {
		return nil, err
	}
	if err := s.users.Update(id, role, status); err != nil {
		return nil, mapNotFound(err)
	}
	return s.users.GetByID(id)
}

// Delete 删除用户。内置 anonymous 用户不可删除。
func (s *UserService) Delete(id int64) error {
	if err := s.rejectAnonymous(id); err != nil {
		return err
	}
	return mapNotFound(s.users.Delete(id))
}

// ChangePassword 重置用户口令。内置 anonymous 用户不可改密。
func (s *UserService) ChangePassword(id int64, password string) error {
	if err := s.rejectAnonymous(id); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return mapNotFound(s.users.UpdatePassword(id, hash))
}

// rejectAnonymous 对内置 anonymous 用户的管理操作返回 ErrValidation（FR-66）。
func (s *UserService) rejectAnonymous(id int64) error {
	u, err := s.users.GetByID(id)
	if err != nil {
		return mapNotFound(err)
	}
	if u.Username == AnonymousUsername {
		return ErrValidation
	}
	return nil
}

// mapNotFound 把 repository.ErrNotFound 转为 domain.ErrNotFound。
func mapNotFound(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
