package repository

import (
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// TestRepoListByNamesResolvesPrimaryAndAlias 钉住 npm 成员批量解析依赖的口径：
// 主名与别名都能解析到同一个仓库、返回的是主名、解析不到的名字不出现在结果里，
// 且空入参不发起查询。
func TestRepoListByNamesResolvesPrimaryAndAlias(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repo-names.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := NewRepoRepo(db)
	primaryID, err := repos.Create("batch-primary", "npm", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	otherID, err := repos.Create("batch-other", "npm", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := repos.SetAliases(primaryID, []string{"batch-alias"}); err != nil {
		t.Fatalf("写别名：%v", err)
	}

	got, err := repos.ListByNames([]string{"batch-primary", "batch-alias", "batch-other", "batch-missing", "", "batch-primary"})
	if err != nil {
		t.Fatalf("批量解析失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("解析结果条数 = %d，期望 3（主名、别名、另一主名各一条）：%v", len(got), got)
	}
	if repo := got["batch-primary"]; repo == nil || repo.ID != primaryID || repo.Name != "batch-primary" {
		t.Fatalf("主名解析错误：%+v", got["batch-primary"])
	}
	if repo := got["batch-alias"]; repo == nil || repo.ID != primaryID {
		t.Fatalf("别名应解析到同一仓库：%+v", got["batch-alias"])
	}
	if repo := got["batch-other"]; repo == nil || repo.ID != otherID {
		t.Fatalf("另一主名解析错误：%+v", got["batch-other"])
	}
	if _, ok := got["batch-missing"]; ok {
		t.Fatal("不存在的名字不应出现在结果里")
	}

	empty, err := repos.ListByNames(nil)
	if err != nil {
		t.Fatalf("空入参解析失败：%v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空入参应返回空 map，得 %v", empty)
	}
}
