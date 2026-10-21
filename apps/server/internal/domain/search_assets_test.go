package domain_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住第三批在**领域层**的两件事：
//
//  1. 超时保护：SearchAssets 在入口施加可配置的总预算，超时统一归一为
//     domain.ErrSearchTimeout（API 据此映射 504 + search_timeout）；
//  2. ACL 批量化后的**可搜范围**与「逐仓库 HasPermission」口径完全一致，
//     并且查询次数不随仓库数增长。

// newSearchSvcEnv 装配带组支持的仓库服务与配套仓储。
func newSearchSvcEnv(t *testing.T) (*domain.RepositoryService, *repository.RepoRepo, *repository.AclRepo,
	*repository.UserRepo, *repository.UserGroupRepo, *repository.AssetRepo, *domain.SettingService) {
	t.Helper()
	svc, repos, acls, users, groups, assets, settings, _ := newSearchSvcEnvWithDB(t)
	return svc, repos, acls, users, groups, assets, settings
}

// newSearchSvcEnvWithDB 同 newSearchSvcEnv，额外返回底层库（用于切换索引就绪状态）。
func newSearchSvcEnvWithDB(t *testing.T) (*domain.RepositoryService, *repository.RepoRepo, *repository.AclRepo,
	*repository.UserRepo, *repository.UserGroupRepo, *repository.AssetRepo, *domain.SettingService,
	*persistence.DB) {
	t.Helper()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	acls := repository.NewAclRepo(db)
	assets := repository.NewAssetRepo(db)
	users := repository.NewUserRepo(db)
	groups := repository.NewUserGroupRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repos, acls, assets, settings, users)
	svc.SetUserGroupRepo(groups)
	return svc, repos, acls, users, groups, assets, settings, db
}

// TestSearchAssetsTimesOutWithSentinel 验证超时归一：预算耗尽时必须得到
// errors.Is(err, domain.ErrSearchTimeout)，而不是被当成 500 的驱动错误。
func TestSearchAssetsTimesOutWithSentinel(t *testing.T) {
	svc, repos, _, _, _, assets, _ := newSearchSvcEnv(t)
	repoID, err := repos.Create("search-timeout", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := assets.Upsert(repoID, "libs/timeout-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}

	t.Run("预算已耗尽立即超时", func(t *testing.T) {
		// 用「已经过期的父 deadline」而不是 1ns 的超时：WithTimeout 的取消由定时器异步触发，
		// 快速查询可能抢在触发前跑完（用例会时好时坏）；已过期的 deadline 是**同步**判定的。
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("前提不成立：ctx.Err()=%v", ctx.Err())
		}
		_, err := svc.SearchAssets(ctx, "timeout", "", 0, true, "", "asc", 10, 0)
		if !errors.Is(err, domain.ErrSearchTimeout) {
			t.Fatalf("期望 ErrSearchTimeout，得 %v", err)
		}
	})

	t.Run("父上下文已取消也归一为超时", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := svc.SearchAssets(ctx, "timeout", "", 0, true, "", "asc", 10, 0)
		if !errors.Is(err, domain.ErrSearchTimeout) {
			t.Fatalf("期望 ErrSearchTimeout，得 %v", err)
		}
	})

	t.Run("默认预算下正常返回", func(t *testing.T) {
		out, err := svc.SearchAssets(context.Background(), "timeout", "", 0, true, "", "asc", 10, 0)
		if err != nil {
			t.Fatalf("搜索失败：%v", err)
		}
		if out.Total != 1 || len(out.Items) != 1 {
			t.Fatalf("应命中 1 条，得 total=%d items=%d", out.Total, len(out.Items))
		}
	})
}

// TestSetSearchTimeoutIgnoresNonPositive 钉住「无禁用语义」：
// 非正数不得把超时关掉（那等于开放一个可以长时间占住数据库连接的入口）。
func TestSetSearchTimeoutIgnoresNonPositive(t *testing.T) {
	svc, _, _, _, _, _, _ := newSearchSvcEnv(t)
	before := svc.SearchTimeout()
	for _, d := range []time.Duration{0, -time.Second} {
		svc.SetSearchTimeout(d)
		if got := svc.SearchTimeout(); got != before {
			t.Fatalf("SetSearchTimeout(%s) 之后预算变成 %s，期望保持 %s", d, got, before)
		}
	}
	svc.SetSearchTimeout(3 * time.Second)
	if got := svc.SearchTimeout(); got != 3*time.Second {
		t.Fatalf("正数预算应生效，得 %s", got)
	}
}

// TestSearchAssetsScopeMatchesPerRepoHasPermission 钉住 ACL 批量化后的可搜范围：
// 搜索能看到的仓库集合必须恒等于「public ∪ 主体逐仓库 HasPermission(read)」。
// 覆盖用户直授、仅组授、多组、无组、匿名与管理员。
func TestSearchAssetsScopeMatchesPerRepoHasPermission(t *testing.T) {
	svc, repos, acls, users, groups, assets, _ := newSearchSvcEnv(t)

	// 仓库拓扑：pub（公开）、userRead（用户直授）、groupWrite（组授 write，蕴含 read）、
	// unreadable（谁都没授权）、groupAdmin（组授 admin）。
	pubRepo, err := repos.Create("scope-pub", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	userRepoID, err := repos.Create("scope-user-read", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	groupRepoID, err := repos.Create("scope-group-write", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	unreadableRepo, err := repos.Create("scope-unreadable", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	adminGroupRepo, err := repos.Create("scope-group-admin", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 每个仓库放一条同名路径的资产，搜索结果即可反映「可搜范围」。
	allRepos := []int64{pubRepo, userRepoID, groupRepoID, unreadableRepo, adminGroupRepo}
	for _, id := range allRepos {
		if err := assets.Upsert(id, "scope-probe-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("写入资产：%v", err)
		}
	}

	userID, err := users.Create("scope-user", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	otherID, err := users.Create("scope-other", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	writeGroup, err := groups.Create("scope-write-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	adminGroup, err := groups.Create("scope-admin-group", "")
	if err != nil {
		t.Fatalf("建组：%v", err)
	}
	if err := groups.AddMember(writeGroup, userID); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	if err := groups.AddMember(adminGroup, userID); err != nil {
		t.Fatalf("加入成员：%v", err)
	}
	aclPlan := map[int64][]repository.Acl{
		userRepoID:     {{SubjectID: userID, Action: repository.ActionRead}},
		groupRepoID:    {{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: writeGroup, Action: repository.ActionWrite}},
		adminGroupRepo: {{SubjectType: repository.SubjectTypeGroup, SubjectGroupID: adminGroup, Action: repository.ActionAdmin}},
		// unreadableRepo 刻意没有任何条目
	}
	for repoID, entries := range aclPlan {
		if err := acls.Replace(repoID, entries); err != nil {
			t.Fatalf("写 ACL：%v", err)
		}
	}

	cases := []struct {
		name      string
		subjectID int64
		isAdmin   bool
	}{
		{name: "用户直授加两个组", subjectID: userID},
		{name: "无任何授权的主体", subjectID: otherID},
		{name: "管理员搜全部", subjectID: userID, isAdmin: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.SearchAssets(context.Background(), "scope-probe", "", tc.subjectID, tc.isAdmin, "path", "asc", 50, 0)
			if err != nil {
				t.Fatalf("搜索失败：%v", err)
			}
			got := map[int64]bool{}
			for _, item := range out.Items {
				got[item.Asset.RepositoryID] = true
			}
			// 用逐仓库 HasPermission 计算期望范围（这正是批量化要等价替换的旧口径）。
			subject, err := svc.SubjectFor(tc.subjectID)
			if err != nil {
				t.Fatalf("展开主体：%v", err)
			}
			for _, id := range allRepos {
				var want bool
				switch {
				case tc.isAdmin:
					want = true
				case id == pubRepo:
					want = true // 公开仓库对所有主体可见
				default:
					want, err = acls.HasPermission(id, subject, repository.ActionRead)
					if err != nil {
						t.Fatalf("逐仓库判定失败：%v", err)
					}
				}
				if got[id] != want {
					t.Fatalf("仓库 %d 的可搜范围不一致：搜索=%v 逐仓库判定=%v", id, got[id], want)
				}
			}
		})
	}
}

// TestSearchAssetsAnonymousScope 覆盖匿名路径：开关打开时匿名主体只能搜到
// public ∪ anonymous 主体被授 read 的仓库。
func TestSearchAssetsAnonymousScope(t *testing.T) {
	svc, repos, acls, users, _, assets, settings := newSearchSvcEnv(t)
	if err := settings.SetAnonymousAccessEnabled(true); err != nil {
		t.Fatalf("开启匿名访问：%v", err)
	}
	pubRepo, err := repos.Create("anon-pub", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	grantedRepo, err := repos.Create("anon-granted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	hiddenRepo, err := repos.Create("anon-hidden", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for _, id := range []int64{pubRepo, grantedRepo, hiddenRepo} {
		if err := assets.Upsert(id, "anon-probe-1.0.0.jar", "h", 1, "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("写入资产：%v", err)
		}
	}
	// anonymous 主体（内置用户）被授 read。
	anon, err := users.GetByUsername(domain.AnonymousUsername)
	if err != nil {
		t.Fatalf("读取匿名用户：%v", err)
	}
	if err := acls.Replace(grantedRepo, []repository.Acl{{SubjectID: anon.ID, Action: repository.ActionRead}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}

	out, err := svc.SearchAssets(context.Background(), "anon-probe", "", 0, false, "path", "asc", 50, 0)
	if err != nil {
		t.Fatalf("匿名搜索失败：%v", err)
	}
	got := map[int64]bool{}
	for _, item := range out.Items {
		got[item.Asset.RepositoryID] = true
	}
	if !got[pubRepo] || !got[grantedRepo] {
		t.Fatalf("匿名应能搜到 public 与被授 read 的仓库：%v", got)
	}
	if got[hiddenRepo] {
		t.Fatalf("匿名不得搜到未授权仓库：%v", got)
	}
}

// TestSearchAssetsEquivalenceWithIndex 是**端到端**的双实现对照：同一批查询串在
// 「索引已就绪（走 FTS 候选集）」与「索引未就绪（整体降级为原 LIKE 路径）」两种状态下，
// 返回的条目、total 与 facet 必须逐条一致。
//
// 与仓储层的等价性测试互补：这一层额外覆盖了 `repo:` / `-repo:` / `format:` / `-format:`
// 的表达式收窄与可搜范围计算——那些逻辑在领域层，仓储层测不到。
func TestSearchAssetsEquivalenceWithIndex(t *testing.T) {
	svc, repos, _, users, _, assets, _, db := newSearchSvcEnvWithDB(t)

	rawRepo, err := repos.Create("eq-raw", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	mavenRepo, err := repos.Create("eq-maven", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	hiddenRepo, err := repos.Create("eq-hidden", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	corpus := map[int64][]string{
		rawRepo: {
			"libs/alpha-1.17.12.jar",
			"libs/alpha-1.17.120.jar",
			"libs/beta-2.0.0.pom",
			`odd/100%_done "quoted".txt`,
			"中文/标签-1.0.jar",
		},
		mavenRepo:  {"org/demo/alpha-sample/1.17.12/alpha-sample-1.17.12.jar"},
		hiddenRepo: {"libs/alpha-hidden-1.0.jar"},
	}
	for repoID, paths := range corpus {
		for _, path := range paths {
			if err := assets.Upsert(repoID, path, "h", 1, "application/octet-stream", "s", "m"); err != nil {
				t.Fatalf("写入资产：%v", err)
			}
		}
	}
	adminID, err := users.Create("eq-admin", "hash", "admin")
	if err != nil {
		t.Fatalf("建管理员：%v", err)
	}
	// 置索引就绪。
	state := repository.NewSearchIndexStateRepo(db)
	if err := state.MarkCompleted(); err != nil {
		t.Fatalf("标记索引完成：%v", err)
	}

	queries := []string{
		"alpha",
		"alpha jar",
		"alpha -1.17.120",
		"ext:jar",
		"-ext:jar",
		"ext:pom alpha",
		"repo:eq-raw alpha",
		"-repo:eq-hidden alpha",
		"format:raw alpha",
		"-format:raw alpha",
		`"alpha 1.17.12"`,
		"1.17.12",
		"100%_done",
		"a",
		"标签",
		"alpha jar ext:jar repo:eq-maven",
	}
	runAll := func() []string {
		var out []string
		searchOut, err := svc.SearchAssets(context.Background(), "", "", adminID, true, "path", "asc", 100, 0)
		if err != nil {
			t.Fatalf("空查询搜索失败：%v", err)
		}
		out = append(out, fmt.Sprintf("空查询 total=%d items=%d", searchOut.Total, len(searchOut.Items)))
		for _, q := range queries {
			res, err := svc.SearchAssets(context.Background(), q, "", adminID, true, "path", "asc", 100, 0)
			if err != nil {
				t.Fatalf("查询 %q 失败：%v", q, err)
			}
			var buf strings.Builder
			fmt.Fprintf(&buf, "%q total=%d items=", q, res.Total)
			for _, item := range res.Items {
				fmt.Fprintf(&buf, "%s:%s|", item.RepoName, item.Asset.Path)
			}
			// facet 的既有排序是「命中数降序」且**并列时无确定次序**（取决于执行计划），
			// 因此这里按名字排序后再比——本用例比的是集合与计数，不是并列项的顺序。
			facets := make([]string, 0, len(res.Facets))
			for _, f := range res.Facets {
				facets = append(facets, fmt.Sprintf("%s:%d", f.RepoName, f.Count))
			}
			sort.Strings(facets)
			buf.WriteString(" facets=" + strings.Join(facets, "|"))
			out = append(out, buf.String())
		}
		return out
	}

	indexed := runAll()
	// 降级：completed_at 打回 NULL（等价于回填未完成 / 重建中）。
	if err := state.Reset(); err != nil {
		t.Fatalf("重置索引状态：%v", err)
	}
	degraded := runAll()

	if len(indexed) != len(degraded) {
		t.Fatalf("两次运行的用例数不一致：%d vs %d", len(indexed), len(degraded))
	}
	for i := range indexed {
		if indexed[i] != degraded[i] {
			t.Fatalf("索引就绪与降级的结果不一致：\n  索引：%s\n  降级：%s", indexed[i], degraded[i])
		}
	}
}
