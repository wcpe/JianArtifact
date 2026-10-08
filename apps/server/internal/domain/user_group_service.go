package domain

import (
	"fmt"
	"log"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// UserGroupService 处理用户组管理（FR-36）：组的增删改查与成员维护。
//
// 组本身只是「名字 + 描述 + 成员集合」的授权载体：真正产生权限的是把组作为主体
// 写进某个仓库的 ACL。因此本服务不判定权限（Permission 判定在 RepositoryService），
// 只负责维护组与成员，并保证删除组时不留下悬空授权。
type UserGroupService struct {
	groups    *repository.UserGroupRepo
	acls      *repository.AclRepo
	users     *repository.UserRepo
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
}

// NewUserGroupService 构造 UserGroupService。
// acls 供删除组时清理以该组为主体的 ACL 条目（nil 时跳过清理）。
// users 供加入成员时校验用户存在（nil 时跳过校验）。
func NewUserGroupService(groups *repository.UserGroupRepo, acls *repository.AclRepo, users *repository.UserRepo) *UserGroupService {
	return &UserGroupService{groups: groups, acls: acls, users: users}
}

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
func (s *UserGroupService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *UserGroupService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// recordChange 记录复制变更日志；记录失败不阻断业务写（对账兜底，见 ADR-0013）。
func (s *UserGroupService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
}

// Create 新建用户组。组名必填且不可为空白；重名返回 ErrConflict。
func (s *UserGroupService) Create(name, description string) (*repository.UserGroup, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w：组名不能为空", ErrValidation)
	}
	id, err := s.groups.Create(name, strings.TrimSpace(description))
	if err != nil {
		// 组名唯一由 DB UNIQUE 兜底，此处转成领域层的冲突语义（映射 409）。
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	g, err := s.groups.GetByID(id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	s.recordChange(EntityUserGroup, UserGroupKey(g.Name), OpPut, UserGroupChangeData{
		Name: g.Name, Description: g.Description, Members: nil, CreatedAt: g.CreatedAt,
	})
	return g, nil
}

// List 返回全部分组（按名称升序）。
func (s *UserGroupService) List() ([]repository.UserGroup, error) { return s.groups.List() }

// Get 按 ID 取组；不存在返回 ErrNotFound。
func (s *UserGroupService) Get(id int64) (*repository.UserGroup, error) {
	g, err := s.groups.GetByID(id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return g, nil
}

// GetByName 按组名取组；不存在返回 ErrNotFound。
func (s *UserGroupService) GetByName(name string) (*repository.UserGroup, error) {
	g, err := s.groups.GetByName(name)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return g, nil
}

// Update 更新组名与描述（空串表示不改）。重名返回 ErrConflict。
func (s *UserGroupService) Update(id int64, name, description string) (*repository.UserGroup, error) {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if err := s.groups.Update(id, name, strings.TrimSpace(description)); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, mapNotFound(err)
	}
	g, err := s.groups.GetByID(id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	s.recordChange(EntityUserGroup, UserGroupKey(g.Name), OpPut, UserGroupChangeData{
		Name: g.Name, Description: g.Description, Members: s.memberNames(id), CreatedAt: g.CreatedAt,
	})
	return g, nil
}

// Delete 删除用户组，并清理以该组为主体的 ACL 条目。
//
// 成员关系由外键 ON DELETE CASCADE 清理（不写代码）；ACL 条目也带同样的 CASCADE，
// 但显式再清一次是为了让「清理」这件事在领域层可观测、可测试——外键默认关闭的部署
// 下同样不留悬空授权。清理失败即返回错误：宁可删不掉组，也不留下指向已消失组的授权。
func (s *UserGroupService) Delete(id int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	g, err := s.groups.GetByID(id)
	if err != nil {
		return mapNotFound(err)
	}
	if s.acls != nil {
		if err := s.acls.DeleteByGroup(id); err != nil {
			return err
		}
	}
	if err := s.groups.Delete(id); err != nil {
		return mapNotFound(err)
	}
	s.recordChange(EntityUserGroup, UserGroupKey(g.Name), OpDelete, TombstoneData{Deleted: true})
	return nil
}

// AddMember 把用户加入组。组或用户不存在返回 ErrNotFound；重复加入是幂等的（不报错）。
func (s *UserGroupService) AddMember(groupID, userID int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if err := s.requireGroup(groupID); err != nil {
		return err
	}
	if err := s.requireUser(userID); err != nil {
		return err
	}
	if err := s.groups.AddMember(groupID, userID); err != nil {
		return err
	}
	s.recordMembershipChange(groupID)
	return nil
}

// RemoveMember 把用户移出组。关系不存在返回 ErrNotFound。
func (s *UserGroupService) RemoveMember(groupID, userID int64) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	if err := s.groups.RemoveMember(groupID, userID); err != nil {
		return mapNotFound(err)
	}
	s.recordMembershipChange(groupID)
	return nil
}

// ListMembers 列出组的全部成员用户（按用户名升序）。
func (s *UserGroupService) ListMembers(groupID int64) ([]repository.User, error) {
	return s.groups.ListMembers(groupID)
}

// ListMemberRows 列出组成员（按用户名升序），带加入时间；供管理面成员列表展示。
// 组不存在时返回 ErrNotFound（与 Get 同口径，避免把「组不存在」误报成空列表）。
func (s *UserGroupService) ListMemberRows(groupID int64) ([]repository.MemberRow, error) {
	if err := s.requireGroup(groupID); err != nil {
		return nil, err
	}
	return s.groups.ListMemberRows(groupID)
}

// MemberJoinedAt 取某用户加入某组的时间；不存在返回 ErrNotFound。
// 供「加入成员」端点回填 createdAt，避免为一条记录拉全量成员列表。
func (s *UserGroupService) MemberJoinedAt(groupID, userID int64) (string, error) {
	return s.groups.MemberJoinedAt(groupID, userID)
}

// ListGroupsOfUser 列出某用户所属的全部组 ID（升序）；供鉴权展开主体。
func (s *UserGroupService) ListGroupsOfUser(userID int64) ([]int64, error) {
	return s.groups.ListGroupsOfUser(userID)
}

// requireGroup 校验组存在。
func (s *UserGroupService) requireGroup(groupID int64) error {
	if _, err := s.groups.GetByID(groupID); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// requireUser 校验用户存在（users 未注入时跳过）。
func (s *UserGroupService) requireUser(userID int64) error {
	if s.users == nil {
		return nil
	}
	if _, err := s.users.GetByID(userID); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// memberNames 取组成员的用户名快照（复制变更日志用）；取不到时返回空。
func (s *UserGroupService) memberNames(groupID int64) []string {
	members, err := s.groups.ListMembers(groupID)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Username)
	}
	return names
}

// recordMembershipChange 以组为单位记录成员快照变更（组成员是整体快照，不做增量）。
func (s *UserGroupService) recordMembershipChange(groupID int64) {
	g, err := s.groups.GetByID(groupID)
	if err != nil {
		return
	}
	s.recordChange(EntityUserGroup, UserGroupKey(g.Name), OpPut, UserGroupChangeData{
		Name: g.Name, Description: g.Description, Members: s.memberNames(groupID), CreatedAt: g.CreatedAt,
	})
}
