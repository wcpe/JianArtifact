package repository_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newPinnedEnv 打开临时 SQLite、迁移，返回置顶仓储 + 仓库仓储 + 用户仓储（置顶依赖二者的外键）。
func newPinnedEnv(t *testing.T) (*repository.PinnedRepoRepo, *repository.RepoRepo, *repository.UserRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "pinned.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewPinnedRepoRepo(db), repository.NewRepoRepo(db), repository.NewUserRepo(db)
}

// seedRepo 建一个 raw hosted 仓库并返回其 ID。
func seedRepo(t *testing.T, repos *repository.RepoRepo, name string) int64 {
	t.Helper()
	id, err := repos.Create(name, "raw", "hosted", "public", "{}")
	if err != nil {
		t.Fatalf("创建仓库 %s：%v", name, err)
	}
	return id
}

// newPinnedDB 打开临时 SQLite 并迁移，返回原始连接（供直接验证约束）。
func newPinnedDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "pinned-raw.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

// TestPinnedGlobalUniqueIndexRejectsDuplicates 全局置顶（user_id IS NULL）由 partial unique
// index 保证唯一：SQLite 主键对 NULL 不做唯一性判定，若无该索引，同一仓库可被插入两次。
func TestPinnedGlobalUniqueIndexRejectsDuplicates(t *testing.T) {
	db := newPinnedDB(t)
	repos := repository.NewRepoRepo(db)
	id, err := repos.Create("raw-a", "raw", "hosted", "public", "{}")
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}

	if _, err := db.Exec(`INSERT INTO pinned_repository (user_id, repository_id) VALUES (NULL, ?)`, id); err != nil {
		t.Fatalf("首次插入全局置顶应成功：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO pinned_repository (user_id, repository_id) VALUES (NULL, ?)`, id); err == nil {
		t.Fatalf("重复插入同一仓库的全局置顶应被唯一索引拒绝")
	}

	// 同一仓库在**不同用户**作用域下可各自置顶（NULL 与具体 user_id 互不冲突）。
	users := repository.NewUserRepo(db)
	uid, err := users.Create("bob", "hash", "user")
	if err != nil {
		t.Fatalf("创建用户：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO pinned_repository (user_id, repository_id) VALUES (?, ?)`, uid, id); err != nil {
		t.Fatalf("用户级置顶与全局置顶互不冲突：%v", err)
	}
}

// TestPinnedSetOverwritesWithoutDuplicates 覆盖式写入：重复 Set 只保留最后一次集合，不产生重复行。
func TestPinnedSetOverwritesWithoutDuplicates(t *testing.T) {
	pinned, repos, users := newPinnedEnv(t)
	uid, err := users.Create("bob", "hash", "user")
	if err != nil {
		t.Fatalf("创建用户：%v", err)
	}
	a := seedRepo(t, repos, "raw-a")
	b := seedRepo(t, repos, "raw-b")
	c := seedRepo(t, repos, "raw-c")

	// 首次写入含重复 ID：去重后只落一行。
	if err := pinned.Set(&uid, []int64{a, a, b}); err != nil {
		t.Fatalf("写入置顶：%v", err)
	}
	if got, _ := pinned.List(&uid); !reflect.DeepEqual(got, []int64{a, b}) {
		t.Fatalf("首次置顶应去重且保序，得 %v", got)
	}

	// 覆盖式：第二次写入整体替换（旧的 a、b 都不保留），且不残留重复行。
	if err := pinned.Set(&uid, []int64{c, c, c}); err != nil {
		t.Fatalf("覆盖写入置顶：%v", err)
	}
	if got, _ := pinned.List(&uid); !reflect.DeepEqual(got, []int64{c}) {
		t.Fatalf("覆盖后应只剩 c，得 %v", got)
	}

	// 空集合即清空。
	if err := pinned.Set(&uid, nil); err != nil {
		t.Fatalf("清空置顶：%v", err)
	}
	if got, _ := pinned.List(&uid); len(got) != 0 {
		t.Fatalf("清空后应为空，得 %v", got)
	}
}

// TestPinnedGlobalScopeAndUserScopeAreIndependent 全局置顶与用户级置顶互不覆盖，且均可重复写入。
func TestPinnedGlobalScopeAndUserScopeAreIndependent(t *testing.T) {
	pinned, repos, users := newPinnedEnv(t)
	uid, err := users.Create("bob", "hash", "user")
	if err != nil {
		t.Fatalf("创建用户：%v", err)
	}
	a := seedRepo(t, repos, "raw-a")
	b := seedRepo(t, repos, "raw-b")

	if err := pinned.Set(nil, []int64{a}); err != nil {
		t.Fatalf("写入全局置顶：%v", err)
	}
	if err := pinned.Set(&uid, []int64{b}); err != nil {
		t.Fatalf("写入用户置顶：%v", err)
	}

	// 再次写入全局置顶：同一仓库重复全局置顶由 partial unique index 拒绝，
	// 覆盖式语义下应先删后插、最终仍只有一行。
	if err := pinned.Set(nil, []int64{a, b}); err != nil {
		t.Fatalf("覆盖全局置顶：%v", err)
	}
	if got, _ := pinned.List(nil); !reflect.DeepEqual(got, []int64{a, b}) {
		t.Fatalf("全局置顶得 %v", got)
	}
	if got, _ := pinned.ListGlobal(); !reflect.DeepEqual(got, []int64{a, b}) {
		t.Fatalf("ListGlobal 得 %v", got)
	}
	// 用户级置顶不受全局写入影响。
	if got, _ := pinned.List(&uid); !reflect.DeepEqual(got, []int64{b}) {
		t.Fatalf("用户置顶应仍为 [b]，得 %v", got)
	}
}

// TestPinnedSetRejectsUnknownRepositoryWithoutWiping 非法仓库 ID 报 ErrNotFound，且不清空既有置顶。
func TestPinnedSetRejectsUnknownRepositoryWithoutWiping(t *testing.T) {
	pinned, repos, users := newPinnedEnv(t)
	uid, err := users.Create("bob", "hash", "user")
	if err != nil {
		t.Fatalf("创建用户：%v", err)
	}
	a := seedRepo(t, repos, "raw-a")
	if err := pinned.Set(&uid, []int64{a}); err != nil {
		t.Fatalf("写入置顶：%v", err)
	}

	if err := pinned.Set(&uid, []int64{a, 999999}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("未知仓库应返回 ErrNotFound，得 %v", err)
	}
	if got, _ := pinned.List(&uid); !reflect.DeepEqual(got, []int64{a}) {
		t.Fatalf("失败写入不应改动既有置顶，得 %v", got)
	}
}

// TestPinnedRowsCascadeOnRepositoryAndUserDelete 删除仓库 / 删除用户时置顶行级联清理。
func TestPinnedRowsCascadeOnRepositoryAndUserDelete(t *testing.T) {
	pinned, repos, users := newPinnedEnv(t)
	uid, err := users.Create("bob", "hash", "user")
	if err != nil {
		t.Fatalf("创建用户：%v", err)
	}
	a := seedRepo(t, repos, "raw-a")
	b := seedRepo(t, repos, "raw-b")

	if err := pinned.Set(nil, []int64{a, b}); err != nil {
		t.Fatalf("写入全局置顶：%v", err)
	}
	if err := pinned.Set(&uid, []int64{a, b}); err != nil {
		t.Fatalf("写入用户置顶：%v", err)
	}

	// 删除仓库 a：全局与用户级的 a 行都随外键级联消失。
	if err := repos.Delete("raw-a"); err != nil {
		t.Fatalf("删除仓库：%v", err)
	}
	if got, _ := pinned.ListGlobal(); !reflect.DeepEqual(got, []int64{b}) {
		t.Fatalf("删仓库后全局置顶应只剩 b，得 %v", got)
	}
	if got, _ := pinned.List(&uid); !reflect.DeepEqual(got, []int64{b}) {
		t.Fatalf("删仓库后用户置顶应只剩 b，得 %v", got)
	}

	// 删除用户：其私有置顶级联清理，全局置顶不受影响。
	if err := users.Delete(uid); err != nil {
		t.Fatalf("删除用户：%v", err)
	}
	if got, _ := pinned.List(&uid); len(got) != 0 {
		t.Fatalf("删用户后其置顶应为空，得 %v", got)
	}
	if got, _ := pinned.ListGlobal(); !reflect.DeepEqual(got, []int64{b}) {
		t.Fatalf("删用户不应影响全局置顶，得 %v", got)
	}
}

// TestPinnedNamesByIDsResolvesCurrentName 名字按 ID 实时解析：重命名后返回新名。
func TestPinnedNamesByIDsResolvesCurrentName(t *testing.T) {
	pinned, repos, _ := newPinnedEnv(t)
	a := seedRepo(t, repos, "raw-a")
	b := seedRepo(t, repos, "raw-b")

	names, err := pinned.NamesByIDs([]int64{a, b, 999999})
	if err != nil {
		t.Fatalf("解析仓库名：%v", err)
	}
	if len(names) != 2 || names[a] != "raw-a" || names[b] != "raw-b" {
		t.Fatalf("名字映射异常：%v", names)
	}

	if err := repos.Rename("raw-a", "raw-a-renamed"); err != nil {
		t.Fatalf("重命名仓库：%v", err)
	}
	names, err = pinned.NamesByIDs([]int64{a})
	if err != nil {
		t.Fatalf("重命名后解析仓库名：%v", err)
	}
	if names[a] != "raw-a-renamed" {
		t.Fatalf("应返回重命名后的主名，得 %q", names[a])
	}

	if empty, err := pinned.NamesByIDs(nil); err != nil || len(empty) != 0 {
		t.Fatalf("空集合应返回空 map，得 %v（err=%v）", empty, err)
	}
}
