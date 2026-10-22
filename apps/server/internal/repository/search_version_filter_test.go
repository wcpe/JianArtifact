package repository

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住的 `version:` **段边界条件**：严格相等必须落在 SQL 里，
// 而不是"先松散 LIKE 再 Go 侧后置过滤"——后者会让 total 变成过滤后的计数、分页语义失真。
//
// 关键回归：`1.17.12` 不得命中 `1.17.120/` 与 `1.17.12-rc1/`，但必须命中
// `pkg/-/pkg-1.17.12.tgz`（版本不在独立路径段里的 npm tarball 形态）。

// versionFilterFixture 建一个仓库并写入版本语义语料。
type versionFilterFixture struct {
	assets *AssetRepo
	repoID int64
}

// newVersionFilterFixture 准备语料库。
//
// 语料刻意同时包含 Maven 目录形态、npm tarball 形态与"版本号只是子串"的噪声路径。
//
// **语料写完后必须把全文索引标记为就绪**：searchSnapshot 会在只读快照里复核
// search_index_state.completed_at，未就绪时一律把 UseFullTextIndex 打回 false。若夹具不标记，
// 「索引路径 vs LIKE 路径」的对照用例两边都走 LIKE，测试通过但什么也没验证（形同虚设）。
// Upsert 对新行无条件写 asset_path_fts（与就绪状态无关），因此标记之后索引内容就是完整的。
func newVersionFilterFixture(t *testing.T) *versionFilterFixture {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "search-version.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := NewRepoRepo(db)
	repoID, err := repos.Create("vf-maven", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	assets := NewAssetRepo(db)
	corpus := []string{
		"com/example/foo/1.17.12/foo-1.17.12.jar",
		"com/example/foo/1.17.12/foo-1.17.12.pom",
		"com/example/foo/1.17.12/foo-1.17.12.jar.sha1",
		"com/example/foo/1.17.120/foo-1.17.120.jar",
		"com/example/foo/1.17.12-rc1/foo-1.17.12-rc1.jar",
		"com/example/foo/1.17.12.1/foo-1.17.12.1.jar",
		"com/example/foo/2.0.0/foo-2.0.0.jar",
		"npm/pkg-a/-/pkg-a-1.17.12.tgz",
		"npm/pkg-a/-/pkg-a-1.17.120.tgz",
		"raw/docs/1.17.12/notes.txt",
		// 版本号出现在文件名里但不在独立路径段：不得被 version: 命中。
		"raw/docs/notes-1.17.12.txt",
	}
	for _, path := range corpus {
		if err := assets.Upsert(repoID, path, "h", 1, "application/octet-stream", "s", "m"); err != nil {
			t.Fatalf("写入资产 %s：%v", path, err)
		}
	}
	if err := NewSearchIndexStateRepo(db).MarkCompleted(); err != nil {
		t.Fatalf("标记索引就绪：%v", err)
	}
	return &versionFilterFixture{assets: assets, repoID: repoID}
}

// expectIndexedBranch 在**生产同一个入口**（searchSnapshot）里断言本次查询真的走了索引分支。
//
// 只断言「候选值等于索引路径」不够：两种情况都可能让对照用例静默退化成双 LIKE。因此这里
// 逐层取证——
//  1. 领域层前置判定用的 SearchIndexReady() 为真（searchSnapshot 的复核用同一个谓词）；
//  2. 快照内复核后的有效标记 UseFullTextIndex 仍为真（没有被就绪复核打回）；
//  3. 该标记下拼出的 WHERE 的执行计划确实走 asset_path_fts（而不是全表/索引扫描）。
//
// 三条同时成立，才能说这次查询是「真的在索引分支上」与 LIKE 分支做对照。
func (f *versionFilterFixture) expectIndexedBranch(t *testing.T, filter SearchFilter) {
	t.Helper()
	ready, err := f.assets.SearchIndexReady()
	if err != nil {
		t.Fatalf("读取索引就绪状态：%v", err)
	}
	if !ready {
		t.Fatal("索引未标记就绪：查理会整体退回 LIKE，索引分支根本没有被验证")
	}
	// UseFullTextIndex 置真只是「允许走索引」，是否真的走由快照复核决定，这里一并验证。
	filter.UseFullTextIndex = true
	err = f.assets.searchSnapshot(context.Background(), filter, func(tx *sqlx.Tx, effective SearchFilter) error {
		if !effective.UseFullTextIndex {
			t.Fatalf("快照内复核后 UseFullTextIndex=false：本次查询走的是 LIKE（filter=%+v）", filter)
		}
		where, args := buildSearchWhereIndexed(effective, []int64{f.repoID})
		rows, err := tx.Queryx(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM asset WHERE `+where, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var plan strings.Builder
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				return err
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !strings.Contains(plan.String(), "asset_path_fts") {
			t.Fatalf("执行计划未使用 asset_path_fts（filter=%+v）：\n%s", filter, plan.String())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("在搜索快照内校验索引分支失败：%v", err)
	}
}

// searchPaths 执行一次搜索并返回按 path 升序排列的结果路径。
func (f *versionFilterFixture) searchPaths(t *testing.T, filter SearchFilter) []string {
	t.Helper()
	assets, total, err := f.assets.SearchByFilter(context.Background(), filter, []int64{f.repoID}, "path", "asc", 100, 0)
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if total != len(assets) {
		t.Fatalf("total=%d 与 items=%d 不一致（段边界条件必须让 total 精确）", total, len(assets))
	}
	paths := make([]string, 0, len(assets))
	for _, a := range assets {
		paths = append(paths, a.Path)
	}
	sort.Strings(paths)
	return paths
}

// TestSearchVersionStrictMatch 钉住「整段相等」的严格性。
func TestSearchVersionStrictMatch(t *testing.T) {
	f := newVersionFilterFixture(t)
	got := f.searchPaths(t, SearchFilter{IncludeVersions: []string{"1.17.12"}})
	want := []string{
		"com/example/foo/1.17.12/foo-1.17.12.jar",
		"com/example/foo/1.17.12/foo-1.17.12.jar.sha1",
		"com/example/foo/1.17.12/foo-1.17.12.pom",
		"npm/pkg-a/-/pkg-a-1.17.12.tgz",
		"raw/docs/1.17.12/notes.txt",
	}
	if !equalStrings(got, want) {
		t.Fatalf("version:1.17.12 命中\n got  %v\n want %v", got, want)
	}
	for _, path := range got {
		for _, noise := range []string{"1.17.120", "1.17.12-rc1", "1.17.12.1", "notes-1.17.12.txt"} {
			if strings.Contains(path, noise) {
				t.Fatalf("version:1.17.12 不得命中 %s（含 %s）", path, noise)
			}
		}
	}
}

// TestSearchVersionMultipleOR 钉住多个 version: 为 OR。
func TestSearchVersionMultipleOR(t *testing.T) {
	f := newVersionFilterFixture(t)
	got := f.searchPaths(t, SearchFilter{IncludeVersions: []string{"1.17.120", "2.0.0"}})
	want := []string{
		"com/example/foo/1.17.120/foo-1.17.120.jar",
		"com/example/foo/2.0.0/foo-2.0.0.jar",
		"npm/pkg-a/-/pkg-a-1.17.120.tgz",
	}
	if !equalStrings(got, want) {
		t.Fatalf("version:1.17.120 version:2.0.0 命中\n got  %v\n want %v", got, want)
	}
}

// TestSearchExcludeVersion 钉住多个 -version: 为 AND（都不相等）。
func TestSearchExcludeVersion(t *testing.T) {
	f := newVersionFilterFixture(t)
	got := f.searchPaths(t, SearchFilter{
		ExcludeVersions: []string{"1.17.12", "1.17.120"},
	})
	for _, path := range got {
		for _, banned := range []string{"/1.17.12/", "/1.17.120/", "-1.17.12.tgz", "-1.17.120.tgz"} {
			if strings.Contains(path, banned) {
				t.Fatalf("-version: 未排除 %s（含 %s）", path, banned)
			}
		}
	}
	// 未被排除的版本必须仍然在结果里（排除不能让其余结果消失）。
	for _, must := range []string{
		"com/example/foo/1.17.12-rc1/foo-1.17.12-rc1.jar",
		"com/example/foo/1.17.12.1/foo-1.17.12.1.jar",
		"com/example/foo/2.0.0/foo-2.0.0.jar",
	} {
		if !containsString(got, must) {
			t.Fatalf("-version: 误删了未排除的路径 %s：%v", must, got)
		}
	}
}

// TestSearchVersionCoexistsWithFTS 钉住版本条件与的 FTS 候选集条件**共存**：
// 走索引与走全表 LIKE 两条路径必须给出逐条一致的结果（版本条件不得在走索引时被丢掉）。
//
// 本用例的**前提**由 expectIndexedBranch 现场取证：索引已就绪、快照内复核后仍为真、
// 执行计划确实经过 asset_path_fts。少了这三条，对照的两边都是 LIKE，用例就变成了
// 「LIKE 与 LIKE 一致」的同义反复。
func TestSearchVersionCoexistsWithFTS(t *testing.T) {
	f := newVersionFilterFixture(t)
	cases := []SearchFilter{
		{IncludeTerms: []string{"foo"}, IncludeVersions: []string{"1.17.12"}},
		{IncludeTerms: []string{"pkg-a"}, IncludeVersions: []string{"1.17.12"}},
		{IncludeTerms: []string{"foo"}, ExcludeVersions: []string{"1.17.12"}},
		{IncludeVersions: []string{"1.17.12"}},
	}
	for _, base := range cases {
		degraded := base
		degraded.UseFullTextIndex = false
		indexed := base
		indexed.UseFullTextIndex = true
		gotDegraded := f.searchPaths(t, degraded)
		// 只有真的存在可下推包含词时，索引分支才成立（纯 version: 条件没有候选驱动词，
		// 本来就该走 LIKE）；这类用例只做两条路径的结果对照。
		if len(indexableTerms(base.IncludeTerms)) > 0 {
			f.expectIndexedBranch(t, base)
		}
		gotIndexed := f.searchPaths(t, indexed)
		if !equalStrings(gotDegraded, gotIndexed) {
			t.Fatalf("索引与降级结果不一致（filter=%+v）\n 降级 %v\n 索引 %v",
				base, gotDegraded, gotIndexed)
		}
	}
}

// TestSearchAllByFilterTruncates 钉住聚合扫描的预算截断口径：
// 按 path ASC 确定性截断，truncated 如实回报，未截断时行数等于 total。
func TestSearchAllByFilterTruncates(t *testing.T) {
	f := newVersionFilterFixture(t)
	ctx := context.Background()
	filter := SearchFilter{}

	all, truncated, err := f.assets.SearchAllByFilter(ctx, filter, []int64{f.repoID}, 1000)
	if err != nil {
		t.Fatalf("扫描失败：%v", err)
	}
	if truncated {
		t.Fatal("预算充足时不应标记截断")
	}
	_, total, err := f.assets.SearchByFilter(ctx, filter, []int64{f.repoID}, "path", "asc", 1, 0)
	if err != nil {
		t.Fatalf("计数失败：%v", err)
	}
	if len(all) != total {
		t.Fatalf("未截断时扫描行数 = %d，期望等于 total=%d", len(all), total)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Path > all[i].Path {
			t.Fatalf("扫描顺序不是 path 升序：%s 在 %s 之前", all[i-1].Path, all[i].Path)
		}
	}

	limited, truncated, err := f.assets.SearchAllByFilter(ctx, filter, []int64{f.repoID}, 3)
	if err != nil {
		t.Fatalf("截断扫描失败：%v", err)
	}
	if !truncated {
		t.Fatal("超过预算时必须标记截断")
	}
	if len(limited) != 3 {
		t.Fatalf("截断后行数 = %d，期望 3", len(limited))
	}
	// 截断是确定性的：取到的必须是 path 最小的 3 条（与全量前 3 条逐条一致）。
	if !equalStrings(pathsOf(limited), pathsOf(all[:3])) {
		t.Fatalf("截断结果不是 path 最小的 3 条：%v", pathsOf(limited))
	}
}

// TestSearchVersionCombinesWithOtherConditions 钉住「version: 与其他条件组合」（规格 §5）：
// `version:1.17.12 ext:jar` 只返该版本的 jar；`version:1.17.12 -ext:sha1` 生效。
func TestSearchVersionCombinesWithOtherConditions(t *testing.T) {
	f := newVersionFilterFixture(t)

	onlyJar := f.searchPaths(t, SearchFilter{
		IncludeVersions: []string{"1.17.12"},
		IncludeExts:     []string{"jar"},
	})
	for _, path := range onlyJar {
		if !strings.HasSuffix(path, ".jar") {
			t.Fatalf("version:1.17.12 ext:jar 返回了非 jar 路径：%v", onlyJar)
		}
		if !strings.Contains(path, "/1.17.12/") {
			t.Fatalf("version:1.17.12 ext:jar 返回了非目标版本：%v", onlyJar)
		}
	}
	if !equalStrings(onlyJar, []string{"com/example/foo/1.17.12/foo-1.17.12.jar"}) {
		t.Fatalf("version:1.17.12 ext:jar 命中 = %v，期望只有 foo-1.17.12.jar", onlyJar)
	}

	withoutSha1 := f.searchPaths(t, SearchFilter{
		IncludeVersions: []string{"1.17.12"},
		ExcludeExts:     []string{"sha1"},
	})
	for _, path := range withoutSha1 {
		if strings.HasSuffix(path, ".sha1") {
			t.Fatalf("version:1.17.12 -ext:sha1 未排除 %s", path)
		}
	}
	if !containsString(withoutSha1, "com/example/foo/1.17.12/foo-1.17.12.jar") {
		t.Fatalf("version:1.17.12 -ext:sha1 误删了 jar：%v", withoutSha1)
	}

	// version: 与普通关键词组合：两者都是 AND，版本值不参与子串匹配。
	withTerm := f.searchPaths(t, SearchFilter{
		IncludeTerms:    []string{"foo"},
		IncludeVersions: []string{"1.17.12"},
	})
	if !equalStrings(withTerm, []string{"com/example/foo/1.17.12/foo-1.17.12.jar", "com/example/foo/1.17.12/foo-1.17.12.jar.sha1", "com/example/foo/1.17.12/foo-1.17.12.pom"}) {
		t.Fatalf("foo + version:1.17.12 命中 = %v", withTerm)
	}
}

// pathsOf 提取路径列表。
func pathsOf(assets []Asset) []string {
	out := make([]string, 0, len(assets))
	for _, a := range assets {
		out = append(out, a.Path)
	}
	return out
}

// equalStrings 比较两个字符串切片是否逐项相等。
func equalStrings(a, b []string) bool {
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

// containsString 判断切片是否含目标值。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
