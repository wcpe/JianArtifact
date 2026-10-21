package domain_test

import (
	"sort"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 RepositoryService.ReadableRepoIDs 与逐仓库 CanAccessResolved 的等价性。
//
// 仓库列表端点把「每个仓库各判一次」压成一次批量判定，允许改变的只有语句数：判定结果必须
// 逐行不变——这是安全路径，任何放宽都等于提权。故这里不写「批量判定的期望值」，而是把仍然
// 保留在领域层的逐仓库判定当成对照物，对每个仓库逐一比对。
//
// 覆盖仓库侧四类可见性来源（公开 / 用户直授 / 仅组授 / 无权限，另加「write 蕴含 read」与
// 「publish 不蕴含 read」两条蕴含关系）和主体侧多种形态（普通用户、含组用户、陌生人、匿名）。

// readableFixture 是等价性对照用的固定拓扑。
type readableFixture struct {
	svc        *domain.RepositoryService
	settings   *domain.SettingService
	users      *repository.UserRepo
	allRows    []repository.Repository
	memberID   int64
	strangerID int64
}

// newReadableFixture 建库并铺设「两个用户 + 一个组 + 六个仓库」的拓扑：
//
//	readable-public   公开仓库（无需任何 ACL）
//	readable-direct   private，直接授 member read
//	readable-group    private，仅授 dev 组 read
//	readable-denied   private，无任何 ACL
//	readable-write    private，直接授 member write（write 蕴含 read）
//	readable-publish  private，直接授 member publish（publish 不蕴含 read）
func newReadableFixture(t *testing.T) *readableFixture {
	t.Helper()
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	acls := repository.NewAclRepo(db)
	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repoRepo, acls, repository.NewAssetRepo(db), settings, users)
	svc.SetUserGroupRepo(groups)

	member, err := users.Create("readable-member", "hash", "user")
	if err != nil {
		t.Fatalf("建成员用户：%v", err)
	}
	stranger, err := users.Create("readable-stranger", "hash", "user")
	if err != nil {
		t.Fatalf("建陌生用户：%v", err)
	}
	devGroup, err := groups.Create("readable-dev", "可读组")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(devGroup, member); err != nil {
		t.Fatalf("加入组成员：%v", err)
	}

	create := func(name, visibility string) *repository.Repository {
		repo, err := svc.Create(name, "raw", "hosted", visibility, "", repository.RepositoryConfig{})
		if err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
		return repo
	}
	publicRepo := create("readable-public", "public")
	directRepo := create("readable-direct", "private")
	groupRepo := create("readable-group", "private")
	create("readable-denied", "private")
	writeRepo := create("readable-write", "private")
	publishRepo := create("readable-publish", "private")

	setAcl := func(repo *repository.Repository, entries []repository.Acl) {
		if err := acls.Replace(repo.ID, entries); err != nil {
			t.Fatalf("写 %s 的 ACL：%v", repo.Name, err)
		}
	}
	setAcl(directRepo, []repository.Acl{{SubjectID: member, Action: repository.ActionRead}})
	setAcl(groupRepo, []repository.Acl{{
		SubjectType: repository.SubjectTypeGroup, SubjectGroupID: devGroup, Action: repository.ActionRead}})
	setAcl(writeRepo, []repository.Acl{{SubjectID: member, Action: repository.ActionWrite}})
	setAcl(publishRepo, []repository.Acl{{SubjectID: member, Action: repository.ActionPublish}})
	_ = publicRepo

	allRows, err := repoRepo.List(1000, 0)
	if err != nil {
		t.Fatalf("列举仓库：%v", err)
	}
	return &readableFixture{
		svc:        svc,
		settings:   settings,
		users:      users,
		allRows:    allRows,
		memberID:   member,
		strangerID: stranger,
	}
}

// subjectOf 按用户 ID 展开组归属，与接口层构造主体的方式一致。
func (f *readableFixture) subjectOf(t *testing.T, userID int64) repository.Subject {
	t.Helper()
	subject, err := f.svc.SubjectFor(userID)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}
	return subject
}

// readableNames 把可读 ID 集合翻译成有序仓库名，便于断言与失败信息阅读。
func (f *readableFixture) readableNames(t *testing.T, readable map[int64]bool) []string {
	t.Helper()
	names := make([]string, 0, len(readable))
	for _, repo := range f.allRows {
		if readable[repo.ID] {
			names = append(names, repo.Name)
		}
	}
	sort.Strings(names)
	return names
}

// expectedNames 用逐仓库 CanAccessResolved 算出期望集合（即改造前列表端点的判定方式）。
func (f *readableFixture) expectedNames(t *testing.T, subject repository.Subject) []string {
	t.Helper()
	names := make([]string, 0, len(f.allRows))
	for i := range f.allRows {
		ok, err := f.svc.CanAccessResolved(&f.allRows[i], subject, repository.ActionRead)
		if err != nil {
			t.Fatalf("逐仓库判定 %s：%v", f.allRows[i].Name, err)
		}
		if ok {
			names = append(names, f.allRows[i].Name)
		}
	}
	sort.Strings(names)
	return names
}

// equalNames 比较两个有序名称切片。
func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReadableRepoIDsMatchesCanAccessResolvedPerRepo 是逐行的等价性对照：
// 批量判定的结果对每个仓库都必须与逐仓库判定一致，主体形态不管怎么组合都不能分叉。
func TestReadableRepoIDsMatchesCanAccessResolvedPerRepo(t *testing.T) {
	t.Parallel()
	f := newReadableFixture(t)

	member := f.subjectOf(t, f.memberID)
	subjects := []struct {
		name    string
		subject repository.Subject
	}{
		{name: "含组用户（组授 read 应命中）", subject: member},
		{name: "用户自身无组归属（组授 read 应落空）", subject: repository.UserSubject(f.memberID)},
		{name: "仅用户自身且组集合为空切片", subject: repository.Subject{UserID: f.memberID, GroupIDs: []int64{}}},
		{name: "组集合含重复与非正数", subject: repository.Subject{UserID: f.memberID,
			GroupIDs: append([]int64{0, -3}, append(append([]int64{}, member.GroupIDs...), member.GroupIDs...)...)}},
		{name: "无任何授权的陌生人", subject: f.subjectOf(t, f.strangerID)},
		{name: "匿名主体", subject: repository.AnonymousSubject()},
	}
	for _, sc := range subjects {
		t.Run(sc.name, func(t *testing.T) {
			readable, err := f.svc.ReadableRepoIDs(f.allRows, sc.subject)
			if err != nil {
				t.Fatalf("批量判定失败：%v", err)
			}
			want := f.expectedNames(t, sc.subject)
			got := f.readableNames(t, readable)
			if !equalNames(got, want) {
				t.Fatalf("批量判定与逐仓库判定不一致：\n批量   = %v\n逐仓库 = %v", got, want)
			}
		})
	}
}

// TestReadableRepoIDsVisibilityCases 把四类可见性来源的期望值直接写死，
// 避免「批量结果 == 逐仓库结果」在两边同时改错时仍然通过。
func TestReadableRepoIDsVisibilityCases(t *testing.T) {
	t.Parallel()
	f := newReadableFixture(t)

	cases := []struct {
		name    string
		subject repository.Subject
		want    []string
	}{
		{
			// 公开仓库放行、直授 read 命中、组授 read 命中、write 蕴含 read；
			// 无授权的 readable-denied 与只授 publish 的 readable-publish 都不得出现。
			name:    "含组用户",
			subject: f.subjectOf(t, f.memberID),
			want:    []string{"readable-direct", "readable-group", "readable-public", "readable-write"},
		},
		{
			name:    "只按用户自身判定（无组）",
			subject: repository.UserSubject(f.memberID),
			want:    []string{"readable-direct", "readable-public", "readable-write"},
		},
		{
			name:    "陌生人",
			subject: f.subjectOf(t, f.strangerID),
			want:    []string{"readable-public"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readable, err := f.svc.ReadableRepoIDs(f.allRows, tc.subject)
			if err != nil {
				t.Fatalf("批量判定失败：%v", err)
			}
			if got := f.readableNames(t, readable); !equalNames(got, tc.want) {
				t.Fatalf("可读集合不符：\n期望 = %v\n实际 = %v", tc.want, got)
			}
		})
	}
}

// TestReadableRepoIDsAnonymousFollowsSwitch 覆盖匿名判定的顺序：
// 开关关闭时匿名**连公开仓库也不可读**（与 canAccessAnonymous 的判定顺序一致），
// 开关开启时公开仓库放行、私有仓库按内置 anonymous 主体的 ACL 判定；
// 已登录主体不受该开关影响。
func TestReadableRepoIDsAnonymousFollowsSwitch(t *testing.T) {
	t.Parallel()
	f := newReadableFixture(t)

	// 默认（尚无关键时）匿名访问是开启的：公开仓库可读，私有仓库一律不可读。
	readable, err := f.svc.ReadableRepoIDs(f.allRows, repository.AnonymousSubject())
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	if got := f.readableNames(t, readable); !equalNames(got, []string{"readable-public"}) {
		t.Fatalf("匿名默认应只读到公开仓库，得 %v", got)
	}

	// 给内置 anonymous 主体授 read：对应私有仓库随之进入匿名可读集合。
	anon, err := f.users.GetByUsername(domain.AnonymousUsername)
	if err != nil {
		t.Fatalf("迁移应内置 anonymous 用户：%v", err)
	}
	var deniedID int64
	for _, repo := range f.allRows {
		if repo.Name == "readable-denied" {
			deniedID = repo.ID
		}
	}
	if deniedID == 0 {
		t.Fatal("测试拓扑缺少 readable-denied 仓库")
	}
	if _, err := f.svc.SetAcl("readable-denied", []repository.Acl{
		{SubjectID: anon.ID, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("给 anonymous 授 read：%v", err)
	}
	readable, err = f.svc.ReadableRepoIDs(f.allRows, repository.AnonymousSubject())
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	if got := f.readableNames(t, readable); !equalNames(got, []string{"readable-denied", "readable-public"}) {
		t.Fatalf("anonymous 被授 read 后应读到公开 + 该私有仓库，得 %v", got)
	}

	// 关闭匿名访问：匿名主体对**全部**仓库都不可读，公开仓库也不例外。
	if err := f.settings.SetAnonymousAccessEnabled(false); err != nil {
		t.Fatalf("关闭匿名访问：%v", err)
	}
	readable, err = f.svc.ReadableRepoIDs(f.allRows, repository.AnonymousSubject())
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	if got := f.readableNames(t, readable); len(got) != 0 {
		t.Fatalf("匿名访问关闭后不应有任何可读仓库，得 %v", got)
	}
	// 已登录主体不受开关影响。
	readable, err = f.svc.ReadableRepoIDs(f.allRows, f.subjectOf(t, f.memberID))
	if err != nil {
		t.Fatalf("批量判定失败：%v", err)
	}
	want := []string{"readable-direct", "readable-group", "readable-public", "readable-write"}
	if got := f.readableNames(t, readable); !equalNames(got, want) {
		t.Fatalf("开关只约束匿名主体，已登录主体的可读集合不应变化：\n期望 = %v\n实际 = %v", want, got)
	}
}

// TestReadableRepoIDsEmptyInput 覆盖空入参：返回空集合而不是 nil 错误。
func TestReadableRepoIDsEmptyInput(t *testing.T) {
	t.Parallel()
	f := newReadableFixture(t)
	readable, err := f.svc.ReadableRepoIDs(nil, f.subjectOf(t, f.memberID))
	if err != nil {
		t.Fatalf("空入参不应报错：%v", err)
	}
	if len(readable) != 0 {
		t.Fatalf("空入参应返回空集合，得 %v", readable)
	}
}
