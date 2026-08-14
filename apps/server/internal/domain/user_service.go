package domain

import (
	"errors"
	"log"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// UserService 处理用户管理与口令。
type UserService struct {
	users    *repository.UserRepo
	recorder ChangeRecorder
}

// NewUserService 构造 UserService。
func NewUserService(users *repository.UserRepo) *UserService {
	return &UserService{users: users}
}

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
func (s *UserService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// recordChange 记录复制变更日志；记录失败不阻断业务写（对账兜底，见 ADR-0013）。
func (s *UserService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
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
	u, err := s.users.GetByID(id)
	if err != nil {
		return nil, err
	}
	s.recordChange(EntityUser, UserKey(username), OpPut, UserChangeData{
		Username: username, Role: role, Status: u.Status, PasswordHash: hash, CreatedAt: u.CreatedAt,
	})
	return u, nil
}

// Update 更新角色 / 状态（空串不改）。内置 anonymous 用户不可修改。
func (s *UserService) Update(id int64, role, status string) (*repository.User, error) {
	if err := s.rejectAnonymous(id); err != nil {
		return nil, err
	}
	if err := s.users.Update(id, role, status); err != nil {
		return nil, mapNotFound(err)
	}
	u, err := s.users.GetByID(id)
	if err != nil {
		return nil, err
	}
	s.recordChange(EntityUser, UserKey(u.Username), OpPut, UserChangeData{
		Username: u.Username, Role: u.Role, Status: u.Status, PasswordHash: u.PasswordHash, CreatedAt: u.CreatedAt,
	})
	return u, nil
}

// Delete 删除用户。内置 anonymous 用户不可删除。
func (s *UserService) Delete(id int64) error {
	if err := s.rejectAnonymous(id); err != nil {
		return err
	}
	u, err := s.users.GetByID(id)
	if err != nil {
		return mapNotFound(err)
	}
	if err := s.users.Delete(id); err != nil {
		return mapNotFound(err)
	}
	s.recordChange(EntityUser, UserKey(u.Username), OpDelete, TombstoneData{Deleted: true})
	return nil
}

// ChangePassword 重置用户口令。内置 anonymous 用户不可改密。
func (s *UserService) ChangePassword(id int64, password string) error {
	if err := s.rejectAnonymous(id); err != nil {
		return err
	}
	u, err := s.users.GetByID(id)
	if err != nil {
		return mapNotFound(err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.users.UpdatePassword(id, hash); err != nil {
		return mapNotFound(err)
	}
	s.recordChange(EntityUser, UserKey(u.Username), OpPut, UserChangeData{
		Username: u.Username, Role: u.Role, Status: u.Status, PasswordHash: hash, CreatedAt: u.CreatedAt,
	})
	return nil
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
