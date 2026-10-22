package domain_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住  / §3.5 / §3.7 的**聚合口径**（用真实库驱动）：
//
//   - 计数自洽：每个版本的文件数、每个包的文件数、合计三者与 total 对齐；
//   - 跨仓库同坐标合并为一个包节点；
//   - 不解析坐标的裸路径降级为「无坐标」伪包且不丢条目；
//   - relevance 的核心验收顺序 1.17.12 → 1.17.12-rc1 → 1.17.120；
//   - group_by=file（缺省）与改造前逐条一致。

// aggregateEnv 是包级聚合测试的装配环境。
type aggregateEnv struct {
	svc     *domain.RepositoryService
	repos   *repository.RepoRepo
	assets  *repository.AssetRepo
	mavenID int64
	rawID   int64
	proxyID int64
}

// newAggregateEnv 建三个仓库：一个 Maven 主仓库、一个 Maven 代理仓库（用于跨仓库合并）、
// 一个 raw 仓库（用于裸路径降级）。
func newAggregateEnv(t *testing.T) *aggregateEnv {
	t.Helper()
	svc, repos, _, _, _, assets, _ := newSearchSvcEnv(t)
	env := &aggregateEnv{svc: svc, repos: repos, assets: assets}
	var err error
	if env.mavenID, err = repos.Create("agg-maven", "maven", "hosted", "public", ""); err != nil {
		t.Fatalf("建 maven 仓库：%v", err)
	}
	if env.proxyID, err = repos.Create("agg-proxy", "maven", "proxy", "public", ""); err != nil {
		t.Fatalf("建 maven 代理仓库：%v", err)
	}
	if env.rawID, err = repos.Create("agg-raw", "raw", "hosted", "public", ""); err != nil {
		t.Fatalf("建 raw 仓库：%v", err)
	}
	return env
}

// put 写入一条资产（大小与更新时间为测试固定值）。
func (e *aggregateEnv) put(t *testing.T, repoID int64, path, updatedAt string) {
	t.Helper()
	const blob, contentType, sha1, md5 = "h", "application/octet-stream", "s", "m"
	if err := e.assets.UpsertWithTime(repoID, path, blob, 100, contentType, sha1, md5, updatedAt, updatedAt); err != nil {
		t.Fatalf("写入资产 %s：%v", path, err)
	}
}

// search 以管理员身份执行一次聚合搜索。
func (e *aggregateEnv) search(t *testing.T, q, sort, order, groupBy string, limit, offset int) *domain.SearchOutput {
	t.Helper()
	out, err := e.svc.SearchAggregate(context.Background(), q, "", 0, true, sort, order, groupBy, limit, offset)
	if err != nil {
		t.Fatalf("聚合搜索 %q 失败：%v", q, err)
	}
	return out
}

// findPackage 按坐标查包节点。
func findPackage(pkgs []domain.SearchPackage, coordinate string) *domain.SearchPackage {
	for i := range pkgs {
		if pkgs[i].Coordinate == coordinate {
			return &pkgs[i]
		}
	}
	return nil
}

// TestSearchAggregateCountsAndRelevanceOrder 是本 FR 的核心验收：
// q=1.17.12 时同包的三个版本必须按 1.17.12 → 1.17.12-rc1 → 1.17.120 排列，
// 精度分别为 exact / prerelease / substring，且各级计数自洽。
func TestSearchAggregateCountsAndRelevanceOrder(t *testing.T) {
	env := newAggregateEnv(t)
	env.put(t, env.mavenID, "com/example/foo/1.17.12/foo-1.17.12.jar", "2026-10-18 09:00:00")
	env.put(t, env.mavenID, "com/example/foo/1.17.12/foo-1.17.12.pom", "2026-10-18 09:00:00")
	env.put(t, env.mavenID, "com/example/foo/1.17.12-rc1/foo-1.17.12-rc1.jar", "2026-10-17 09:00:00")
	env.put(t, env.mavenID, "com/example/foo/1.17.120/foo-1.17.120.jar", "2026-10-16 09:00:00")
	// 与关键词无关的包：不得出现在结果里。
	env.put(t, env.mavenID, "com/example/bar/2.0.0/bar-2.0.0.jar", "2026-10-18 09:00:00")

	out := env.search(t, "1.17.12", "", "asc", domain.SearchGroupByPackage, 50, 0)

	if out.Total != 4 {
		t.Fatalf("total = %d，期望 4（1.17.12 两条 + rc1 一条 + 1.17.120 一条）", out.Total)
	}
	// 计数自洽：包内文件数之和 = total（未截断时）。
	sum := 0
	for _, p := range out.Packages {
		sum += p.FileCount
	}
	if sum != out.Total {
		t.Fatalf("各包 fileCount 之和 = %d，期望等于 total=%d", sum, out.Total)
	}
	if out.PackageTotal != len(out.Packages) {
		t.Fatalf("packageTotal = %d 与 packages 长度 %d 不一致", out.PackageTotal, len(out.Packages))
	}
	if out.Aggregation.Scanned != 4 || out.Aggregation.Truncated {
		t.Fatalf("aggregation = %+v，期望 scanned=4 truncated=false", out.Aggregation)
	}

	foo := findPackage(out.Packages, "com.example:foo")
	if foo == nil {
		t.Fatalf("缺少 com.example:foo 包节点：%+v", out.Packages)
	}
	if foo.Kind != domain.PackageKindMaven {
		t.Fatalf("kind = %q，期望 maven", foo.Kind)
	}
	if foo.BestPrecision != domain.PrecisionExact {
		t.Fatalf("bestPrecision = %q，期望 exact", foo.BestPrecision)
	}
	if foo.VersionCount != 3 || foo.FileCount != 4 {
		t.Fatalf("foo 计数 = %d 版本 / %d 文件，期望 3 / 4", foo.VersionCount, foo.FileCount)
	}
	if foo.TopVersion != "1.17.12" {
		t.Fatalf("topVersion = %q，期望 1.17.12", foo.TopVersion)
	}

	// **核心验收断言**：同包三个版本的顺序与精度。
	wantVersions := []struct{ version, precision string }{
		{"1.17.12", domain.PrecisionExact},
		{"1.17.12-rc1", domain.PrecisionPrerelease},
		{"1.17.120", domain.PrecisionSubstring},
	}
	if len(foo.Versions) != len(wantVersions) {
		t.Fatalf("版本组数 = %d，期望 %d", len(foo.Versions), len(wantVersions))
	}
	versionFiles := 0
	for i, want := range wantVersions {
		got := foo.Versions[i]
		if got.Version != want.version || got.Precision != want.precision {
			t.Fatalf("第 %d 个版本 = %s[%s]，期望 %s[%s]",
				i, got.Version, got.Precision, want.version, want.precision)
		}
		if got.FileCount != len(got.Files) {
			t.Fatalf("版本 %s 的 fileCount=%d 与 files 长度 %d 不一致",
				got.Version, got.FileCount, len(got.Files))
		}
		if got.Size != int64(100*len(got.Files)) {
			t.Fatalf("版本 %s 的 size=%d，期望 %d", got.Version, got.Size, 100*len(got.Files))
		}
		versionFiles += got.FileCount
	}
	if versionFiles != foo.FileCount {
		t.Fatalf("各版本 fileCount 之和 = %d，期望等于包 fileCount=%d", versionFiles, foo.FileCount)
	}
	// 组内文件排序：主构件在前（jar 优先级 0 < pom 优先级 5）。
	if jar := foo.Versions[0]; len(jar.Files) != 2 || jar.Files[0].FileKind != "jar" || jar.Files[1].FileKind != "pom" {
		t.Fatalf("1.17.12 组文件 = %+v，期望 jar 在前 pom 在后", foo.Versions[0].Files)
	}
}

// TestSearchAggregateDegradedPseudoPackage 钉住裸路径的降级口径（规格 §3.2 / 验收 §5）：
// raw 仓库里无坐标的路径进「无坐标」伪包，跳过版本层、精度 none，但条目与文件都在。
func TestSearchAggregateDegradedPseudoPackage(t *testing.T) {
	env := newAggregateEnv(t)
	env.put(t, env.rawID, "docs/a/1.17.12/notes.txt", "2026-10-18 09:00:00")
	env.put(t, env.rawID, "docs/b.txt", "2026-10-18 09:00:00")

	out := env.search(t, "docs", "", "asc", domain.SearchGroupByPackage, 50, 0)
	if out.Total != 2 {
		t.Fatalf("total = %d，期望 2", out.Total)
	}
	p := findPackage(out.Packages, "agg-raw")
	if p == nil {
		t.Fatalf("raw 仓库应有一个以仓库名为坐标的伪包：%+v", out.Packages)
	}
	if p.Kind != domain.PackageKindFile {
		t.Fatalf("伪包 kind = %q，期望 file", p.Kind)
	}
	if p.BestPrecision != domain.PrecisionNone {
		t.Fatalf("伪包 bestPrecision = %q，期望 none", p.BestPrecision)
	}
	if p.VersionCount != 0 || len(p.Versions) != 0 {
		t.Fatalf("伪包应跳过版本层：versionCount=%d versions=%d", p.VersionCount, len(p.Versions))
	}
	if p.FileCount != 2 || len(p.Files) != 2 {
		t.Fatalf("伪包文件 = %d/%d，期望 2/2（文件直挂包下）", p.FileCount, len(p.Files))
	}
	// 同一仓库只出一个伪包节点（避免每个路径都成一个节点把树冲垮）。
	pseudoCount := 0
	for _, pkg := range out.Packages {
		if pkg.Kind == domain.PackageKindFile {
			pseudoCount++
		}
	}
	if pseudoCount != 1 {
		t.Fatalf("伪包节点数 = %d，期望 1（每仓库一个）", pseudoCount)
	}
}

// TestSearchAggregateMergesAcrossRepos 钉住「包身份键不含仓库」（规格 §3.3）：
// 同一坐标在 hosted 与 proxy 各出现一次时合并为一个包节点，两个仓库都记在 repositories 里。
func TestSearchAggregateMergesAcrossRepos(t *testing.T) {
	env := newAggregateEnv(t)
	env.put(t, env.mavenID, "com/example/foo/1.0.0/foo-1.0.0.jar", "2026-10-18 09:00:00")
	env.put(t, env.proxyID, "com/example/foo/1.0.0/foo-1.0.0.jar", "2026-10-18 10:00:00")

	out := env.search(t, "foo", "", "asc", domain.SearchGroupByPackage, 50, 0)
	if out.Total != 2 || out.PackageTotal != 1 {
		t.Fatalf("total/packageTotal = %d/%d，期望 2/1（同坐标合并）", out.Total, out.PackageTotal)
	}
	p := out.Packages[0]
	if len(p.Repositories) != 2 {
		t.Fatalf("repositories = %v，期望两个仓库", p.Repositories)
	}
	if p.FileCount != 2 || p.VersionCount != 1 {
		t.Fatalf("计数 = %d 文件 / %d 版本，期望 2 / 1", p.FileCount, p.VersionCount)
	}
	// 组内文件顺序：两条记录的 path 完全相同（同坐标同文件名），只能由末位的仓库名次级键决定
	// 顺序。少了它，比较器对这两条双向返回 false，顺序会随输入到达顺序（SQL 对同 path 不保证
	// 相对顺序）与 sort.Slice 的非稳定性抖动。
	files := p.Versions[0].Files
	if len(files) != 2 {
		t.Fatalf("版本组内文件数 = %d，期望 2", len(files))
	}
	if files[0].Repository != "agg-maven" || files[1].Repository != "agg-proxy" {
		t.Fatalf("同 path 的跨仓库文件应按仓库名升序（agg-maven, agg-proxy），得 %s, %s",
			files[0].Repository, files[1].Repository)
	}
}

// TestSearchAggregateGroupByFileMatchesSearchAssets 是**回归保护**：
// group_by=file（缺省）下聚合入口必须与改造前的 SearchAssets 逐条一致。
func TestSearchAggregateGroupByFileMatchesSearchAssets(t *testing.T) {
	env := newAggregateEnv(t)
	env.put(t, env.mavenID, "com/example/foo/1.17.12/foo-1.17.12.jar", "2026-10-18 09:00:00")
	env.put(t, env.mavenID, "com/example/foo/1.17.120/foo-1.17.120.jar", "2026-10-17 09:00:00")
	env.put(t, env.rawID, "docs/1.17.12/notes.txt", "2026-10-16 09:00:00")

	queries := []string{"1.17.12", "foo", "version:1.17.12", "docs ext:txt"}
	orders := []struct{ sort, order string }{
		{"", "asc"}, {"", "desc"}, {"updated", "desc"}, {"size", "asc"}, {"name", "asc"},
	}
	for _, q := range queries {
		for _, o := range orders {
			want, err := env.svc.SearchAssets(context.Background(), q, "", 0, true, o.sort, o.order, 20, 0)
			if err != nil {
				t.Fatalf("SearchAssets(%q) 失败：%v", q, err)
			}
			got := env.search(t, q, o.sort, o.order, domain.SearchGroupByFile, 20, 0)
			if got.Total != want.Total || len(got.Items) != len(want.Items) {
				t.Fatalf("q=%q sort=%q order=%q：total/items = %d/%d，期望 %d/%d",
					q, o.sort, o.order, got.Total, len(got.Items), want.Total, len(want.Items))
			}
			for i := range want.Items {
				if got.Items[i].RepoName != want.Items[i].RepoName || got.Items[i].Asset.Path != want.Items[i].Asset.Path {
					t.Fatalf("q=%q 第 %d 条 = %s/%s，期望 %s/%s", q, i,
						got.Items[i].RepoName, got.Items[i].Asset.Path,
						want.Items[i].RepoName, want.Items[i].Asset.Path)
				}
			}
			for i := range want.Facets {
				if got.Facets[i] != want.Facets[i] {
					t.Fatalf("q=%q facet 第 %d 条 = %+v，期望 %+v", q, i, got.Facets[i], want.Facets[i])
				}
			}
			// file 模式不填充聚合字段。
			if got.Packages != nil || got.PackageTotal != 0 || got.Aggregation.Scanned != 0 {
				t.Fatalf("q=%q：file 模式不应填充聚合字段（%+v）", q, got.Aggregation)
			}
		}
	}
}

// TestSearchAggregateNoVersionSignal 钉住 §5 的两条"降级正确"验收：
//
//  1. 关键词命中文件名但不命中版本段时，精度必须是 none——**不能误报 substring**，
//     因为精度只由版本段决定（§3.4 末尾的理由：Maven 文件名里同样带版本，
//     若一并计算，`1.17.120` 目录下的文件就不会被判为子串）。
//  2. 完全没有版本信号时，relevance 退化为 updated desc，与改造前的默认排序同向。
func TestSearchAggregateNoVersionSignal(t *testing.T) {
	env := newAggregateEnv(t)
	env.put(t, env.mavenID, "com/example/alpha/1.0.0/alpha-1.0.0.jar", "2026-10-10 09:00:00")
	env.put(t, env.mavenID, "com/example/beta/2.0.0/beta-2.0.0.jar", "2026-10-12 09:00:00")

	// 关键词 "alpha" 只出现在包名与文件名里，版本段是 1.0.0。
	out := env.search(t, "alpha", "", "asc", domain.SearchGroupByPackage, 50, 0)
	if out.PackageTotal != 1 {
		t.Fatalf("packageTotal = %d，期望 1", out.PackageTotal)
	}
	if got := out.Packages[0].BestPrecision; got != domain.PrecisionNone {
		t.Fatalf("命中包名/文件名时包精度 = %q，期望 none（不得误报 substring）", got)
	}
	if got := out.Packages[0].Versions[0].Precision; got != domain.PrecisionNone {
		t.Fatalf("版本精度 = %q，期望 none（精度只看版本段）", got)
	}

	// 关键词 "jar" 命中两边的文件名，但两者都没有版本信号 → 按 updated desc 排列。
	out = env.search(t, "jar", "", "asc", domain.SearchGroupByPackage, 50, 0)
	if out.PackageTotal != 2 {
		t.Fatalf("packageTotal = %d，期望 2", out.PackageTotal)
	}
	for _, p := range out.Packages {
		if p.BestPrecision != domain.PrecisionNone {
			t.Fatalf("包 %s 精度 = %q，期望 none", p.Coordinate, p.BestPrecision)
		}
	}
	if out.Packages[0].Coordinate != "com.example:beta" || out.Packages[1].Coordinate != "com.example:alpha" {
		t.Fatalf("顺序 = [%s %s]，期望 beta 在前（更新更晚，等价于旧的 updated desc）",
			out.Packages[0].Coordinate, out.Packages[1].Coordinate)
	}
}

// TestSearchAggregatePaginationKeepsTotal 钉住「分页只作用于 items，packageTotal 与 total
// 不受分页影响」（规格 §3.8）。
func TestSearchAggregatePaginationKeepsTotal(t *testing.T) {
	env := newAggregateEnv(t)
	for i, path := range []string{
		"com/example/foo/1.0.0/foo-1.0.0.jar",
		"com/example/foo/1.0.0/foo-1.0.0.pom",
		"com/example/foo/2.0.0/foo-2.0.0.jar",
	} {
		env.put(t, env.mavenID, path, fmt.Sprintf("2026-10-1%d 09:00:00", i))
	}
	page1 := env.search(t, "foo", "", "asc", domain.SearchGroupByPackage, 2, 0)
	page2 := env.search(t, "foo", "", "asc", domain.SearchGroupByPackage, 2, 2)
	if page1.Total != 3 || page1.PackageTotal != 1 {
		t.Fatalf("第 1 页 total/packageTotal = %d/%d，期望 3/1", page1.Total, page1.PackageTotal)
	}
	if page2.Total != 3 || page2.PackageTotal != 1 {
		t.Fatalf("第 2 页 total/packageTotal = %d/%d，期望 3/1（不随分页变化）", page2.Total, page2.PackageTotal)
	}
	if len(page1.Items) != 2 || len(page2.Items) != 1 {
		t.Fatalf("分页 items = %d/%d，期望 2/1", len(page1.Items), len(page2.Items))
	}
	// 聚合看到的是全量候选行，与分页无关。
	if page1.Aggregation.Scanned != 3 {
		t.Fatalf("第 1 页 scanned = %d，期望 3（聚合不受分页影响）", page1.Aggregation.Scanned)
	}
}

// TestSearchAggregateResponseIsInternallyConsistent 钉住响应**内部自洽**：
// 同一个响应里的 total、items、facets、packages、aggregation 必须是同一份数据的四种视图。
//
// 回归的是「四条查询各开一份快照」导致的错位：共用 WHERE 只保证条件相同，不保证数据版本
// 相同。最坏情形是 COUNT 之后命中的资产被并发删除 → total=1、items 非空，而聚合扫描看到
// 0 行 → packages=[] 且 truncated=false。搜索页只渲染 packages，用户看到「无结果」，
// 响应自身却前后矛盾（total 说有 1 条，包列表说有 0 条）。
//
// 这里不靠并发时序（那样只能概率性复现），而是直接断言「同一份快照」蕴含的不变式：
//   - 未截断时，聚合扫描行数 = total（扫描与计数看到同一行集）；
//   - 未截断时，各包 fileCount 之和 = total（包视图不丢行、也不重复计数）；
//   - items 的每一条都必须出现在聚合出的包里（分页是扫描结果的子集）；
//   - facets 的计数之和 = total（facet 与计数同源）。
//
// 快照数为 1 的事实由仓储层的 TestSearchAggregateByFilterUsesSingleSnapshot 直接断言，
// 两者合起来才完整：仓储层证明「只开一份快照」，这里证明「一份快照的结果自洽」。
func TestSearchAggregateResponseIsInternallyConsistent(t *testing.T) {
	env := newAggregateEnv(t)
	env.put(t, env.mavenID, "com/example/foo/1.0.0/foo-1.0.0.jar", "2026-10-18 09:00:00")
	env.put(t, env.mavenID, "com/example/foo/1.0.0/foo-1.0.0.pom", "2026-10-18 09:00:00")
	env.put(t, env.mavenID, "com/example/foo/2.0.0/foo-2.0.0.jar", "2026-10-17 09:00:00")
	env.put(t, env.proxyID, "com/example/bar/1.5.0/bar-1.5.0.jar", "2026-10-16 09:00:00")
	env.put(t, env.rawID, "docs/foo-notes.txt", "2026-10-15 09:00:00")

	cases := []struct {
		name          string
		query         string
		limit, offset int
	}{
		{name: "全量", query: "foo", limit: 50, offset: 0},
		{name: "分页第 1 页", query: "foo", limit: 2, offset: 0},
		{name: "分页越界", query: "foo", limit: 2, offset: 10},
		{name: "跨仓库关键词", query: "com/example", limit: 50, offset: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := env.search(t, tc.query, "", "asc", domain.SearchGroupByPackage, tc.limit, tc.offset)
			if out.Aggregation.Truncated {
				t.Fatalf("语料远小于扫描预算，不应截断：%+v", out.Aggregation)
			}
			if out.Aggregation.Scanned != out.Total {
				t.Fatalf("扫描行数 = %d，期望等于 total=%d（两者必须来自同一份快照）",
					out.Aggregation.Scanned, out.Total)
			}
			packageFiles := 0
			for _, p := range out.Packages {
				packageFiles += p.FileCount
			}
			if packageFiles != out.Total {
				t.Fatalf("各包 fileCount 之和 = %d，期望等于 total=%d（包视图不得丢行或重复计数）",
					packageFiles, out.Total)
			}
			if out.PackageTotal != len(out.Packages) {
				t.Fatalf("packageTotal = %d 与 packages 长度 %d 不一致", out.PackageTotal, len(out.Packages))
			}
			// 分页条目必须是聚合扫描结果的子集：出现了不在任何包里的条目，就说明
			// 「items 与 packages 来自两份不同快照」。
			inPackages := map[string]bool{}
			for _, p := range out.Packages {
				for _, v := range p.Versions {
					for _, f := range v.Files {
						inPackages[f.Repository+"|"+f.Path] = true
					}
				}
				for _, f := range p.Files {
					inPackages[f.Repository+"|"+f.Path] = true
				}
			}
			for _, item := range out.Items {
				key := item.RepoName + "|" + item.Asset.Path
				if !inPackages[key] {
					t.Fatalf("分页条目 %s 不在任何聚合包里（items 与 packages 不同源）：%+v",
						key, out.Packages)
				}
			}
			facetSum := 0
			for _, f := range out.Facets {
				facetSum += f.Count
			}
			if facetSum != out.Total {
				t.Fatalf("facets 计数之和 = %d，期望等于 total=%d（facet 与计数必须同源）",
					facetSum, out.Total)
			}
		})
	}
}

// TestSearchAggregateTruncatesAtBudget 钉住扫描预算（规格 §3.7）：
// 超过 maxAggregateRows 时 truncated=true、各计数变下界，而 total 仍是精确 COUNT(*)。
func TestSearchAggregateTruncatesAtBudget(t *testing.T) {
	svc, repos, _, _, _, _, _, db := newSearchSvcEnvWithDB(t)
	repoID, err := repos.Create("agg-bulk", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 直接批量 INSERT 而不走 Upsert：本次只验证聚合预算，FTS 索引未就绪
	// （search_index_state.completed_at 为 NULL）时搜索走 LIKE 路径，不读索引。
	const bulk = 5001 // maxAggregateRows + 1
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	stmt, err := tx.Preparex(`INSERT INTO asset
		(repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at)
		VALUES (?, ?, 'h', 1, 'text/plain', 's', 'm', '2026-10-18 09:00:00', '2026-10-18 09:00:00')`)
	if err != nil {
		t.Fatalf("准备语句：%v", err)
	}
	for i := 0; i < bulk; i++ {
		// 序号补零成 5 位，让 path 的字典序与数值序一致。
		if _, err := stmt.Exec(repoID, fmt.Sprintf("bulk/pkg/1.0.0/f-%05d.txt", i)); err != nil {
			t.Fatalf("批量写入第 %d 条：%v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交：%v", err)
	}

	out, err := svc.SearchAggregate(context.Background(), "bulk", "", 0, true, "", "asc", domain.SearchGroupByPackage, 20, 0)
	if err != nil {
		t.Fatalf("聚合搜索失败：%v", err)
	}
	if out.Total != bulk {
		t.Fatalf("total = %d，期望 %d（不因聚合截断而失真）", out.Total, bulk)
	}
	if !out.Aggregation.Truncated {
		t.Fatal("超过扫描预算时必须标记 truncated")
	}
	if out.Aggregation.Scanned != 5000 {
		t.Fatalf("scanned = %d，期望 5000（maxAggregateRows）", out.Aggregation.Scanned)
	}
	if len(out.Packages) != 1 {
		t.Fatalf("包节点数 = %d，期望 1", len(out.Packages))
	}
	if got := out.Packages[0].FileCount; got != 5000 {
		t.Fatalf("截断后 fileCount = %d，期望 5000（total 的下界）", got)
	}
}
