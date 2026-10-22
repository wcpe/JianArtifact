package domain_test

import (
	"context"
	"sort"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住搜索侧的两条**权限不变量**（实现目前是正确的，故这里是保护性测试）：
//
//  1. facet 只统计主体可读的仓库。facet 是响应里唯一会主动列出「仓库名 + 命中数」的字段，
//     比 items 更容易泄漏「某个你看不到的仓库里有多少命中」——不能只靠 items 正确就放心。
//  2. repo: / -repo: / format: 这类收窄表达式只能缩小可读集合，不能反向扩大权限：
//     非管理员把表达式指向不可读仓库或仅被不可读仓库使用的格式时，结果必须为空，
//     而不是「按表达式直接查库」。
//
// 期望值一律用逐仓库 CanAccessResolved **现算**，不写死仓库名组合——这样断言的是
// 「批量口径 == 逐仓库口径」，实现若在两处同时漂移也能被发现（与仓库列表侧的对照手法一致）。

// searchAuthzEnv 是搜索权限测试的固定拓扑。
type searchAuthzEnv struct {
	svc      *domain.RepositoryService
	repos    *repository.RepoRepo
	memberID int64

	publicName string // 公开仓库（raw），任何主体可读
	directName string // private，直授成员 read（maven）
	groupName  string // private，仅组授成员 read（pypi）
	hiddenName string // private，无任何 ACL（npm：全实例唯一格式，供 format: 收窄验证）
	hiddenFmt  string
}

// newSearchAuthzEnv 建四个仓库与一条同名命中路径，并给成员用户铺好直授 / 组授两条通路。
func newSearchAuthzEnv(t *testing.T) *searchAuthzEnv {
	t.Helper()
	svc, repos, acls, users, groups, assets, _ := newSearchSvcEnv(t)
	env := &searchAuthzEnv{
		svc:        svc,
		repos:      repos,
		publicName: "facx-public",
		directName: "facx-direct",
		groupName:  "facx-group",
		hiddenName: "facx-hidden",
		hiddenFmt:  "npm",
	}

	member, err := users.Create("facx-member", "hash", "user")
	if err != nil {
		t.Fatalf("建成员用户：%v", err)
	}
	env.memberID = member
	readers, err := groups.Create("facx-readers", "搜索授权组")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(readers, member); err != nil {
		t.Fatalf("加入组成员：%v", err)
	}

	create := func(name, format, visibility string) int64 {
		id, err := repos.Create(name, format, "hosted", visibility, "")
		if err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
		return id
	}
	publicID := create(env.publicName, "raw", "public")
	directID := create(env.directName, "maven", "private")
	groupID := create(env.groupName, "pypi", "private")
	// 无任何 ACL 的仓库：它必须对非管理员彻底不可见，包括 facet 与任何表达式收窄。
	hiddenID := create(env.hiddenName, env.hiddenFmt, "private")

	if err := acls.Replace(directID, []repository.Acl{{SubjectID: member, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("写直授 ACL：%v", err)
	}
	if err := acls.Replace(groupID, []repository.Acl{{
		SubjectType: repository.SubjectTypeGroup, SubjectGroupID: readers, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("写组授 ACL：%v", err)
	}

	// 四个仓库放同一条命中路径：结果里出现的仓库名即「可搜范围」的直接证据。
	for _, id := range []int64{publicID, directID, groupID, hiddenID} {
		if err := assets.Upsert(id, "facx/probe-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("写入资产：%v", err)
		}
	}
	return env
}

// readableNames 用逐仓库 CanAccessResolved 算出成员（非管理员）的可读仓库名。
// 这是**对照物**：搜索侧的批量判定必须与它逐项一致。
func (e *searchAuthzEnv) readableNames(t *testing.T) []string {
	t.Helper()
	subject, err := e.svc.SubjectFor(e.memberID)
	if err != nil {
		t.Fatalf("展开主体：%v", err)
	}
	rows, err := e.repos.List(1000, 0)
	if err != nil {
		t.Fatalf("列举仓库：%v", err)
	}
	names := make([]string, 0, len(rows))
	for i := range rows {
		ok, err := e.svc.CanAccessResolved(&rows[i], subject, repository.ActionRead)
		if err != nil {
			t.Fatalf("逐仓库判定 %s：%v", rows[i].Name, err)
		}
		if ok {
			names = append(names, rows[i].Name)
		}
	}
	sort.Strings(names)
	return names
}

// searchAsMember 以**非管理员**身份执行一次文件级搜索。
func (e *searchAuthzEnv) searchAsMember(t *testing.T, q string) *domain.SearchOutput {
	t.Helper()
	out, err := e.svc.SearchAggregate(context.Background(), q, "", e.memberID, false,
		"", "asc", domain.SearchGroupByFile, 50, 0)
	if err != nil {
		t.Fatalf("搜索 %q 失败：%v", q, err)
	}
	return out
}

// facetRepoNames 取 facet 里的仓库名（排序后便于集合比较；facet 本身按命中数降序）。
func facetRepoNames(out *domain.SearchOutput) []string {
	names := make([]string, 0, len(out.Facets))
	for _, f := range out.Facets {
		names = append(names, f.RepoName)
	}
	sort.Strings(names)
	return names
}

// itemRepoNames 取结果条目的仓库名（排序后端去重，便于集合比较）。
func itemRepoNames(out *domain.SearchOutput) []string {
	seen := make(map[string]bool, len(out.Items))
	names := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		if seen[item.RepoName] {
			continue
		}
		seen[item.RepoName] = true
		names = append(names, item.RepoName)
	}
	sort.Strings(names)
	return names
}

// TestSearchFacetsOnlyListReadableRepos 钉住「facet 只含可读仓库」：
// 非管理员搜索后，facet 的仓库集合必须恰好等于其可读仓库集合。
func TestSearchFacetsOnlyListReadableRepos(t *testing.T) {
	env := newSearchAuthzEnv(t)
	out := env.searchAsMember(t, "facx")

	want := env.readableNames(t)
	if got := facetRepoNames(out); !equalNames(got, want) {
		t.Fatalf("facet 只应统计可读仓库：\n逐仓库判定的可读集合 = %v\nfacet 实际 = %v", want, got)
	}
	// 定向断言：无任何 ACL 的仓库不得出现在 facet 里（也不得出现在 items 里）。
	for _, f := range out.Facets {
		if f.RepoName == env.hiddenName {
			t.Fatalf("facet 泄漏了不可读仓库 %s（count=%d）", f.RepoName, f.Count)
		}
	}
	for _, name := range itemRepoNames(out) {
		if name == env.hiddenName {
			t.Fatalf("items 泄漏了不可读仓库 %s", name)
		}
	}
	// items 必须落在 facet 集合内（facet 覆盖的是「可读且命中」的仓库，是 items 的超集）。
	facets := facetRepoNames(out)
	for _, name := range itemRepoNames(out) {
		found := false
		for _, f := range facets {
			if f == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("items 里的仓库 %s 不在 facet 集合 %v 内", name, facets)
		}
	}
	// 每个可读仓库各一条命中：计数口径与可读范围对齐（facet 条数不会因越权而多出来）。
	if len(out.Facets) != len(want) {
		t.Fatalf("facet 条数 = %d，期望等于可读仓库数 %d", len(out.Facets), len(want))
	}
	for _, f := range out.Facets {
		if f.Count != 1 {
			t.Fatalf("facet %s 的命中数 = %d，期望 1", f.RepoName, f.Count)
		}
	}
}

// TestSearchExpressionsCannotBypassReadAcl 钉住「表达式收窄不能扩大权限」：
// repo: / -repo: / format: 只在可读集合**之内**挑选，指向不可读仓库时必须得到空结果。
func TestSearchExpressionsCannotBypassReadAcl(t *testing.T) {
	env := newSearchAuthzEnv(t)
	readable := env.readableNames(t)
	// 排除公开仓库后的可读集合：用于 -repo: 用例的期望值（收窄只能让它更小）。
	withoutPublic := make([]string, 0, len(readable))
	for _, name := range readable {
		if name != env.publicName {
			withoutPublic = append(withoutPublic, name)
		}
	}

	cases := []struct {
		name       string
		query      string
		wantItems  []string
		wantFacets []string
	}{
		{
			// 正向 repo: 只作用于 items；facet 刻意忽略它（钻取切换需要在聚合条上看到
			// 其它仓库的命中数），因此这里是「items 空、facet 仍是可读全集」。
			// 关键在于 facet 里**没有** facx-hidden：不可读仓库既不贡献条目也不贡献计数。
			name:       "repo: 指向不可读仓库",
			query:      "facx repo:" + env.hiddenName,
			wantItems:  []string{},
			wantFacets: readable,
		},
		{
			name:       "format: 指向仅不可读仓库使用的格式",
			query:      "facx format:" + env.hiddenFmt,
			wantItems:  []string{},
			wantFacets: []string{},
		},
		{
			// 表达式里混入不可读仓库只应保留可读项；facet 仍覆盖全部可读仓库（钻取切换需要）。
			name:       "repo: 混入不可读仓库只保留可读项",
			query:      "facx repo:" + env.hiddenName + " repo:" + env.publicName,
			wantItems:  []string{env.publicName},
			wantFacets: readable,
		},
		{
			// 负向收窄不涉及越权，但同样是「只能在可读集合内做减法」，一并锁住。
			name:       "-repo: 排除可读仓库只缩小范围",
			query:      "facx -repo:" + env.publicName,
			wantItems:  withoutPublic,
			wantFacets: withoutPublic,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := env.searchAsMember(t, tc.query)
			if got := itemRepoNames(out); !equalNames(got, tc.wantItems) {
				t.Fatalf("items 仓库集合不符：\n期望 = %v\n实际 = %v", tc.wantItems, got)
			}
			if got := facetRepoNames(out); !equalNames(got, tc.wantFacets) {
				t.Fatalf("facet 仓库集合不符：\n期望 = %v\n实际 = %v", tc.wantFacets, got)
			}
			// 任何形态下都不得出现不可读仓库——包括「表达式恰好命中它」的情形。
			for _, name := range append(itemRepoNames(out), facetRepoNames(out)...) {
				if name == env.hiddenName {
					t.Fatalf("查询 %q 越权命中了不可读仓库 %s", tc.query, name)
				}
				inReadable := false
				for _, r := range readable {
					if r == name {
						inReadable = true
						break
					}
				}
				if !inReadable {
					t.Fatalf("查询 %q 的结果超出可读集合：%s（可读 = %v）", tc.query, name, readable)
				}
			}
			// 空集情形下 total 也必须是 0：不得出现「total 有数、items 为空」的信息泄漏形状。
			if len(tc.wantItems) == 0 && out.Total != 0 {
				t.Fatalf("查询 %q 的 total = %d，期望 0（空结果不得留下其它仓库的命中痕迹）", tc.query, out.Total)
			}
		})
	}
}
