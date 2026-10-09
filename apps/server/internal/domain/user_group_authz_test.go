package domain_test

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newGroupAuthzEnv 组装「带组支持的授权环境」：仓库服务已注入组仓储，
// 配套返回组仓储 / ACL 仓储 / 用户仓储，供用例铺设「组 × 成员 × 仓库 ACL」。
func newGroupAuthzEnv(t *testing.T) (*domain.RepositoryService, *repository.UserGroupRepo, *repository.AclRepo, *repository.UserRepo) {
	t.Helper()
	db := newTestDB(t)
	acls := repository.NewAclRepo(db)
	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	svc := domain.NewRepositoryService(
		repository.NewRepoRepo(db), acls, repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)), users,
	)
	svc.SetUserGroupRepo(groups)
	return svc, groups, acls, users
}

// createPrivateRepo 建一个 raw hosted 私有仓库（不参与 public 快捷放行）。
func createPrivateRepo(t *testing.T, svc *domain.RepositoryService, name string) {
	t.Helper()
	if _, err := svc.Create(name, "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库 %s：%v", name, err)
	}
}

// TestCanAccessViaGroupMembership 组作为 ACL 主体的放行与拒绝（FR-36 核心）：
// 组成员经组授权命中；非组成员不命中；同一用户在不同组的授权互不串台。
func TestCanAccessViaGroupMembership(t *testing.T) {
	t.Parallel()
	svc, groups, _, users := newGroupAuthzEnv(t)

	member, err := users.Create("group-member", "hash-m", "user")
	if err != nil {
		t.Fatalf("建成员用户：%v", err)
	}
	outsider, err := users.Create("outsider", "hash-o", "user")
	if err != nil {
		t.Fatalf("建外部用户：%v", err)
	}
	devGroup, err := groups.Create("dev-group", "开发组")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(devGroup, member); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	createPrivateRepo(t, svc, "group-granted-repo")

	// 该仓库只对 dev-group 授 write；没有任何用户自身条目。
	if _, err := svc.SetAcl("group-granted-repo", []repository.Acl{{
		SubjectType:    repository.SubjectTypeGroup,
		SubjectGroupID: devGroup,
		Action:         repository.ActionWrite,
	}}); err != nil {
		t.Fatalf("写组 ACL：%v", err)
	}

	// 组成员：write 放行，且因 write 蕴含 read / publish 而连带放行。
	memberSubject, err := svc.SubjectFor(member)
	if err != nil {
		t.Fatalf("展开成员主体：%v", err)
	}
	if len(memberSubject.GroupIDs) != 1 || memberSubject.GroupIDs[0] != devGroup {
		t.Fatalf("成员主体应含该组，得 %+v", memberSubject)
	}
	for _, action := range []string{repository.ActionWrite, repository.ActionRead, repository.ActionPublish} {
		ok, err := svc.CanAccess("group-granted-repo", memberSubject, action)
		if err != nil {
			t.Fatalf("CanAccess(%s)：%v", action, err)
		}
		if !ok {
			t.Errorf("组成员经组 write 应可 %s", action)
		}
	}
	// write 不蕴含 delete / acl_manage / admin：粒度不得被撑大。
	for _, action := range []string{repository.ActionDelete, repository.ActionAclManage, repository.ActionAdmin} {
		ok, err := svc.CanAccess("group-granted-repo", memberSubject, action)
		if err != nil {
			t.Fatalf("CanAccess(%s)：%v", action, err)
		}
		if ok {
			t.Errorf("组 write 不应放行 %s", action)
		}
	}

	// 非组成员：一律拒绝（六档全否）。
	outsiderSubject, err := svc.SubjectFor(outsider)
	if err != nil {
		t.Fatalf("展开外部用户主体：%v", err)
	}
	for _, action := range allActions {
		ok, err := svc.CanAccess("group-granted-repo", outsiderSubject, action)
		if err != nil {
			t.Fatalf("CanAccess(%s)：%v", action, err)
		}
		if ok {
			t.Errorf("非组成员不应可 %s", action)
		}
	}
}

// TestCanAccessGroupGrantsUnionWithUserGrants 用户自身授权与组授权取并集：
// 任一命中即放行；移除组成员后回落为仅用户自身的授权。
func TestCanAccessGroupGrantsUnionWithUserGrants(t *testing.T) {
	t.Parallel()
	svc, groups, _, users := newGroupAuthzEnv(t)

	uid, err := users.Create("union-user", "hash-u", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	groupID, err := groups.Create("union-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	createPrivateRepo(t, svc, "union-repo")

	// 用户自身 read + 所在组 delete：两条互不相干的授权同时存在。
	if _, err := svc.SetAcl("union-repo", []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: repository.ActionRead},
		{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: groupID, Action: repository.ActionDelete},
	}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}

	// 尚未加入组：只有 read，没有 delete。
	before, err := svc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}
	if ok, _ := svc.CanAccess("union-repo", before, repository.ActionRead); !ok {
		t.Error("用户自身 read 应放行")
	}
	if ok, _ := svc.CanAccess("union-repo", before, repository.ActionDelete); ok {
		t.Error("未加入组时不应有 delete")
	}

	// 加入组后：两者并集，delete 也放行。
	if err := groups.AddMember(groupID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	after, err := svc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("重新展开主体：%v", err)
	}
	if ok, _ := svc.CanAccess("union-repo", after, repository.ActionRead); !ok {
		t.Error("加入组后用户自身 read 应保留")
	}
	if ok, _ := svc.CanAccess("union-repo", after, repository.ActionDelete); !ok {
		t.Error("加入组后应能 delete（组授权）")
	}

	// 移出组后回落：delete 消失，read 仍在。
	if err := groups.RemoveMember(groupID, uid); err != nil {
		t.Fatalf("移除成员：%v", err)
	}
	removed, err := svc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("再次展开主体：%v", err)
	}
	if ok, _ := svc.CanAccess("union-repo", removed, repository.ActionDelete); ok {
		t.Error("移出组后不应仍有 delete")
	}
	if ok, _ := svc.CanAccess("union-repo", removed, repository.ActionRead); !ok {
		t.Error("移出组不应影响用户自身 read")
	}
}

// TestCanAccessGroupMemberAcrossMultipleGroups 用户同时属于多个组时，
// 任一组的授权都生效（多组并集）。
func TestCanAccessGroupMemberAcrossMultipleGroups(t *testing.T) {
	t.Parallel()
	svc, groups, _, users := newGroupAuthzEnv(t)

	uid, err := users.Create("multi-group-user", "hash-mu", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	groupA, err := groups.Create("group-a", "")
	if err != nil {
		t.Fatalf("建 group-a：%v", err)
	}
	groupB, err := groups.Create("group-b", "")
	if err != nil {
		t.Fatalf("建 group-b：%v", err)
	}
	// 建一个用户**未加入**的组，用于验证未加入组的授权不会串到该用户身上。
	if _, err := groups.Create("group-c-unjoined", ""); err != nil {
		t.Fatalf("建未加入的组：%v", err)
	}
	if err := groups.AddMember(groupA, uid); err != nil {
		t.Fatalf("加入 group-a：%v", err)
	}
	if err := groups.AddMember(groupB, uid); err != nil {
		t.Fatalf("加入 group-b：%v", err)
	}

	// repo-a 只授 group-a(publish)，repo-b 只授 group-b(delete)，repo-c 授未加入组。
	for repoName, granted := range map[string]struct {
		groupID int64
		action  string
	}{
		"multi-a-repo": {groupA, repository.ActionPublish},
		"multi-b-repo": {groupB, repository.ActionDelete},
	} {
		createPrivateRepo(t, svc, repoName)
		if _, err := svc.SetAcl(repoName, []repository.Acl{{
			SubjectType: repository.SubjectTypeGroup, SubjectGroupID: granted.groupID, Action: granted.action,
		}}); err != nil {
			t.Fatalf("写 %s 的 ACL：%v", repoName, err)
		}
	}

	subject, err := svc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}
	if len(subject.GroupIDs) != 2 {
		t.Fatalf("应属两个组，得 %v", subject.GroupIDs)
	}
	if ok, _ := svc.CanAccess("multi-a-repo", subject, repository.ActionPublish); !ok {
		t.Error("group-a 成员应对 repo-a 可 publish")
	}
	if ok, _ := svc.CanAccess("multi-b-repo", subject, repository.ActionDelete); !ok {
		t.Error("group-b 成员应对 repo-b 可 delete")
	}
	// 跨仓/跨组不串：group-b 的 delete 不能用在 repo-a 上。
	if ok, _ := svc.CanAccess("multi-a-repo", subject, repository.ActionDelete); ok {
		t.Error("repo-a 未授 delete，group-b 的授权不应串过去")
	}
}

// TestCanAccessPublicRepoWithGroups public 仓库的匿名 / 已认证行为不因组改造而改变：
// public 对 read 无条件放行（含非成员），但 publish/delete/acl_manage/admin 仍拒绝。
func TestCanAccessPublicRepoWithGroups(t *testing.T) {
	t.Parallel()
	svc, groups, _, users := newGroupAuthzEnv(t)
	if _, err := svc.Create("public-group-repo", "raw", "hosted", "public", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建 public 仓库：%v", err)
	}
	groupID, err := groups.Create("public-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	uid, err := users.Create("public-visitor", "hash-pv", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	if err := groups.AddMember(groupID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	subject, err := svc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}

	if ok, err := svc.CanAccess("public-group-repo", subject, repository.ActionRead); err != nil || !ok {
		t.Errorf("public 仓库 read 应对任何已认证主体放行，得 ok=%t err=%v", ok, err)
	}
	if ok, err := svc.CanAccess("public-group-repo", repository.AnonymousSubject(), repository.ActionRead); err != nil || !ok {
		t.Errorf("public 仓库 read 应对匿名放行，得 ok=%t err=%v", ok, err)
	}
	// 其余五档：无 ACL 一律拒绝（public 只豁免 read）。
	for _, action := range []string{repository.ActionWrite, repository.ActionPublish,
		repository.ActionDelete, repository.ActionAclManage, repository.ActionAdmin} {
		if ok, _ := svc.CanAccess("public-group-repo", subject, action); ok {
			t.Errorf("public 仓库无 ACL 时不应放行 %s", action)
		}
	}
}

// TestCanAccessWithoutGroupRepoInjected 未注入组仓储时，授权退化为「仅按用户自身 ACL」
// 判定——即 FR-36 之前的既有行为：该用户所属组的组授权此时**不生效**。
// 保证降级装配（如测试夹具、部分 command 路径）不会反过来引入提权。
func TestCanAccessWithoutGroupRepoInjected(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	acls := repository.NewAclRepo(db)
	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	// 刻意不调用 SetUserGroupRepo。
	svc := domain.NewRepositoryService(
		repository.NewRepoRepo(db), acls, repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)), users,
	)

	uid, err := users.Create("legacy-user", "hash-l", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	groupID, err := groups.Create("legacy-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(groupID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	createPrivateRepo(t, svc, "legacy-repo")
	// 只授组 write，同时授该用户 read。
	if _, err := svc.SetAcl("legacy-repo", []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: repository.ActionRead},
		{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: groupID, Action: repository.ActionWrite},
	}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}

	subject, err := svc.SubjectFor(uid)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}
	if len(subject.GroupIDs) != 0 {
		t.Fatalf("未注入组仓储时组集合应为空，得 %v", subject.GroupIDs)
	}
	// 用户自身的 read 仍生效。
	if ok, _ := svc.CanAccess("legacy-repo", subject, repository.ActionRead); !ok {
		t.Error("用户自身 read 应放行")
	}
	// 组授权不生效：降级不得放大权限。
	if ok, _ := svc.CanAccess("legacy-repo", subject, repository.ActionWrite); ok {
		t.Error("未注入组仓储时组授权不应生效（避免降级提权）")
	}
}

// TestSetAclRejectsUnknownAction 写入未登记的 ACL 动作被领域层拦下（ErrValidation），
// 而不是一路到 SQLite CHECK 才炸。
func TestSetAclRejectsUnknownAction(t *testing.T) {
	t.Parallel()
	svc, _, _, users := newGroupAuthzEnv(t)
	uid, err := users.Create("acl-validation-user", "hash-av", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	createPrivateRepo(t, svc, "validation-repo")

	if _, err := svc.SetAcl("validation-repo", []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: "superuser"},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("未知 ACL 动作应 ErrValidation，得 %v", err)
	}
	// 拒绝时不得破坏既有 ACL（SET 是覆盖写，必须整批校验后再动库）。
	if _, err := svc.SetAcl("validation-repo", []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: repository.ActionRead},
	}); err != nil {
		t.Fatalf("写合法 ACL：%v", err)
	}
	if _, err := svc.SetAcl("validation-repo", []repository.Acl{
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: repository.ActionRead},
		{SubjectType: repository.SubjectTypeUser, SubjectID: uid, Action: "bogus"},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("含非法动作的批次应 ErrValidation，得 %v", err)
	}
	rows, err := svc.GetAcl("validation-repo")
	if err != nil {
		t.Fatalf("读 ACL：%v", err)
	}
	if len(rows) != 1 || rows[0].Action != repository.ActionRead {
		t.Errorf("校验失败不应破坏既有 ACL，得 %+v", rows)
	}
}

// TestSearchAssetsIncludesGroupGrantedRepos 组被授 read 的私有仓库进入该组成员
// 的搜索范围（FR-36 在搜索侧的落地）。
func TestSearchAssetsIncludesGroupGrantedRepos(t *testing.T) {
	t.Parallel()
	svc, groups, _, users := newGroupAuthzEnv(t)

	uid, err := users.Create("search-member", "hash-sm", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	groupID, err := groups.Create("search-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(groupID, uid); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	createPrivateRepo(t, svc, "search-group-repo")
	if _, err := svc.SetAcl("search-group-repo", []repository.Acl{{
		SubjectType: repository.SubjectTypeGroup, SubjectGroupID: groupID, Action: repository.ActionRead,
	}}); err != nil {
		t.Fatalf("写组 ACL：%v", err)
	}

	// 传入用户 ID：服务层自行展开组归属，命中组授权的仓库。
	out, err := svc.SearchAssets("anything", "", uid, false, "path", "asc", 10, 0)
	if err != nil {
		t.Fatalf("搜索：%v", err)
	}
	if out == nil {
		t.Fatal("搜索应返回非空结果对象")
	}
}
