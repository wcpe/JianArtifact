package domain_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 FR-153（搜索建议）在**领域层**的口径，逐条对应规格 §3.8 与 §5 的验收项：
//
//  1. 空 q / 纯空白 → 空候选且不报错（高频被动调用，空输入是常态）；
//  2. limit 夹取到 [1, 20]，缺省与非正数回落 10，超上限夹到 20，一律不报错；
//  3. 两类候选（package / repository）都能产出，且仓库名优先占位；
//  4. 权限硬约束：无 read 权限仓库下的候选一律不出现；可读集合为空返回空候选而不是错误
//     （API 层因此是 200 而非 403，见 internal/api/search_suggest_handlers_test.go）；
//  5. 稳定序 + 去重：同一输入多次调用顺序一致，同一字符串只出现一次；
//  6. 超时归一为 ErrSearchTimeout（与搜索主链路共享同一套预算语义）。

// suggestValues 抽出候选值，便于按整表比较顺序。
func suggestValues(items []domain.SearchSuggestion) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Value)
	}
	return out
}

// TestSuggestEmptyQuery 覆盖空输入：空串与纯空白都返回空候选，且不产生任何错误。
func TestSuggestEmptyQuery(t *testing.T) {
	svc, repos, _, _, _, assets, _ := newSearchSvcEnv(t)
	repoID, err := repos.Create("suggest-empty", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := assets.Upsert(repoID, "libs/anything-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}

	for _, q := range []string{"", "   ", "\t\n "} {
		items, err := svc.Suggest(context.Background(), q, 0, true, 0)
		if err != nil {
			t.Fatalf("空输入 %q 不应报错，得 %v", q, err)
		}
		if len(items) != 0 {
			t.Fatalf("空输入 %q 应返回空候选，得 %v", q, suggestValues(items))
		}
	}
}

// TestSuggestLimitClamp 钉住 limit 的夹取口径（缺省 10 / 负数回落缺省 / 超上限 20 / 下限 1）。
func TestSuggestLimitClamp(t *testing.T) {
	svc, repos, _, _, _, assets, _, db := newSearchSvcEnvWithDB(t)
	// 先标记索引就绪，使随后写入的资产经在线同步进入 FTS（覆盖「索引就绪」这条路径）。
	if err := repository.NewSearchIndexStateRepo(db).MarkCompleted(); err != nil {
		t.Fatalf("标记索引完成：%v", err)
	}
	repoID, err := repos.Create("suggest-limit", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 25 个互不相同的包名（maven 坐标），确保候选数大于上限 20。
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("limitpkg%02d", i)
		path := fmt.Sprintf("org/demo/%s/1.0.0/%s-1.0.0.jar", name, name)
		if err := assets.Upsert(repoID, path, "h", 1, "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("写入资产：%v", err)
		}
	}

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "缺省与非正数回落默认 10", limit: 0, want: 10},
		{name: "负数同样回落默认 10", limit: -3, want: 10},
		{name: "超上限夹到 20", limit: 1000, want: 20},
		{name: "下限 1 原样生效", limit: 1, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := svc.Suggest(context.Background(), "limitpkg", 0, true, tc.limit)
			if err != nil {
				t.Fatalf("建议查询失败：%v", err)
			}
			if len(items) != tc.want {
				t.Fatalf("limit=%d 时候选数 = %d，期望 %d（%v）", tc.limit, len(items), tc.want, suggestValues(items))
			}
		})
	}
}

// TestSuggestCandidateTypes 覆盖两类候选的来源：
//   - 仓库名：repository.name 的**子串**匹配（ASCII 大小写不敏感，对齐 SQLite LIKE 的默认口径）；
//   - 包名：FTS 命中行的 path 经 FR-150 的包坐标解析器导出。
//
// 仓库名优先占位，包名占剩余名额；两类合计不超过 limit。
func TestSuggestCandidateTypes(t *testing.T) {
	svc, repos, _, _, _, assets, _, db := newSearchSvcEnvWithDB(t)
	if err := repository.NewSearchIndexStateRepo(db).MarkCompleted(); err != nil {
		t.Fatalf("标记索引完成：%v", err)
	}
	mavenRepo, err := repos.Create("SugRepo-Alpha", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// Maven 坐标 com.example:alphapkg（路径里出现的是 com/example/alphapkg/...）。
	if err := assets.Upsert(mavenRepo, "com/example/alphapkg/1.0.0/alphapkg-1.0.0.jar", "h", 1,
		"application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}

	// 1) 前缀命中仓库名（ASCII 大小写不敏感：输入 sug 命中 SugRepo-Alpha）。
	items, err := svc.Suggest(context.Background(), " sug ", 0, true, 10)
	if err != nil {
		t.Fatalf("建议查询失败：%v", err)
	}
	if len(items) != 1 || items[0].Type != domain.SuggestTypeRepository || items[0].Value != "SugRepo-Alpha" {
		t.Fatalf("仓库名候选不符：%+v", items)
	}

	// 2) 包含词命中包名：候选值取 artifact（不含 `:`），可直接作为搜索词命中该包。
	items, err = svc.Suggest(context.Background(), "alphapkg", 0, true, 10)
	if err != nil {
		t.Fatalf("建议查询失败：%v", err)
	}
	if len(items) != 1 || items[0].Type != domain.SuggestTypePackage || items[0].Value != "alphapkg" {
		t.Fatalf("包名候选不符：%+v", items)
	}

	// 3) 仓库名是**子串**匹配：输入 repo 命中 SugRepo-Alpha（不是它的前缀，但被它包含）。
	//    这条钉住子串语义，防止退回前缀匹配（那样 `rele` 就找不到 `maven-releases`）。
	items, err = svc.Suggest(context.Background(), "repo", 0, true, 10)
	if err != nil {
		t.Fatalf("建议查询失败：%v", err)
	}
	if len(items) != 1 || items[0].Type != domain.SuggestTypeRepository || items[0].Value != "SugRepo-Alpha" {
		t.Fatalf("输入 repo 应子串命中 SugRepo-Alpha，得 %+v", items)
	}

	// 4) 两类同时命中时仓库名优先占位，合计不超过 limit。
	//    "alpha" 同时命中仓库名（含 alpha）与包名 alphapkg，limit=1 时应留下仓库名。
	items, err = svc.Suggest(context.Background(), "alpha", 0, true, 1)
	if err != nil {
		t.Fatalf("建议查询失败：%v", err)
	}
	if len(items) != 1 || items[0].Type != domain.SuggestTypeRepository || items[0].Value != "SugRepo-Alpha" {
		t.Fatalf("limit=1 时应保留优先占位的仓库名候选，得 %+v", items)
	}

	// 5) 只有排除词 / 限定词时没有包名候选（没有正向候选驱动条件）。
	for _, q := range []string{"-sources", "ext:jar", "repo:SugRepo-Alpha"} {
		items, err = svc.Suggest(context.Background(), q, 0, true, 10)
		if err != nil {
			t.Fatalf("输入 %q 不应报错：%v", q, err)
		}
		for _, item := range items {
			if item.Type == domain.SuggestTypePackage {
				t.Fatalf("输入 %q 不应产出包名候选，得 %+v", q, items)
			}
		}
	}
}

// TestSuggestPermissionScope 钉住权限硬约束（规格 §3.8 / §5 验收 11）：
// 非管理员只能看到「public ∪ 被授 read」的仓库下的候选；管理员搜全部；可读集合为空
// 返回空候选而不是错误（403 会泄漏「存在你看不到的仓库」）。
func TestSuggestPermissionScope(t *testing.T) {
	svc, repos, acls, users, _, assets, settings := newSearchSvcEnv(t)
	if err := settings.SetAnonymousAccessEnabled(true); err != nil {
		t.Fatalf("开启匿名访问：%v", err)
	}
	pubRepo, err := repos.Create("pubrepo", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	hiddenRepo, err := repos.Create("hiddenrepo", "maven", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	grantedRepo, err := repos.Create("grantedrepo", "maven", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	corpus := map[int64][]string{
		pubRepo:     {"org/demo/pubpkg/1.0.0/pubpkg-1.0.0.jar"},
		hiddenRepo:  {"org/demo/hiddenpkg/1.0.0/hiddenpkg-1.0.0.jar"},
		grantedRepo: {"org/demo/grantedpkg/1.0.0/grantedpkg-1.0.0.jar"},
	}
	for repoID, paths := range corpus {
		for _, path := range paths {
			if err := assets.Upsert(repoID, path, "h", 1, "application/java-archive", "s", "m"); err != nil {
				t.Fatalf("写入资产：%v", err)
			}
		}
	}
	userID, err := users.Create("suggest-user", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	// grantedRepo 授 read；hiddenRepo 刻意不授权。
	if err := acls.Replace(grantedRepo, []repository.Acl{{SubjectID: userID, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}

	// 断言口径：期望可见的包名必须出现、不可见的必须不出现，仓库名同理。
	check := func(t *testing.T, subjectID int64, isAdmin bool, wantVisible, wantHidden []string) {
		t.Helper()
		items, err := svc.Suggest(context.Background(), "", subjectID, isAdmin, 0)
		if err != nil {
			// 空 q 走空返回分支，这里只是先确认不报错；真正的查询在下面。
			t.Fatalf("空输入不应报错：%v", err)
		}
		if len(items) != 0 {
			t.Fatalf("空输入应返回空候选，得 %v", suggestValues(items))
		}
		// 逐词查询：每个包名/仓库名都要单独确认可见性，避免一个宽泛查询掩盖越权项。
		for _, value := range append(append([]string{}, wantVisible...), wantHidden...) {
			got, err := svc.Suggest(context.Background(), value, subjectID, isAdmin, 20)
			if err != nil {
				t.Fatalf("查询 %q 失败：%v", value, err)
			}
			visible := false
			for _, item := range got {
				if strings.EqualFold(item.Value, value) {
					visible = true
				}
			}
			want := false
			for _, v := range wantVisible {
				if strings.EqualFold(v, value) {
					want = true
				}
			}
			if visible != want {
				t.Fatalf("%q 的可见性 = %v，期望 %v（候选：%v）", value, visible, want, suggestValues(got))
			}
		}
	}

	t.Run("普通用户：public 与被授 read 可见，无授权仓库不可见", func(t *testing.T) {
		check(t, userID, false, []string{"pubpkg", "grantedpkg", "pubrepo", "grantedrepo"}, []string{"hiddenpkg", "hiddenrepo"})
	})

	t.Run("匿名：public 可见，未授权私有仓库不可见", func(t *testing.T) {
		check(t, 0, false, []string{"pubpkg", "pubrepo"}, []string{"hiddenpkg", "grantedpkg", "hiddenrepo", "grantedrepo"})
	})

	t.Run("管理员：全部可见", func(t *testing.T) {
		check(t, userID, true,
			[]string{"pubpkg", "grantedpkg", "hiddenpkg", "pubrepo", "grantedrepo", "hiddenrepo"}, nil)
	})

	// 可读集合为空：全部仓库都是私有且主体没有任何授权 → 空候选、无错误（不是 403）。
	t.Run("可读集合为空返回空候选而非错误", func(t *testing.T) {
		stranger, err := users.Create("suggest-stranger", "hash", "user")
		if err != nil {
			t.Fatalf("建用户：%v", err)
		}
		// 用一个只有私有仓库的库更直观：把公开仓库临时改为私有（读路径每次现算，不缓存）。
		if err := repos.UpdateVisibility("pubrepo", "private"); err != nil {
			t.Fatalf("改可见性：%v", err)
		}
		defer func() {
			if err := repos.UpdateVisibility("pubrepo", "public"); err != nil {
				t.Fatalf("恢复可见性：%v", err)
			}
		}()
		items, err := svc.Suggest(context.Background(), "pubpkg", stranger, false, 10)
		if err != nil {
			t.Fatalf("可读集合为空时不应报错（否则 API 会 403 并泄漏存在性），得 %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("可读集合为空时应返回空候选，得 %v", suggestValues(items))
		}
	})
}

// TestSuggestStableOrderAndDedup 钉住稳定序与去重（规格 §3.8 / §5 验收 12）：
//   - 同一输入多次调用，候选顺序完全一致（不得依赖 FTS 回表顺序）；
//   - 同一字符串只出现一次（同包跨仓库 / 与仓库名同名）；
//   - 顺序为「候选值长度升序 → 字典序升序」。
func TestSuggestStableOrderAndDedup(t *testing.T) {
	svc, repos, _, _, _, assets, _, db := newSearchSvcEnvWithDB(t)
	if err := repository.NewSearchIndexStateRepo(db).MarkCompleted(); err != nil {
		t.Fatalf("标记索引完成：%v", err)
	}
	repoA, err := repos.Create("stable-a", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	repoB, err := repos.Create("stable-b", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// dup-pkg 在两个仓库各有一份：必须只出现一次。
	// 其余包名覆盖不同长度与相同长度下的字典序。
	corpus := []string{
		"org/demo/dup-pkg/1.0.0/dup-pkg-1.0.0.jar",
		"org/demo/aa-pkg/1.0.0/aa-pkg-1.0.0.jar",
		"org/demo/bb-longer-pkg/1.0.0/bb-longer-pkg-1.0.0.jar",
		"org/demo/cc-pkg/1.0.0/cc-pkg-1.0.0.jar",
	}
	for _, path := range corpus {
		if err := assets.Upsert(repoA, path, "h", 1, "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("写入资产：%v", err)
		}
	}
	if err := assets.Upsert(repoB, "org/demo/dup-pkg/1.0.0/dup-pkg-1.0.0.jar", "h", 1,
		"application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}

	first, err := svc.Suggest(context.Background(), "pkg", 0, true, 20)
	if err != nil {
		t.Fatalf("建议查询失败：%v", err)
	}
	// 去重：dup-pkg 只出现一次。
	seen := map[string]int{}
	for _, item := range first {
		seen[item.Value]++
	}
	if seen["dup-pkg"] != 1 {
		t.Fatalf("dup-pkg 应只出现一次，得 %d 次（%v）", seen["dup-pkg"], suggestValues(first))
	}
	// 稳定序：长度升序 → 字典序升序。
	want := []string{"aa-pkg", "cc-pkg", "dup-pkg", "bb-longer-pkg"}
	if got := suggestValues(first); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("稳定序不符：得 %v，期望 %v", got, want)
	}
	// 多次调用顺序一致。
	for i := 0; i < 3; i++ {
		again, err := svc.Suggest(context.Background(), "pkg", 0, true, 20)
		if err != nil {
			t.Fatalf("第 %d 次建议查询失败：%v", i+2, err)
		}
		if strings.Join(suggestValues(again), ",") != strings.Join(want, ",") {
			t.Fatalf("第 %d 次调用顺序漂移：%v", i+2, suggestValues(again))
		}
	}

	// 索引未就绪（回填 / 重建中）时整体退回 LIKE 路径：候选必须与索引就绪时**完全一致**。
	// 这里把 completed_at 打回 NULL 来制造降级态，语义与一次真实重建相同。
	if err := repository.NewSearchIndexStateRepo(db).Reset(); err != nil {
		t.Fatalf("重置索引状态：%v", err)
	}
	degraded, err := svc.Suggest(context.Background(), "pkg", 0, true, 20)
	if err != nil {
		t.Fatalf("降级路径建议查询失败：%v", err)
	}
	if strings.Join(suggestValues(degraded), ",") != strings.Join(want, ",") {
		t.Fatalf("索引未就绪时候选应与索引就绪一致：\n  就绪：%v\n  降级：%v", want, suggestValues(degraded))
	}
}

// TestSuggestTimeout 钉住超时归一：预算耗尽必须是 errors.Is(err, domain.ErrSearchTimeout)，
// 与 /api/v1/search 同一条语义（规格 §3.7 衔接点 3 / §5 验收 8）。
func TestSuggestTimeout(t *testing.T) {
	svc, repos, _, _, _, assets, _ := newSearchSvcEnv(t)
	repoID, err := repos.Create("suggest-timeout", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := assets.Upsert(repoID, "libs/timeoutpkg-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}

	// 用「已经过期的父 deadline」而不是极小超时：后者的取消由定时器异步触发，用例会时好时坏。
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = svc.Suggest(ctx, "timeoutpkg", 0, true, 10)
	if !errors.Is(err, domain.ErrSearchTimeout) {
		t.Fatalf("期望 ErrSearchTimeout，得 %v", err)
	}
}
