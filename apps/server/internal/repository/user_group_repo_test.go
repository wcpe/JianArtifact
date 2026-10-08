package repository_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newGroupEnv 打开临时 SQLite 并迁移，返回组仓储 + ACL 仓储 + 仓库仓储 + 用户仓储。
// 组测试依赖四者：ACL 条目需要仓库作外键、需要用户作成员。
func newGroupEnv(t *testing.T) (*repository.UserGroupRepo, *repository.AclRepo, *repository.RepoRepo, *repository.UserRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "user-group.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewUserGroupRepo(db), repository.NewAclRepo(db), repository.NewRepoRepo(db), repository.NewUserRepo(db)
}

// seedGroupRepo 建一个 raw hosted 私有仓库并返回其 ID。
func seedGroupRepo(t *testing.T, repos *repository.RepoRepo, name string) int64 {
	t.Helper()
	id, err := repos.Create(name, "raw", "hosted", "private", "{}")
	if err != nil {
		t.Fatalf("创建仓库 %s：%v", name, err)
	}
	return id
}

// TestUserGroupCRUD 组的创建 / 列表 / 按名查 / 按 ID 查 / 更新 / 删除全流程。
func TestUserGroupCRUD(t *testing.T) {
	groups, _, _, _ := newGroupEnv(t)

	createdID, err := groups.Create("release-team", "发布组")
	if err != nil {
		t.Fatalf("创建组：%v", err)
	}
	if createdID <= 0 {
		t.Fatalf("创建组返回非法 ID：%d", createdID)
	}
	// 回读确认名字与描述都落库（Create 只返回 ID）。
	created, err := groups.GetByID(createdID)
	if err != nil {
		t.Fatalf("回读新组：%v", err)
	}
	if created.Name != "release-team" || created.Description != "发布组" {
		t.Fatalf("创建组内容异常：%+v", created)
	}

	// 按 ID 与按名都能取回。
	byID, err := groups.GetByID(createdID)
	if err != nil || byID.Name != "release-team" {
		t.Fatalf("按 ID 取组：%+v err=%v", byID, err)
	}
	byName, err := groups.GetByName("release-team")
	if err != nil || byName.ID != createdID {
		t.Fatalf("按名取组：%+v err=%v", byName, err)
	}

	// 列表按名升序：再建一个名字更小的组，应排在前。
	if _, err := groups.Create("audit-team", "审计组"); err != nil {
		t.Fatalf("创建第二组：%v", err)
	}
	list, err := groups.List()
	if err != nil {
		t.Fatalf("列组：%v", err)
	}
	if len(list) != 2 || list[0].Name != "audit-team" || list[1].Name != "release-team" {
		t.Fatalf("组列表未按名升序：%+v", list)
	}

	// 更新：只改描述时名字不变。
	if err := groups.Update(createdID, "", "发布组（改名后）"); err != nil {
		t.Fatalf("更新组：%v", err)
	}
	updated, err := groups.GetByID(createdID)
	if err != nil {
		t.Fatalf("取更新后的组：%v", err)
	}
	if updated.Name != "release-team" || updated.Description != "发布组（改名后）" {
		t.Errorf("空串应表示不改名字：%+v", updated)
	}

	// 删除后按 ID 与按名都取不到（ErrNotFound）。
	if err := groups.Delete(createdID); err != nil {
		t.Fatalf("删除组：%v", err)
	}
	if _, err := groups.GetByID(createdID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("删除后按 ID 取组应 ErrNotFound，得 %v", err)
	}
	if _, err := groups.GetByName("release-team"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("删除后按名取组应 ErrNotFound，得 %v", err)
	}
}

// TestUserGroupNameUnique 组名唯一由 DB UNIQUE 约束兜底（仓储层把错误透出，
// 由领域层转成 ErrConflict）。
func TestUserGroupNameUnique(t *testing.T) {
	groups, _, _, _ := newGroupEnv(t)
	if _, err := groups.Create("dup-group", ""); err != nil {
		t.Fatalf("首次创建组：%v", err)
	}
	if _, err := groups.Create("dup-group", "重名"); err == nil {
		t.Fatal("重名组应被唯一约束拒绝")
	}
}

// TestUserGroupNotFound 不存在的组按 ID / 按名 / 更新 / 删除都返回 ErrNotFound。
func TestUserGroupNotFound(t *testing.T) {
	groups, _, _, _ := newGroupEnv(t)
	if _, err := groups.GetByID(4242); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("按 ID 取不存在的组应 ErrNotFound，得 %v", err)
	}
	if _, err := groups.GetByName("ghost-group"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("按名取不存在的组应 ErrNotFound，得 %v", err)
	}
	if err := groups.Update(4242, "x", "y"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("更新不存在的组应 ErrNotFound，得 %v", err)
	}
	if err := groups.Delete(4242); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("删除不存在的组应 ErrNotFound，得 %v", err)
	}
}

// TestUserGroupMembers 成员增删与双向反查：列出组成员、列出某用户所属组。
func TestUserGroupMembers(t *testing.T) {
	groups, _, _, users := newGroupEnv(t)

	alice, err := users.Create("alice", "hash-a", "user")
	if err != nil {
		t.Fatalf("建 alice：%v", err)
	}
	bob, err := users.Create("bob", "hash-b", "user")
	if err != nil {
		t.Fatalf("建 bob：%v", err)
	}
	teamAID, err := groups.Create("team-a", "")
	if err != nil {
		t.Fatalf("建 team-a：%v", err)
	}
	teamBID, err := groups.Create("team-b", "")
	if err != nil {
		t.Fatalf("建 team-b：%v", err)
	}

	if err := groups.AddMember(teamAID, alice); err != nil {
		t.Fatalf("alice 加入 team-a：%v", err)
	}
	if err := groups.AddMember(teamAID, bob); err != nil {
		t.Fatalf("bob 加入 team-a：%v", err)
	}
	if err := groups.AddMember(teamBID, bob); err != nil {
		t.Fatalf("bob 加入 team-b：%v", err)
	}

	// 列出组成员：按用户名升序。
	members, err := groups.ListMembers(teamAID)
	if err != nil {
		t.Fatalf("列 team-a 成员：%v", err)
	}
	if len(members) != 2 || members[0].Username != "alice" || members[1].Username != "bob" {
		t.Fatalf("team-a 成员异常：%+v", members)
	}

	// 反查：bob 属两个组，alice 属一个组，carol 不属任何组。
	bobGroups, err := groups.ListGroupsOfUser(bob)
	if err != nil {
		t.Fatalf("反查 bob 的组：%v", err)
	}
	if len(bobGroups) != 2 || bobGroups[0] != teamAID || bobGroups[1] != teamBID {
		t.Errorf("bob 应属 team-a/team-b（升序），得 %v", bobGroups)
	}
	aliceGroups, err := groups.ListGroupsOfUser(alice)
	if err != nil {
		t.Fatalf("反查 alice 的组：%v", err)
	}
	if len(aliceGroups) != 1 || aliceGroups[0] != teamAID {
		t.Errorf("alice 应只属 team-a，得 %v", aliceGroups)
	}

	// 移出成员：关系消失，另一组不受影响。
	if err := groups.RemoveMember(teamAID, bob); err != nil {
		t.Fatalf("bob 移出 team-a：%v", err)
	}
	bobGroups, err = groups.ListGroupsOfUser(bob)
	if err != nil {
		t.Fatalf("重查 bob 的组：%v", err)
	}
	if len(bobGroups) != 1 || bobGroups[0] != teamBID {
		t.Errorf("bob 移出后应只属 team-b，得 %v", bobGroups)
	}
	// 重复移出（已不存在的成员关系）返回 ErrNotFound。
	if err := groups.RemoveMember(teamAID, bob); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("重复移出应 ErrNotFound，得 %v", err)
	}
}

// TestUserGroupMemberCascade 删组与删用户都由外键 CASCADE 清理成员关系：
// 不留悬空的「组→已删用户」或「用户→已删组」。
func TestUserGroupMemberCascade(t *testing.T) {
	groups, _, _, users := newGroupEnv(t)
	uid, err := users.Create("carol", "hash-c", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	gid, err := groups.Create("temp-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(gid, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}

	// 删组：成员关系随之消失。
	if err := groups.Delete(gid); err != nil {
		t.Fatalf("删组：%v", err)
	}
	if ids, err := groups.ListGroupsOfUser(uid); err != nil {
		t.Fatalf("反查用户所属组：%v", err)
	} else if len(ids) != 0 {
		t.Errorf("删组后成员关系应被 CASCADE 清理，得 %v", ids)
	}

	// 删用户：另一组的成员关系也随之消失。
	gid2, err := groups.Create("another-group", "")
	if err := groups.AddMember(gid2, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	if err := users.Delete(uid); err != nil {
		t.Fatalf("删用户：%v", err)
	}
	members, err := groups.ListMembers(gid2)
	if err != nil {
		t.Fatalf("列成员：%v", err)
	}
	if len(members) != 0 {
		t.Errorf("删用户后成员关系应被 CASCADE 清理，得 %d 条", len(members))
	}
}

// TestAclGroupSubjectGrant 组作为 ACL 主体的写入与判定：
// 组成员经组授权命中，非组成员不命中；用户自身授权与组授权互不覆盖。
func TestAclGroupSubjectGrant(t *testing.T) {
	groups, acls, repos, users := newGroupEnv(t)
	repoID := seedGroupRepo(t, repos, "group-acl-repo")

	alice, err := users.Create("alice", "hash-a", "user")
	if err != nil {
		t.Fatalf("建 alice：%v", err)
	}
	stranger, err := users.Create("stranger", "hash-s", "user")
	if err != nil {
		t.Fatalf("建 stranger：%v", err)
	}
	gid, err := groups.Create("team", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(gid, alice); err != nil {
		t.Fatalf("alice 加入组：%v", err)
	}

	// 混合写入：一条用户主体（stranger read）+ 一条组主体（team write）。
	entries := []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: stranger, Action: repository.ActionRead},
		{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: gid, Action: repository.ActionWrite},
	}
	if err := acls.Replace(repoID, entries); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}

	// 读回的条目两类主体都在，且各自列的正确。
	rows, err := acls.ListByRepo(repoID)
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应读出 2 条 ACL，得 %d：%+v", len(rows), rows)
	}
	var userRow, groupRow repository.Acl
	for _, r := range rows {
		switch r.SubjectType {
		case repository.SubjectTypeUser:
			userRow = r
		case repository.SubjectTypeGroup:
			groupRow = r
		}
	}
	if userRow.SubjectID != stranger || userRow.SubjectGroupID != 0 {
		t.Errorf("用户主体行列错位：%+v", userRow)
	}
	if groupRow.SubjectGroupID != gid || groupRow.SubjectID != 0 {
		t.Errorf("组主体行列错位：%+v", groupRow)
	}

	// alice 经组获得 write（并因此蕴含 read）；stranger 只有 read。
	aliceSubject := repository.Subject{UserID: alice, GroupIDs: []int64{gid}}
	if ok, err := acls.HasPermission(repoID, aliceSubject, repository.ActionWrite); err != nil || !ok {
		t.Errorf("组成员经组授权应可 write，得 ok=%t err=%v", ok, err)
	}
	if ok, err := acls.HasPermission(repoID, aliceSubject, repository.ActionRead); err != nil || !ok {
		t.Errorf("write 蕴含 read，得 ok=%t err=%v", ok, err)
	}
	// 非组成员：既没有自身授权也没有组授权，一律拒绝。
	strangerSubject := repository.Subject{UserID: stranger}
	if ok, _ := acls.HasPermission(repoID, strangerSubject, repository.ActionWrite); ok {
		t.Error("非组成员不应因他人所属组的授权而放行")
	}

	// 同一用户在**别的组**的授权不得串到本仓库：alice 换成一个未被授权的组 ID。
	if ok, _ := acls.HasPermission(repoID, repository.Subject{UserID: alice, GroupIDs: []int64{gid + 100}}, repository.ActionWrite); ok {
		t.Error("未授权组的成员不应放行")
	}
}

// TestAclGroupSubjectDeleteCleansGrants 删组后其 ACL 条目被清理（DeleteByGroup + 外键 CASCADE）。
func TestAclGroupSubjectDeleteCleansGrants(t *testing.T) {
	groups, acls, repos, users := newGroupEnv(t)
	repoID := seedGroupRepo(t, repos, "group-cleanup-repo")

	uid, err := users.Create("dave", "hash-d", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	gid, err := groups.Create("doomed-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(gid, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	if err := acls.Replace(repoID, []repository.Acl{
		{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: gid, Action: repository.ActionRead},
	}); err != nil {
		t.Fatalf("写组 ACL：%v", err)
	}
	if ok, _ := acls.HasPermission(repoID, repository.Subject{UserID: uid, GroupIDs: []int64{gid}}, repository.ActionRead); !ok {
		t.Fatal("前置：组成员应可读")
	}

	// 显式清理（领域层删组时走这一步）。
	if err := acls.DeleteByGroup(gid); err != nil {
		t.Fatalf("清理组 ACL：%v", err)
	}
	rows, err := acls.ListByRepo(repoID)
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 0 {
		t.Errorf("清理后仓库不应残留组条目，得 %+v", rows)
	}
	// 清理后组成员不再可读。
	if ok, _ := acls.HasPermission(repoID, repository.Subject{UserID: uid, GroupIDs: []int64{gid}}, repository.ActionRead); ok {
		t.Error("组授权清理后成员不应仍有权限")
	}

	// 外键 CASCADE 兜底：再写一条后直接删组，条目同样消失。
	if err := acls.Replace(repoID, []repository.Acl{
		{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: gid, Action: repository.ActionRead},
	}); err != nil {
		t.Fatalf("重写组 ACL：%v", err)
	}
	if err := groups.Delete(gid); err != nil {
		t.Fatalf("删组：%v", err)
	}
	rows, err = acls.ListByRepo(repoID)
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 0 {
		t.Errorf("删组后 ACL 条目应被 CASCADE 清理，得 %+v", rows)
	}
}

// TestAclActionImplicationMatrix 校验六档动作的蕴含矩阵（FR-36）：
// admin 蕴含全部；write 蕴含 read + publish；read/publish/delete/acl_manage 只满足自身。
// 同时钉住既有三档（read/write/admin）的行为完全不变。
func TestAclActionImplicationMatrix(t *testing.T) {
	_, acls, repos, users := newGroupEnv(t)
	uid, err := users.Create("erin", "hash-e", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}

	// 每条 granted 对应一个独立仓库，避免相互干扰。
	cases := []struct {
		granted string
		want    map[string]bool
	}{
		// 既有三档：行为必须与 FR-36 之前逐项一致。
		{repository.ActionRead, map[string]bool{
			repository.ActionRead: true, repository.ActionWrite: false, repository.ActionAdmin: false,
			repository.ActionPublish: false, repository.ActionDelete: false, repository.ActionAclManage: false,
		}},
		{repository.ActionWrite, map[string]bool{
			repository.ActionRead: true, repository.ActionWrite: true, repository.ActionAdmin: false,
			// write 蕴含 publish：存量 write 授权不因动作细化而失去发布能力。
			repository.ActionPublish: true, repository.ActionDelete: false, repository.ActionAclManage: false,
		}},
		{repository.ActionAdmin, map[string]bool{
			repository.ActionRead: true, repository.ActionWrite: true, repository.ActionAdmin: true,
			repository.ActionPublish: true, repository.ActionDelete: true, repository.ActionAclManage: true,
		}},
		// 新增三档：彼此独立，只满足自身。
		{repository.ActionPublish, map[string]bool{
			repository.ActionRead: false, repository.ActionWrite: false, repository.ActionAdmin: false,
			repository.ActionPublish: true, repository.ActionDelete: false, repository.ActionAclManage: false,
		}},
		{repository.ActionDelete, map[string]bool{
			repository.ActionRead: false, repository.ActionWrite: false, repository.ActionAdmin: false,
			repository.ActionPublish: false, repository.ActionDelete: true, repository.ActionAclManage: false,
		}},
		{repository.ActionAclManage, map[string]bool{
			repository.ActionRead: false, repository.ActionWrite: false, repository.ActionAdmin: false,
			repository.ActionPublish: false, repository.ActionDelete: false, repository.ActionAclManage: true,
		}},
	}

	subject := repository.UserSubject(uid)
	for _, c := range cases {
		t.Run("granted="+c.granted, func(t *testing.T) {
			name := "imply-" + c.granted
			repoID := seedGroupRepo(t, repos, name)
			if err := acls.Replace(repoID, []repository.Acl{{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: c.granted}}); err != nil {
				t.Fatalf("写 ACL：%v", err)
			}
			for action, want := range c.want {
				got, err := acls.HasPermission(repoID, subject, action)
				if err != nil {
					t.Fatalf("HasPermission(%s)：%v", action, err)
				}
				if got != want {
					t.Errorf("granted=%s 请求 %s = %t，期望 %t", c.granted, action, got, want)
				}
			}
		})
	}
}

// TestAclAnonymousNeverGranted 匿名主体（UserID==0，无组）在任何仓库上都不命中。
func TestAclAnonymousNeverGranted(t *testing.T) {
	_, acls, repos, users := newGroupEnv(t)
	repoID := seedGroupRepo(t, repos, "anon-acl-repo")
	uid, err := users.Create("frank", "hash-f", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	if err := acls.Replace(repoID, []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: repository.ActionAdmin},
	}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	for _, action := range []string{repository.ActionRead, repository.ActionWrite, repository.ActionPublish,
		repository.ActionDelete, repository.ActionAclManage, repository.ActionAdmin} {
		if ok, err := acls.HasPermission(repoID, repository.AnonymousSubject(), action); err != nil || ok {
			t.Errorf("匿名请求 %s 应恒拒绝，得 ok=%t err=%v", action, ok, err)
		}
	}
}

// TestAclUnknownActionNotGranted 未登记的动作（含空串）不得被宽泛授权放行。
func TestAclUnknownActionNotGranted(t *testing.T) {
	_, acls, repos, users := newGroupEnv(t)
	repoID := seedGroupRepo(t, repos, "unknown-action-repo")
	uid, err := users.Create("grace", "hash-g", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	if err := acls.Replace(repoID, []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: repository.ActionAdmin},
	}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	// admin 授权存在，但请求一个不存在的动作时不应命中。
	if ok, _ := acls.HasPermission(repoID, repository.UserSubject(uid), "superuser"); ok {
		t.Error("未登记动作不应被 admin 授权放行")
	}
	if ok, _ := acls.HasPermission(repoID, repository.UserSubject(uid), ""); ok {
		t.Error("空动作不应被授权放行")
	}
}
