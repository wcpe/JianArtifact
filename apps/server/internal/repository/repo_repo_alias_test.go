package repository_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRepoAliasResolve 别名经 GetByName 解析到主名仓库（返回的是主名仓库，别名只是入口）。
func TestRepoAliasResolve(t *testing.T) {
	repos := newRepoRepo(t)
	repoID, err := repos.Create("maven-main", "maven", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	got, err := repos.GetByName("maven-main")
	if err != nil {
		t.Fatalf("按主名取仓库：%v", err)
	}
	if got.ID != repoID {
		t.Fatalf("主名取到错误仓库：%+v", got)
	}

	if err := repos.SetAliases(repoID, []string{"maven-legacy", "maven-old"}); err != nil {
		t.Fatalf("写别名：%v", err)
	}
	for _, alias := range []string{"maven-legacy", "maven-old"} {
		byAlias, err := repos.GetByName(alias)
		if err != nil {
			t.Fatalf("按别名 %q 取仓库：%v", alias, err)
		}
		if byAlias.ID != repoID || byAlias.Name != "maven-main" {
			t.Errorf("别名 %q 应解析到主名仓库 maven-main，得 %+v", alias, byAlias)
		}
	}

	// 未知名字仍返回 ErrNotFound。
	if _, err := repos.GetByName("ghost"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("未知名应 ErrNotFound，得 %v", err)
	}
}

// TestRepoListAliasesAndBatch ListAliases 与 ListAliasesByRepos 的口径一致（字典序）。
func TestRepoListAliasesAndBatch(t *testing.T) {
	repos := newRepoRepo(t)
	id1, err := repos.Create("raw-a", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库 a：%v", err)
	}
	id2, err := repos.Create("raw-b", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库 b：%v", err)
	}
	if err := repos.SetAliases(id1, []string{"z-alias", "a-alias"}); err != nil {
		t.Fatalf("写别名 a：%v", err)
	}
	if err := repos.SetAliases(id2, []string{"b-alias"}); err != nil {
		t.Fatalf("写别名 b：%v", err)
	}

	aliases, err := repos.ListAliases(id1)
	if err != nil {
		t.Fatalf("ListAliases：%v", err)
	}
	if !slices.Equal(aliases, []string{"a-alias", "z-alias"}) {
		t.Errorf("别名应按字典序，得 %v", aliases)
	}

	batch, err := repos.ListAliasesByRepos([]int64{id1, id2})
	if err != nil {
		t.Fatalf("ListAliasesByRepos：%v", err)
	}
	if !slices.Equal(batch[id1], []string{"a-alias", "z-alias"}) || !slices.Equal(batch[id2], []string{"b-alias"}) {
		t.Errorf("批量别名口径应与 ListAliases 一致，得 %+v", batch)
	}

	// 无别名的仓库返回空切片，不带 nil 使其在 JSON 中稳定。
	if err := repos.SetAliases(id2, nil); err != nil {
		t.Fatalf("清空别名：%v", err)
	}
	empty, err := repos.ListAliases(id2)
	if err != nil {
		t.Fatalf("ListAliases（空）：%v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("清空后应为空切片，得 %#v", empty)
	}
}

// TestRepoSetAliasesOverwrite 覆盖式写入：旧的别名被整体替换。
func TestRepoSetAliasesOverwrite(t *testing.T) {
	repos := newRepoRepo(t)
	repoID, err := repos.Create("raw-x", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := repos.SetAliases(repoID, []string{"one", "two"}); err != nil {
		t.Fatalf("写别名：%v", err)
	}
	if err := repos.SetAliases(repoID, []string{"three"}); err != nil {
		t.Fatalf("覆盖写别名：%v", err)
	}
	aliases, err := repos.ListAliases(repoID)
	if err != nil {
		t.Fatalf("ListAliases：%v", err)
	}
	if !slices.Equal(aliases, []string{"three"}) {
		t.Fatalf("覆盖后应只剩新集合，得 %v", aliases)
	}
	if _, err := repos.GetByName("one"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("被替换掉的旧别名不应再解析，得 %v", err)
	}
}

// TestRepoSetAliasesUniquenessConflict alias 主键全局唯一：跨仓库重复别名被拒绝。
func TestRepoSetAliasesUniquenessConflict(t *testing.T) {
	repos := newRepoRepo(t)
	id1, err := repos.Create("raw-a", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库 a：%v", err)
	}
	id2, err := repos.Create("raw-b", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库 b：%v", err)
	}
	if err := repos.SetAliases(id1, []string{"shared"}); err != nil {
		t.Fatalf("写别名 a：%v", err)
	}
	if err := repos.SetAliases(id2, []string{"shared"}); err == nil {
		t.Fatalf("跨仓库重复别名应被唯一约束拒绝")
	}
}

// TestRepoNameTaken NameTaken 需同时覆盖主名与别名。
func TestRepoNameTaken(t *testing.T) {
	repos := newRepoRepo(t)
	repoID, err := repos.Create("raw-main", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := repos.SetAliases(repoID, []string{"raw-alias"}); err != nil {
		t.Fatalf("写别名：%v", err)
	}
	cases := []struct {
		name string
		want bool
	}{
		{"raw-main", true},
		{"raw-alias", true},
		{"raw-free", false},
	}
	for _, tc := range cases {
		got, err := repos.NameTaken(tc.name)
		if err != nil {
			t.Fatalf("NameTaken(%q)：%v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("NameTaken(%q) = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestRepoRenameKeepsOldNameResolvable 重命名后：新名为主名，旧名自动转别名仍可解析。
func TestRepoRenameKeepsOldNameResolvable(t *testing.T) {
	repos := newRepoRepo(t)
	repoID, err := repos.Create("raw-old", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := repos.Rename("raw-old", "raw-new"); err != nil {
		t.Fatalf("Rename：%v", err)
	}

	renamed, err := repos.GetByName("raw-new")
	if err != nil {
		t.Fatalf("按新名取仓库：%v", err)
	}
	if renamed.ID != repoID || renamed.Name != "raw-new" {
		t.Fatalf("新名应为主名，得 %+v", renamed)
	}
	// 旧名已成别名，仍解析到同一仓库。
	byOld, err := repos.GetByName("raw-old")
	if err != nil {
		t.Fatalf("按旧名取仓库：%v", err)
	}
	if byOld.ID != repoID || byOld.Name != "raw-new" {
		t.Errorf("旧名应经别名解析到 raw-new，得 %+v", byOld)
	}
	if _, err := repos.GetByName("raw-old"); err != nil {
		t.Errorf("旧名应可解析，得 %v", err)
	}
}

// TestRepoRenameNotFound 对不存在的仓库重命名返回 ErrNotFound。
func TestRepoRenameNotFound(t *testing.T) {
	repos := newRepoRepo(t)
	if err := repos.Rename("ghost", "x"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("重命名不存在仓库应 ErrNotFound，得 %v", err)
	}
}
