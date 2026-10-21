package repository

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
	return &versionFilterFixture{assets: assets, repoID: repoID}
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
