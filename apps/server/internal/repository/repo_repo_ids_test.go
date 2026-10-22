package repository

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住 RepoRepo.ListByIDs 的口径：按 ID 批量补齐仓库元数据，与仓库总数无关。
//
// 它存在的业务理由是搜索建议：候选行的仓库可能不在「前 1000 个仓库」的全量列举里，
// 若拿那份列举当格式映射的唯一来源，这些仓库下的命中行会因为格式为空而静默消失。

// newRepoIDsFixture 建库并创建若干仓库，返回仓储与各自的 ID。
func newRepoIDsFixture(t *testing.T, names ...string) (*RepoRepo, []int64) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repo-ids.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := NewRepoRepo(db)
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		id, err := repos.Create(name, "maven", "hosted", "public", "")
		if err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
		ids = append(ids, id)
	}
	return repos, ids
}

// TestRepoListByIDsReturnsRequestedMetadata 覆盖基本口径：
// 命中项带完整元数据（含格式）、缺席的 ID 不出现、重复 ID 与非法 ID 不影响结果、空入参返回空映射。
func TestRepoListByIDsReturnsRequestedMetadata(t *testing.T) {
	repos, ids := newRepoIDsFixture(t, "ids-a", "ids-b", "ids-c")

	got, err := repos.ListByIDs([]int64{ids[0], ids[2], ids[0], 0, -1, 999999})
	if err != nil {
		t.Fatalf("按 ID 批量取仓库失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("返回条数 = %d，期望 2（只含存在的两个 ID）：%+v", len(got), got)
	}
	if r, ok := got[ids[0]]; !ok || r.Name != "ids-a" || r.Format != "maven" || r.Type != "hosted" || r.Visibility != "public" {
		t.Fatalf("ID %d 的元数据不符：%+v（存在=%v）", ids[0], r, ok)
	}
	if r, ok := got[ids[2]]; !ok || r.Name != "ids-c" {
		t.Fatalf("ID %d 的元数据不符：%+v（存在=%v）", ids[2], r, ok)
	}
	if _, ok := got[ids[1]]; ok {
		t.Fatalf("未请求的 ID %d 不应出现：%+v", ids[1], got)
	}

	empty, err := repos.ListByIDs(nil)
	if err != nil {
		t.Fatalf("空入参不应报错：%v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空入参应返回空映射，得 %+v", empty)
	}
}

// TestRepoListByIDsCoversReposBeyondListPage 钉住「与全量列举上限无关」：
// 仓库数超过 List 的分页上限（1000）时，超出部分仍能按 ID 取到（这正是搜索建议依赖的性质）。
func TestRepoListByIDsCoversReposBeyondListPage(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repo-ids-bulk.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := NewRepoRepo(db)
	// 单事务批量插入 1001 个元数据仓库：只为把最后一个挤到全量列举的分页上限之外。
	const total = 1001
	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("开启事务：%v", err)
	}
	stmt, err := tx.Preparex(`INSERT INTO repository (name, format, type, visibility, config)
		VALUES (?, 'maven', 'hosted', 'public', '{}')`)
	if err != nil {
		t.Fatalf("准备语句：%v", err)
	}
	for i := 0; i < total; i++ {
		if _, err := stmt.Exec("bulk-" + strconv.Itoa(i)); err != nil {
			t.Fatalf("插入第 %d 个仓库：%v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交：%v", err)
	}

	page, err := repos.List(1000, 0)
	if err != nil {
		t.Fatalf("全量列举失败：%v", err)
	}
	if len(page) != 1000 {
		t.Fatalf("全量列举条数 = %d，期望 1000（前提：列举有上限）", len(page))
	}
	// 最后一个仓库（id 最大）必然落在列举之外。
	lastID := int64(total)
	for _, r := range page {
		if r.ID == lastID {
			t.Fatalf("前提不成立：ID %d 仍在全量列举里", lastID)
		}
	}
	got, err := repos.ListByIDs([]int64{lastID})
	if err != nil {
		t.Fatalf("按 ID 取仓库失败：%v", err)
	}
	if r, ok := got[lastID]; !ok || r.Format != "maven" {
		t.Fatalf("全量列举之外的仓库必须能按 ID 取到：%+v（存在=%v）", r, ok)
	}
}
