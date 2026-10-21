package repository

import (
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住 FR-149 §3.5 的 ACL 判定批量化：
//
//  1. **结果必须与逐仓库 HasPermission 完全一致**——批量方法复用 satisfyingGrants（动作蕴含
//     关系只有一份实现），覆盖用户直授 / 仅组授 / 多组 / 无组 / admin / 匿名 / write 抵扣 read；
//  2. **查询次数不随仓库数增长**——用驱动级语句计数直接量出来，而不是靠耗时或语料规模推断。
//
// 第 2 条是本批改动的**目的本身**：只断言结果正确的测试无法发现「改完还是 N 次查询」。

// aclBatchFixture 准备一批仓库、用户与用户组，用于批量判定对照。
type aclBatchFixture struct {
	acls    *AclRepo
	groups  *UserGroupRepo
	counter *int64
	repos   []int64
	// 三类主体：直接授过权、未授过权、经组授过权。
	directUser int64
	plainUser  int64
	groupUser  int64
	writeGroup int64
	adminGroup int64
}

// aclSubjectFixture 是测试用的主体描述：用户 + 其所属组。
type aclSubjectFixture struct {
	name    string
	subject Subject
}

// newACLBatchFixture 建库、迁移，创建 repoCount 个仓库与固定的用户 / 组拓扑：
//
//	directUser  → 无组
//	plainUser   → 无组
//	groupUser   → writeGroup, adminGroup
//	writeGroup  → 授 write 的组
//	adminGroup  → 授 admin 的组
func newACLBatchFixture(t *testing.T, repoCount int) *aclBatchFixture {
	t.Helper()
	db, counter, err := persistence.OpenCountingForTest(filepath.Join(t.TempDir(), "acl-batch.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := NewRepoRepo(db)
	ids := make([]int64, 0, repoCount)
	for i := 0; i < repoCount; i++ {
		id, err := repos.Create("acl-repo-"+strconv.Itoa(i), "raw", "hosted", "private", "")
		if err != nil {
			t.Fatalf("建仓库：%v", err)
		}
		ids = append(ids, id)
	}
	users := NewUserRepo(db)
	createUser := func(name string) int64 {
		id, err := users.Create(name, "hash", "user")
		if err != nil {
			t.Fatalf("建用户 %s：%v", name, err)
		}
		return id
	}
	groups := NewUserGroupRepo(db)
	createGroup := func(name string) int64 {
		id, err := groups.Create(name, "")
		if err != nil {
			t.Fatalf("建组 %s：%v", name, err)
		}
		return id
	}
	f := &aclBatchFixture{
		acls:       NewAclRepo(db),
		groups:     groups,
		counter:    counter,
		repos:      ids,
		directUser: createUser("acl-direct"),
		plainUser:  createUser("acl-plain"),
		groupUser:  createUser("acl-group-member"),
		writeGroup: createGroup("acl-write-group"),
		adminGroup: createGroup("acl-admin-group"),
	}
	for _, g := range []int64{f.writeGroup, f.adminGroup} {
		if err := groups.AddMember(g, f.groupUser); err != nil {
			t.Fatalf("加入组成员：%v", err)
		}
	}
	return f
}

// queryCount 把计数器归零后执行 fn，返回 fn 期间发出的语句数。
func (f *aclBatchFixture) queryCount(fn func()) int64 {
	atomic.StoreInt64(f.counter, 0)
	fn()
	return atomic.LoadInt64(f.counter)
}

// subjects 返回参与一致性对照的主体集合。
func (f *aclBatchFixture) subjects() []aclSubjectFixture {
	return []aclSubjectFixture{
		{name: "用户直授命中", subject: UserSubject(f.directUser)},
		{name: "用户直授并属两个组", subject: Subject{UserID: f.directUser, GroupIDs: []int64{f.writeGroup, f.adminGroup}}},
		{name: "仅经组授权", subject: Subject{UserID: f.groupUser, GroupIDs: []int64{f.writeGroup, f.adminGroup}}},
		{name: "多组含重复与非正数", subject: Subject{UserID: f.groupUser, GroupIDs: []int64{f.writeGroup, f.writeGroup, 0, -3}}},
		{name: "无任何授权", subject: UserSubject(f.plainUser)},
		{name: "匿名主体", subject: AnonymousSubject()},
		{name: "已登录但无组", subject: Subject{UserID: f.plainUser, GroupIDs: nil}},
	}
}

// TestRepoIDsWithPermissionMatchesHasPermission 是批量与逐仓库判定的一致性对照。
func TestRepoIDsWithPermissionMatchesHasPermission(t *testing.T) {
	f := newACLBatchFixture(t, 6)
	repoA, repoB, repoC, repoD, repoE := f.repos[0], f.repos[1], f.repos[2], f.repos[3], f.repos[4]

	// repoA：用户直授 read
	if err := f.acls.Replace(repoA, []Acl{{SubjectID: f.directUser, Action: ActionRead}}); err != nil {
		t.Fatalf("写 repoA 的 ACL：%v", err)
	}
	// repoB：writeGroup 授 write（write 蕴含 read / publish）
	if err := f.acls.Replace(repoB, []Acl{{
		SubjectType: SubjectTypeGroup, SubjectGroupID: f.writeGroup, Action: ActionWrite}}); err != nil {
		t.Fatalf("写 repoB 的 ACL：%v", err)
	}
	// repoC：adminGroup 授 admin（admin 蕴含全部）
	if err := f.acls.Replace(repoC, []Acl{{
		SubjectType: SubjectTypeGroup, SubjectGroupID: f.adminGroup, Action: ActionAdmin}}); err != nil {
		t.Fatalf("写 repoC 的 ACL：%v", err)
	}
	// repoD：与任何参与对照的主体都无关的用户直授
	if err := f.acls.Replace(repoD, []Acl{{SubjectID: f.plainUser, Action: ActionRead}}); err != nil {
		t.Fatalf("写 repoD 的 ACL：%v", err)
	}
	// repoE：writeGroup 授 read
	if err := f.acls.Replace(repoE, []Acl{{
		SubjectType: SubjectTypeGroup, SubjectGroupID: f.writeGroup, Action: ActionRead}}); err != nil {
		t.Fatalf("写 repoE 的 ACL：%v", err)
	}
	// 第六个仓库没有任何 ACL 条目。

	actions := []string{ActionRead, ActionWrite, ActionPublish, ActionDelete, ActionAclManage, ActionAdmin}
	for _, sc := range f.subjects() {
		for _, action := range actions {
			granted, err := f.acls.RepoIDsWithPermission(f.repos, sc.subject, action)
			if err != nil {
				t.Fatalf("批量判定失败（%s/%s）：%v", sc.name, action, err)
			}
			for _, repoID := range f.repos {
				want, err := f.acls.HasPermission(repoID, sc.subject, action)
				if err != nil {
					t.Fatalf("逐仓库判定失败（%s/%s/%d）：%v", sc.name, action, repoID, err)
				}
				if granted[repoID] != want {
					t.Fatalf("批量与逐仓库不一致：主体=%s 动作=%s 仓库=%d 批量=%v 逐仓库=%v",
						sc.name, action, repoID, granted[repoID], want)
				}
			}
		}
	}
}

// TestRepoIDsWithPermissionEmptyInput 覆盖空入参（不产生任何查询）。
func TestRepoIDsWithPermissionEmptyInput(t *testing.T) {
	f := newACLBatchFixture(t, 1)
	n := f.queryCount(func() {
		got, err := f.acls.RepoIDsWithPermission(nil,
			Subject{UserID: f.groupUser, GroupIDs: []int64{f.writeGroup}}, ActionRead)
		if err != nil {
			t.Fatalf("空入参判定失败：%v", err)
		}
		if len(got) != 0 {
			t.Fatalf("空入参应返回空集合，得 %v", got)
		}
	})
	if n != 0 {
		t.Fatalf("空入参不应发出查询，实际 %d 条", n)
	}
}

// TestRepoIDsWithPermissionQueryCountDoesNotGrow 是本批改动的核心断言：
// 批量判定的查询数固定（无组 1 条 / 有组 2 条），而逐仓库 HasPermission 随仓库数线性增长。
func TestRepoIDsWithPermissionQueryCountDoesNotGrow(t *testing.T) {
	const small, large = 1, 40
	smallFx := newACLBatchFixture(t, small)
	largeFx := newACLBatchFixture(t, large)

	subjects := []struct {
		name      string
		subject   func(f *aclBatchFixture) Subject
		wantFixed int64
	}{
		{
			name: "已登录且属组",
			subject: func(f *aclBatchFixture) Subject {
				return Subject{UserID: f.groupUser, GroupIDs: []int64{f.writeGroup, f.adminGroup}}
			},
			wantFixed: 2,
		},
		{
			name:      "已登录无组",
			subject:   func(f *aclBatchFixture) Subject { return UserSubject(f.plainUser) },
			wantFixed: 1,
		},
		{
			name:      "匿名主体",
			subject:   func(*aclBatchFixture) Subject { return AnonymousSubject() },
			wantFixed: 1,
		},
	}
	for _, sc := range subjects {
		t.Run(sc.name, func(t *testing.T) {
			smallN := smallFx.queryCount(func() {
				if _, err := smallFx.acls.RepoIDsWithPermission(smallFx.repos, sc.subject(smallFx), ActionRead); err != nil {
					t.Fatalf("批量判定失败：%v", err)
				}
			})
			largeN := largeFx.queryCount(func() {
				if _, err := largeFx.acls.RepoIDsWithPermission(largeFx.repos, sc.subject(largeFx), ActionRead); err != nil {
					t.Fatalf("批量判定失败：%v", err)
				}
			})
			if smallN != sc.wantFixed || largeN != sc.wantFixed {
				t.Fatalf("批量判定查询数应恒为 %d 条（主体=%s）：1 个仓库=%d，%d 个仓库=%d",
					sc.wantFixed, sc.name, smallN, large, largeN)
			}
			// 旧实现（逐仓库 HasPermission）必须随仓库数线性增长——这正是本批要消掉的 N+1。
			legacySmall := smallFx.queryCount(func() {
				for _, id := range smallFx.repos {
					if _, err := smallFx.acls.HasPermission(id, sc.subject(smallFx), ActionRead); err != nil {
						t.Fatalf("逐仓库判定失败：%v", err)
					}
				}
			})
			legacyLarge := largeFx.queryCount(func() {
				for _, id := range largeFx.repos {
					if _, err := largeFx.acls.HasPermission(id, sc.subject(largeFx), ActionRead); err != nil {
						t.Fatalf("逐仓库判定失败：%v", err)
					}
				}
			})
			if legacyLarge <= legacySmall {
				t.Fatalf("逐仓库实现的查询数应随仓库数增长：1 个仓库=%d，%d 个仓库=%d",
					legacySmall, large, legacyLarge)
			}
		})
	}
}

// TestRepoIDsWithPermissionActionImplication 钉住「复用 satisfyingGrants」：
// 只授 write 能读（FR-36 蕴含），只授 publish 不能读，admin 蕴含全部。
func TestRepoIDsWithPermissionActionImplication(t *testing.T) {
	f := newACLBatchFixture(t, 4)
	writeRepo, publishRepo, adminRepo, plainRepo := f.repos[0], f.repos[1], f.repos[2], f.repos[3]
	if err := f.acls.Replace(writeRepo, []Acl{{SubjectID: f.directUser, Action: ActionWrite}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	if err := f.acls.Replace(publishRepo, []Acl{{SubjectID: f.directUser, Action: ActionPublish}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	if err := f.acls.Replace(adminRepo, []Acl{{SubjectID: f.directUser, Action: ActionAdmin}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	subject := UserSubject(f.directUser)

	readable, err := f.acls.RepoIDsWithPermission(f.repos, subject, ActionRead)
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	if !readable[writeRepo] || !readable[adminRepo] {
		t.Fatalf("write / admin 授权应满足 read 请求：%v", readable)
	}
	if readable[publishRepo] || readable[plainRepo] {
		t.Fatalf("publish 授权与无授权都不应满足 read 请求：%v", readable)
	}

	publishable, err := f.acls.RepoIDsWithPermission(f.repos, subject, ActionPublish)
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	if !publishable[writeRepo] || !publishable[publishRepo] || !publishable[adminRepo] {
		t.Fatalf("write / publish / admin 都应满足 publish 请求：%v", publishable)
	}

	deleteable, err := f.acls.RepoIDsWithPermission(f.repos, subject, ActionDelete)
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	if deleteable[writeRepo] || deleteable[publishRepo] || !deleteable[adminRepo] {
		t.Fatalf("delete 只应由 admin 满足（write / publish 不得顺带放行）：%v", deleteable)
	}
}
