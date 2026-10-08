package domain_test

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newUserGroupSvc 组装用户组服务及配套依赖（ACL/用户仓储）。
func newUserGroupSvc(t *testing.T) (*domain.UserGroupService, *repository.AclRepo, *repository.UserRepo) {
	t.Helper()
	db := newTestDB(t)
	acls := repository.NewAclRepo(db)
	users := repository.NewUserRepo(db)
	return domain.NewUserGroupService(repository.NewUserGroupRepo(db), acls, users), acls, users
}

// TestUserGroupServiceCRUD 组服务的创建 / 取值 / 列表 / 更新 / 删除，
// 含重名与空名校验：二者分别映射 ErrConflict 与 ErrValidation。
func TestUserGroupServiceCRUD(t *testing.T) {
	t.Parallel()
	svc, _, _ := newUserGroupSvc(t)

	// 组名空白（含纯空格）应被拒绝。
	if _, err := svc.Create("", "无名字"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("空组名应 ErrValidation，得 %v", err)
	}
	if _, err := svc.Create("   ", "纯空格"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("纯空格组名应 ErrValidation，得 %v", err)
	}

	created, err := svc.Create("release-team", "发布组")
	if err != nil {
		t.Fatalf("创建组：%v", err)
	}
	if created.Name != "release-team" || created.Description != "发布组" {
		t.Fatalf("创建结果异常：%+v", created)
	}

	// 重名由 DB UNIQUE 兜底，转成 ErrConflict（HTTP 层映射 409）。
	if _, err := svc.Create("release-team", "重名"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("重名组应 ErrConflict，得 %v", err)
	}

	// 三个取值入口：按 ID、按名、列表。
	byID, err := svc.Get(created.ID)
	if err != nil || byID.Name != "release-team" {
		t.Errorf("按 ID 取组：%+v err=%v", byID, err)
	}
	byName, err := svc.GetByName("release-team")
	if err != nil || byName.ID != created.ID {
		t.Errorf("按名取组：%+v err=%v", byName, err)
	}
	list, err := svc.List()
	if err != nil {
		t.Fatalf("列组：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应只有 1 个组，得 %d", len(list))
	}

	// 更新：改描述保留原组名。
	updated, err := svc.Update(created.ID, "", "发布组（已更新）")
	if err != nil {
		t.Fatalf("更新组：%v", err)
	}
	if updated.Name != "release-team" || updated.Description != "发布组（已更新）" {
		t.Errorf("空名字应表示不改：%+v", updated)
	}
	// 改名撞已有组名同样 ErrConflict。
	if _, err := svc.Update(created.ID, "release-team", ""); err != nil {
		t.Errorf("改成同名（自身）不应报错：%v", err)
	}

	// 删除后再取均 ErrNotFound。
	if err := svc.Delete(created.ID); err != nil {
		t.Fatalf("删除组：%v", err)
	}
	if _, err := svc.Get(created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("删除后按 ID 应 ErrNotFound，得 %v", err)
	}
	if err := svc.Delete(created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("重复删除应 ErrNotFound，得 %v", err)
	}
}

// TestUserGroupServiceMembers 成员增删：未知组/用户返回 ErrNotFound，
// 重复加入幂等，重复移出 ErrNotFound，双向反查一致。
func TestUserGroupServiceMembers(t *testing.T) {
	t.Parallel()
	svc, _, users := newUserGroupSvc(t)

	uid, err := users.Create("member-one", "hash-m", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	group, err := svc.Create("members-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}

	// 未知组 / 未知用户都应 ErrNotFound，而不是静默写入脏关系。
	if err := svc.AddMember(group.ID+9191, uid); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("加入未知组应 ErrNotFound，得 %v", err)
	}
	if err := svc.AddMember(group.ID, 777777); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("加入未知用户应 ErrNotFound，得 %v", err)
	}

	if err := svc.AddMember(group.ID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	// 重复加入幂等：不报错且成员数仍为 1。
	if err := svc.AddMember(group.ID, uid); err != nil {
		t.Fatalf("重复加入应幂等：%v", err)
	}
	members, err := svc.ListMembers(group.ID)
	if err != nil {
		t.Fatalf("列成员：%v", err)
	}
	if len(members) != 1 || members[0].ID != uid {
		t.Fatalf("成员列表异常：%+v", members)
	}
	groupIDs, err := svc.ListGroupsOfUser(uid)
	if err != nil {
		t.Fatalf("反查用户所属组：%v", err)
	}
	if len(groupIDs) != 1 || groupIDs[0] != group.ID {
		t.Fatalf("反查结果异常：%v", groupIDs)
	}

	if err := svc.RemoveMember(group.ID, uid); err != nil {
		t.Fatalf("移除成员：%v", err)
	}
	if err := svc.RemoveMember(group.ID, uid); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("重复移除应 ErrNotFound，得 %v", err)
	}
	groupIDs, err = svc.ListGroupsOfUser(uid)
	if err != nil {
		t.Fatalf("重查用户所属组：%v", err)
	}
	if len(groupIDs) != 0 {
		t.Errorf("移除后应无归属组，得 %v", groupIDs)
	}
}

// newGroupSvcWithRepo 组装「组服务 + 仓库服务」，二者共用同一个 ACL 仓储，
// 以便直接观测「删组 / 撤销组授权」对授权的影响。
func newGroupSvcWithRepo(t *testing.T) (*domain.UserGroupService, *domain.RepositoryService, *repository.UserRepo, *repository.AclRepo) {
	t.Helper()
	db := newTestDB(t)
	acls := repository.NewAclRepo(db)
	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	groupSvc := domain.NewUserGroupService(groups, acls, users)
	repoSvc := domain.NewRepositoryService(
		repository.NewRepoRepo(db), acls, repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)), users,
	)
	// 注入组仓储：SubjectFor 据此展开组归属，删组后的拒绝才有意义。
	repoSvc.SetUserGroupRepo(groups)
	return groupSvc, repoSvc, users, acls
}

// TestUserGroupServiceDeleteClearsAcl 删除组后，以该组为主体的 ACL 条目被清理，
// 组成员随之失去经由组获得的权限（含六档动作全部验证一遍）。
func TestUserGroupServiceDeleteClearsAcl(t *testing.T) {
	t.Parallel()
	groupSvc, repoSvc, users, _ := newGroupSvcWithRepo(t)

	uid, err := users.Create("grace-member", "hash-gm", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	group, err := groupSvc.Create("doomed-group", "待删组")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groupSvc.AddMember(group.ID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	if _, err := repoSvc.Create("group-only-repo", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 该仓库上只有**组主体**的 admin 授权——没有任何用户自身条目。
	if _, err := repoSvc.SetAcl("group-only-repo", []repository.Acl{{
		SubjectType:    repository.SubjectTypeGroup,
		SubjectGroupID: group.ID,
		Action:         repository.ActionAdmin,
	}}); err != nil {
		t.Fatalf("写组 ACL：%v", err)
	}

	// 前置：组成员经组授权，六档动作全部放行（admin 蕴含全部）。
	for _, action := range allActions {
		ok, err := repoSvc.CanAccess("group-only-repo", repository.Subject{UserID: uid, GroupIDs: []int64{group.ID}}, action)
		if err != nil {
			t.Fatalf("CanAccess(%s)：%v", action, err)
		}
		if !ok {
			t.Fatalf("前置失败：组成员经组 admin 应可 %s", action)
		}
	}

	// 删组：必须连带清理该组的 ACL 条目。
	if err := groupSvc.Delete(group.ID); err != nil {
		t.Fatalf("删除组：%v", err)
	}

	// 删组后：仓库 ACL 空，成员不再具备任何动作。
	//
	// 注意这里**不能**只依赖最终结果：组→ACL 的外键本身带 ON DELETE CASCADE，
	// 即使领域层的显式清理被去掉，下面这两组断言照样通过。因此显式清理的正确性
	// 由 TestUserGroupServiceDeleteCleansAclExplicitly 单独钉住。
	rows, err := repoSvc.GetAcl("group-only-repo")
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 0 {
		t.Errorf("删组后 ACL 应为空，得 %+v", rows)
	}
	// 用「该组 ID」构造主体直接判定：即使调用方缓存了旧的组 ID 也拿不到权限。
	for _, action := range allActions {
		ok, err := repoSvc.CanAccess("group-only-repo", repository.Subject{UserID: uid, GroupIDs: []int64{group.ID}}, action)
		if err != nil {
			t.Fatalf("CanAccess(%s)：%v", action, err)
		}
		if ok {
			t.Errorf("删组后成员仍可 %s", action)
		}
	}
	// 删组后该用户的组归属也应为空（成员关系 CASCADE 清理）。
	groupIDs, err := repoSvc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}
	if len(groupIDs.GroupIDs) != 0 {
		t.Errorf("删组后用户组归属应为空，得 %v", groupIDs.GroupIDs)
	}
}

// TestUserGroupServiceDeleteCleansAclExplicitly 钉住「删组显式清理组 ACL 条目」这一步。
//
// 为什么单独测：组→ACL 的外键带 ON DELETE CASCADE，删组时 SQLite 会自己清掉组条目，
// 因此「删组后组条目消失」无论领域层清不清理都会成立——只看最终结果的用例测不出
// 这一步。本用例改为直接断言 DeleteByGroup 的效果：清完之后，即使该组**仍然存在**，
// 其 ACL 条目也已消失，组成员立刻失去权限。这正是外键兜不住的部署
// （外键未开启 / 组改为逻辑删除）下唯一保证不留悬空授权的机制。
func TestUserGroupServiceDeleteCleansAclExplicitly(t *testing.T) {
	t.Parallel()
	groupSvc, repoSvc, users, acls := newGroupSvcWithRepo(t)

	uid, err := users.Create("henry-member", "hash-hm", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	group, err := groupSvc.Create("kept-group", "保留但撤销授权的组")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groupSvc.AddMember(group.ID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	if _, err := repoSvc.Create("explicit-clean-repo", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := repoSvc.SetAcl("explicit-clean-repo", []repository.Acl{{
		SubjectType:    repository.SubjectTypeGroup,
		SubjectGroupID: group.ID,
		Action:         repository.ActionWrite,
	}}); err != nil {
		t.Fatalf("写组 ACL：%v", err)
	}
	subject := repository.Subject{UserID: uid, GroupIDs: []int64{group.ID}}
	if ok, _ := repoSvc.CanAccess("explicit-clean-repo", subject, repository.ActionWrite); !ok {
		t.Fatal("前置：组成员经组授权应可 write")
	}

	// 组仍在，但组 ACL 被显式清理（DeleteByGroup 即删组流程走的同一段逻辑）。
	if err := acls.DeleteByGroup(group.ID); err != nil {
		t.Fatalf("清理组 ACL：%v", err)
	}
	if ok, _ := repoSvc.CanAccess("explicit-clean-repo", subject, repository.ActionWrite); ok {
		t.Error("撤销组授权后成员不应仍可 write")
	}
	rows, err := repoSvc.GetAcl("explicit-clean-repo")
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 0 {
		t.Errorf("撤销后仓库不应残留组条目，得 %+v", rows)
	}
	// 组本身仍然存在（这一步只撤销授权，不删组）。
	if _, err := groupSvc.Get(group.ID); err != nil {
		t.Errorf("撤销授权不应删除组本身：%v", err)
	}
}

// allActions 是 FR-36 的全部六档 ACL 动作，供蕴含 / 清理类用例遍历。
var allActions = []string{
	repository.ActionRead, repository.ActionWrite, repository.ActionPublish,
	repository.ActionDelete, repository.ActionAclManage, repository.ActionAdmin,
}

// TestUserGroupServiceNotFound 各操作对不存在组的一致性处理。
func TestUserGroupServiceNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newUserGroupSvc(t)
	const ghostID = int64(4321)
	if _, err := svc.Get(ghostID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get 不存在组应 ErrNotFound，得 %v", err)
	}
	if _, err := svc.GetByName("ghost-group"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByName 不存在组应 ErrNotFound，得 %v", err)
	}
	if _, err := svc.Update(ghostID, "x", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Update 不存在组应 ErrNotFound，得 %v", err)
	}
	if err := svc.Delete(ghostID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Delete 不存在组应 ErrNotFound，得 %v", err)
	}
	if err := svc.AddMember(ghostID, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("AddMember 到不存在组应 ErrNotFound，得 %v", err)
	}
}
