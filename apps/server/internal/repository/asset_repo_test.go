package repository_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTestRepos 打开临时 SQLite、迁移并返回资产与仓库两个 Repo。
func newTestRepos(t *testing.T) (*repository.AssetRepo, *repository.RepoRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "asset.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewAssetRepo(db), repository.NewRepoRepo(db)
}

func TestAssetUpsertInsertThenOverwrite(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}

	// 首次插入。
	if err := assets.Upsert(repoID, "a/b.txt", "hash1", 11, "text/plain", "sha1-1", "md5-1"); err != nil {
		t.Fatalf("首次 Upsert：%v", err)
	}
	got, err := assets.GetByPath(repoID, "a/b.txt")
	if err != nil {
		t.Fatalf("GetByPath：%v", err)
	}
	if got.BlobHash != "hash1" || got.Size != 11 || got.ContentType != "text/plain" || got.Sha1 != "sha1-1" || got.Md5 != "md5-1" {
		t.Fatalf("首次插入内容不符：%+v", got)
	}

	// 覆盖写同路径。
	if err := assets.Upsert(repoID, "a/b.txt", "hash2", 22, "application/json", "sha1-2", "md5-2"); err != nil {
		t.Fatalf("覆盖 Upsert：%v", err)
	}
	got, err = assets.GetByPath(repoID, "a/b.txt")
	if err != nil {
		t.Fatalf("GetByPath（覆盖后）：%v", err)
	}
	if got.BlobHash != "hash2" || got.Size != 22 || got.ContentType != "application/json" || got.Sha1 != "sha1-2" || got.Md5 != "md5-2" {
		t.Fatalf("覆盖写未生效：%+v", got)
	}
}

func TestAssetGetByPathNotFound(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("应返回 ErrNotFound，实际：%v", err)
	}
}

func TestAssetDeleteByPath(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := assets.Upsert(repoID, "x.bin", "h", 1, "application/octet-stream", "s1", "m1"); err != nil {
		t.Fatalf("Upsert：%v", err)
	}
	if err := assets.DeleteByPath(repoID, "x.bin"); err != nil {
		t.Fatalf("DeleteByPath：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "x.bin"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound，实际：%v", err)
	}
	// 删除不存在的路径应返回 ErrNotFound。
	if err := assets.DeleteByPath(repoID, "nope"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("删除不存在应 ErrNotFound，实际：%v", err)
	}
}

// seedPaths 批量写入制品路径（内容无关，仅验证层级推导）。
func seedPaths(t *testing.T, assets *repository.AssetRepo, repoID int64, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := assets.Upsert(repoID, p, "hash-"+p, 1, "text/plain", "", ""); err != nil {
			t.Fatalf("Upsert %s：%v", p, err)
		}
	}
}

// TestListDirectChildrenSeparatesLevels 验证「直接子项」不递归：子目录只出当前一层的段名，
// 多层嵌套的制品不得作为文件混进当前层。
func TestListDirectChildrenSeparatesLevels(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	seedPaths(t, assets, repoID,
		"a/x.txt", "a/y.txt", "a/deep/z.txt", "a/deep/deeper/w.txt", "b/c.txt", "root.txt",
	)

	cases := []struct {
		prefix    string
		wantDirs  []string
		wantFiles []string
	}{
		{"", []string{"a", "b"}, []string{"root.txt"}},
		{"a/", []string{"a/deep"}, []string{"a/x.txt", "a/y.txt"}},
		{"a/deep/", []string{"a/deep/deeper"}, []string{"a/deep/z.txt"}},
		{"a/deep/deeper/", []string{}, []string{"a/deep/deeper/w.txt"}},
	}
	for _, tc := range cases {
		got, err := assets.ListDirectChildren([]int64{repoID}, tc.prefix, 0, 0)
		if err != nil {
			t.Fatalf("ListDirectChildren(%q)：%v", tc.prefix, err)
		}
		if !reflect.DeepEqual(got.Dirs, tc.wantDirs) {
			t.Errorf("前缀 %q 的子目录 = %v，期望 %v", tc.prefix, got.Dirs, tc.wantDirs)
		}
		gotFiles := make([]string, 0, len(got.Files))
		for _, f := range got.Files {
			gotFiles = append(gotFiles, f.Path)
		}
		if !reflect.DeepEqual(gotFiles, tc.wantFiles) {
			t.Errorf("前缀 %q 的文件 = %v，期望 %v", tc.prefix, gotFiles, tc.wantFiles)
		}
		if got.FileTotal != len(tc.wantFiles) {
			t.Errorf("前缀 %q 的文件计数 = %d，期望 %d", tc.prefix, got.FileTotal, len(tc.wantFiles))
		}
	}
}

// TestListDirectChildrenPaginatesFilesNotDirs 验证分页只作用于直接文件：
// 无论取第几页、页大小多小，同层子目录都必须完整出现（回归：截断曾让同层目录整片消失）。
func TestListDirectChildrenPaginatesFilesNotDirs(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	const totalFiles = 25
	for i := 0; i < totalFiles; i++ {
		seedPaths(t, assets, repoID, fmt.Sprintf("big/f%03d.txt", i))
	}
	seedPaths(t, assets, repoID, "other/x.txt")

	// 首页：10 条文件，计数 25，子目录仍完整（big、other）。
	first, err := assets.ListDirectChildren([]int64{repoID}, "", 10, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren：%v", err)
	}
	if !reflect.DeepEqual(first.Dirs, []string{"big", "other"}) {
		t.Errorf("首页子目录 = %v，期望 [big other]", first.Dirs)
	}
	if first.FileTotal != 0 {
		t.Errorf("根的直接文件计数 = %d，期望 0（big/ 与 other/ 都是目录）", first.FileTotal)
	}

	// 目录内的文件分页：页大小 10 → 第 1 页 f000..f009，第 3 页 5 条。
	page1, err := assets.ListDirectChildren([]int64{repoID}, "big/", 10, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren(big/)：%v", err)
	}
	if page1.FileTotal != totalFiles || len(page1.Files) != 10 {
		t.Fatalf("第 1 页：计数 = %d（期望 %d）、条数 = %d（期望 10）", page1.FileTotal, totalFiles, len(page1.Files))
	}
	if page1.Files[0].Path != "big/f000.txt" || page1.Files[9].Path != "big/f009.txt" {
		t.Errorf("第 1 页范围 = %s..%s，期望 big/f000.txt..big/f009.txt", page1.Files[0].Path, page1.Files[9].Path)
	}
	if len(page1.Dirs) != 0 {
		t.Errorf("big/ 下不应有子目录，实际 %v", page1.Dirs)
	}

	page3, err := assets.ListDirectChildren([]int64{repoID}, "big/", 10, 20)
	if err != nil {
		t.Fatalf("ListDirectChildren(big/, page3)：%v", err)
	}
	if len(page3.Files) != 5 || page3.Files[0].Path != "big/f020.txt" {
		t.Errorf("第 3 页 = %d 条、首条 %s，期望 5 条、big/f020.txt", len(page3.Files), page3.Files[0].Path)
	}
	if page3.FileTotal != totalFiles {
		t.Errorf("第 3 页计数 = %d，期望 %d", page3.FileTotal, totalFiles)
	}
}

// TestListDirectChildrenKeepsAllDirsUnderHugeSubtree 验证「子树极大时同层目录不丢」：
// 旧实现在此场景下会把取数截断，导致 z/ 整片消失。
func TestListDirectChildrenKeepsAllDirsUnderHugeSubtree(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// a/ 下 1200 件（超出旧实现的 1000 条取数上限），z/ 下 1 件。
	for i := 0; i < 1200; i++ {
		seedPaths(t, assets, repoID, fmt.Sprintf("a/f%04d.txt", i))
	}
	seedPaths(t, assets, repoID, "z/only.txt")

	got, err := assets.ListDirectChildren([]int64{repoID}, "", 0, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren：%v", err)
	}
	if !reflect.DeepEqual(got.Dirs, []string{"a", "z"}) {
		t.Fatalf("根子目录 = %v，期望 [a z]（z/ 不因 a/ 子树庞大而消失）", got.Dirs)
	}
	if got.FileTotal != 0 {
		t.Errorf("根直接文件 = %d，期望 0", got.FileTotal)
	}
}

// TestListDirectChildrenHandlesNonASCIILevels 验证非 ASCII 路径的层级切分：
// SQLite 的 substr/instr 对 TEXT 按**字符**计数，而前缀长度若按字节计算就会切错。
func TestListDirectChildrenHandlesNonASCIILevels(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 中文目录名（每个汉字 3 字节）与 emoji（4 字节）都覆盖到。
	seedPaths(t, assets, repoID,
		"中文目录/直接.txt",
		"中文目录/子目录/深层.txt",
		"中文目录/子目录/更深处/x.txt",
		"emoji📦/包/文件.bin",
	)

	cases := []struct {
		prefix    string
		wantDirs  []string
		wantFiles []string
	}{
		{"中文目录/", []string{"中文目录/子目录"}, []string{"中文目录/直接.txt"}},
		{"中文目录/子目录/", []string{"中文目录/子目录/更深处"}, []string{"中文目录/子目录/深层.txt"}},
		{"emoji📦/", []string{"emoji📦/包"}, []string{}},
	}
	for _, tc := range cases {
		got, err := assets.ListDirectChildren([]int64{repoID}, tc.prefix, 0, 0)
		if err != nil {
			t.Fatalf("ListDirectChildren(%q)：%v", tc.prefix, err)
		}
		if !reflect.DeepEqual(got.Dirs, tc.wantDirs) {
			t.Errorf("前缀 %q 的子目录 = %v，期望 %v", tc.prefix, got.Dirs, tc.wantDirs)
		}
		gotFiles := make([]string, 0, len(got.Files))
		for _, f := range got.Files {
			gotFiles = append(gotFiles, f.Path)
		}
		if !reflect.DeepEqual(gotFiles, tc.wantFiles) {
			t.Errorf("前缀 %q 的文件 = %v，期望 %v", tc.prefix, gotFiles, tc.wantFiles)
		}
		if got.FileTotal != len(tc.wantFiles) {
			t.Errorf("前缀 %q 的文件计数 = %d，期望 %d", tc.prefix, got.FileTotal, len(tc.wantFiles))
		}
	}
}

// TestListDirectChildrenSumsFileBytes 验证直接文件的体积合计：
// 只统计当前层的直接文件、与分页无关（取第几页都返回同一合计）。
func TestListDirectChildrenSumsFileBytes(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	put := func(path string, size int64) {
		t.Helper()
		if err := assets.Upsert(repoID, path, "h-"+path, size, "application/octet-stream", "", ""); err != nil {
			t.Fatalf("Upsert %s：%v", path, err)
		}
	}
	put("d/a.bin", 1500)
	put("d/b.bin", 500)
	put("d/sub/c.bin", 9999) // 子目录内的制品不计入 d/ 的直接文件合计

	// 页大小 1：合计与总数仍按全量直接文件计算。
	page, err := assets.ListDirectChildren([]int64{repoID}, "d/", 1, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren：%v", err)
	}
	if page.FileTotal != 2 {
		t.Errorf("直接文件数 = %d，期望 2", page.FileTotal)
	}
	if page.FileBytes != 2000 {
		t.Errorf("直接文件体积合计 = %d，期望 2000（不含 d/sub/ 内的 9999）", page.FileBytes)
	}
	if len(page.Files) != 1 {
		t.Errorf("本页文件数 = %d，期望 1", len(page.Files))
	}

	// 第二页合计不变。
	second, err := assets.ListDirectChildren([]int64{repoID}, "d/", 1, 1)
	if err != nil {
		t.Fatalf("ListDirectChildren(第 2 页)：%v", err)
	}
	if second.FileBytes != 2000 || second.FileTotal != 2 {
		t.Errorf("第 2 页合计/总数 = %d/%d，期望 2000/2", second.FileBytes, second.FileTotal)
	}

	// 仓库根没有直接文件。
	root, err := assets.ListDirectChildren([]int64{repoID}, "", 0, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren(根)：%v", err)
	}
	if root.FileTotal != 0 || root.FileBytes != 0 {
		t.Errorf("根的直接文件 = %d 件 / %d 字节，期望 0/0", root.FileTotal, root.FileBytes)
	}
}

// TestListDirectChildrenAggregatesDirStats 验证子目录聚合：计数是子树内制品总数
// （含更深层级）、时间是子树内最大的 updated_at，且与 Dirs 同序、路径为自仓库根起算的完整路径。
func TestListDirectChildrenAggregatesDirStats(t *testing.T) {
	assets, repos := newTestRepos(t)
	repoID, err := repos.Create("raw-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	put := func(path, updatedAt string) {
		t.Helper()
		if err := assets.UpsertWithTime(repoID, path, "h-"+path, 1, "text/plain", "", "",
			"2026-01-01 00:00:00", updatedAt); err != nil {
			t.Fatalf("UpsertWithTime %s：%v", path, err)
		}
	}
	put("libs/a/one.bin", "2026-02-01 10:00:00")
	put("libs/a/deep/two.bin", "2026-05-20 12:00:00") // 更深层级也要计入 a/
	put("libs/b/three.bin", "2025-12-31 23:59:59")
	put("libs/note.txt", "2026-06-01 00:00:00")

	got, err := assets.ListDirectChildren([]int64{repoID}, "libs/", 0, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren：%v", err)
	}
	if !reflect.DeepEqual(got.Dirs, []string{"libs/a", "libs/b"}) {
		t.Fatalf("子目录 = %v，期望 [libs/a libs/b]", got.Dirs)
	}
	if len(got.DirStats) != len(got.Dirs) {
		t.Fatalf("DirStats 数量 = %d，期望与 Dirs 一致（%d）", len(got.DirStats), len(got.Dirs))
	}
	want := []repository.DirStat{
		{Path: "libs/a", Count: 2, Latest: "2026-05-20 12:00:00"},
		{Path: "libs/b", Count: 1, Latest: "2025-12-31 23:59:59"},
	}
	if !reflect.DeepEqual(got.DirStats, want) {
		t.Errorf("DirStats = %+v，期望 %+v", got.DirStats, want)
	}

	// 仓库根：libs 的计数是整棵子树的制品数（4 件），时间为子树内最大值。
	root, err := assets.ListDirectChildren([]int64{repoID}, "", 0, 0)
	if err != nil {
		t.Fatalf("ListDirectChildren(根)：%v", err)
	}
	wantRoot := []repository.DirStat{{Path: "libs", Count: 4, Latest: "2026-06-01 00:00:00"}}
	if !reflect.DeepEqual(root.DirStats, wantRoot) {
		t.Errorf("根的目录聚合 = %+v，期望 %+v", root.DirStats, wantRoot)
	}
}
