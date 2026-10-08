package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件提供「整个测试进程只真实迁移一次」的建库夹具。
//
// 要解决的问题：domain 包的用例几乎每一个都需要「一个刚 Open + Migrate 完的空库」。
// race 模式下单次 Open+Migrate 约 1.7s（纯 Go SQLite 的每条 DDL 都被竞态插桩放大），
// 而本包有 100+ 处建库；同一套 schema 被逐字节重复执行 100+ 次，累计接近整个包
// race 时长的一半。这是纯粹的重复劳动，与用例要验证的场景无关。
//
// 做法：TestMain 里真实执行一次 Open + Migrate，把它做成一份只读模板；此后每个夹具
// 从模板复制出自己独立的一份。各用例仍然持有各自的库文件、各自的连接与各自的写入，
// 只是不再重复执行同一套建库 DDL。模板本身不会被任何用例写入。
//
// 等价性由 TestFixtureTemplateMatchesFreshMigration 独立断言（该用例自己现场
// Open + Migrate 一份，再与模板副本逐表逐行比对）——因此真实迁移在每次测试运行中
// 仍然会被执行与校验，只是不再重复 100+ 次。
const testDBTemplateEnv = "JIAN_TEST_MIGRATED_DB_TEMPLATE"

// TestMain 在全部用例之前准备好迁移模板，结束后清理临时目录。
// 本包（domain）与外部测试包（domain_test）共用同一个测试二进制，模板只准备一次，
// 外部测试包通过 testDBTemplateEnv 读取模板路径。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "jianartifact-domain-fixture-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建测试夹具目录失败：%v\n", err)
		os.Exit(1)
	}
	templatePath := filepath.Join(dir, "migrated.db")
	if err := migrateFreshDB(templatePath); err != nil {
		fmt.Fprintf(os.Stderr, "生成迁移模板失败：%v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	_ = os.Setenv(testDBTemplateEnv, templatePath)

	code := m.Run()

	_ = os.Unsetenv(testDBTemplateEnv)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// migrateFreshDB 在 path 现场建一个全新实例并跑完整迁移（即"今天每个夹具都在做的事"）。
func migrateFreshDB(path string) error {
	db, err := persistence.Open(path)
	if err != nil {
		return err
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		return err
	}
	return db.Close()
}

// migratedTestDBPath 把「刚迁移完的空库」复制到 dir/name 并返回其路径。
// dir 必须已存在；每个调用者都应传入自己的临时目录，副本之间互不共享。
//
// 复制走 persistence.SnapshotFile（VACUUM INTO）：它是仓库既有的 SQLite 一致性
// 快照原语，产出已 checkpoint、不含 -wal/-shm 残留的单文件，因此不依赖模板文件
// 关闭时机的巧合。
func migratedTestDBPath(t *testing.T, dir, name string) string {
	t.Helper()
	dst := filepath.Join(dir, name)
	templatePath := os.Getenv(testDBTemplateEnv)
	if templatePath == "" {
		// 兜底：模板不可用（例如单独驱动某个测试二进制而未走 TestMain）时退回现场迁移。
		if err := migrateFreshDB(dst); err != nil {
			t.Fatalf("现场迁移数据库：%v", err)
		}
		return dst
	}
	if err := persistence.SnapshotFile(templatePath, dst); err != nil {
		t.Fatalf("从迁移模板复制夹具库：%v", err)
	}
	return dst
}

// openMigratedTestDB 在指定目录建一个「刚迁移完的空库」并打开，注册关闭清理。
// 供同时需要持有该目录（放 blobstore、备份数据目录等）的夹具使用。
func openMigratedTestDB(t *testing.T, dir, name string) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(migratedTestDBPath(t, dir, name))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migratedTestDB 用独立临时目录建一个「刚迁移完的空库」并打开，注册关闭清理。
func migratedTestDB(t *testing.T, name string) *persistence.DB {
	t.Helper()
	return openMigratedTestDB(t, t.TempDir(), name)
}

// TestFixtureTemplateMatchesFreshMigration 守护夹具等价性：模板副本与现场
// Open + Migrate 得到的库，必须在迁移版本、schema 对象集合、每张表的行数以及
// 自增序列上完全一致。模板机制若失效（例如副本丢掉了一部分内容），这里会立刻失败。
func TestFixtureTemplateMatchesFreshMigration(t *testing.T) {
	t.Parallel()

	freshDir := t.TempDir()
	freshPath := filepath.Join(freshDir, "fresh.db")
	if err := migrateFreshDB(freshPath); err != nil {
		t.Fatalf("现场迁移基准库：%v", err)
	}
	fresh := openFixtureDB(t, freshPath)

	copyPath := migratedTestDBPath(t, t.TempDir(), "copy.db")
	fromTemplate := openFixtureDB(t, copyPath)

	if got, want := fixtureVersion(t, fromTemplate), fixtureVersion(t, fresh); got != want {
		t.Fatalf("迁移版本不一致：副本=%q 现场=%q", got, want)
	}

	freshObjects := fixtureSchemaObjects(t, fresh)
	copyObjects := fixtureSchemaObjects(t, fromTemplate)
	if len(copyObjects) != len(freshObjects) {
		t.Fatalf("schema 对象数不一致：副本=%d 现场=%d", len(copyObjects), len(freshObjects))
	}
	for i := range freshObjects {
		if copyObjects[i] != freshObjects[i] {
			t.Fatalf("schema 对象第 %d 项不一致：\n副本=%q\n现场=%q", i, copyObjects[i], freshObjects[i])
		}
	}

	freshCounts := fixtureTableCounts(t, fresh, freshObjects)
	copyCounts := fixtureTableCounts(t, fromTemplate, copyObjects)
	for table, want := range freshCounts {
		if got := copyCounts[table]; got != want {
			t.Fatalf("表 %s 行数不一致：副本=%d 现场=%d", table, got, want)
		}
	}
	if len(copyCounts) != len(freshCounts) {
		t.Fatalf("表数量不一致：副本=%d 现场=%d", len(copyCounts), len(freshCounts))
	}

	if got, want := fixtureSequence(t, fromTemplate), fixtureSequence(t, fresh); got != want {
		t.Fatalf("自增序列不一致：副本=%q 现场=%q", got, want)
	}
}

func openFixtureDB(t *testing.T, path string) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(path)
	if err != nil {
		t.Fatalf("打开 %s：%v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fixtureVersion(t *testing.T, db *persistence.DB) string {
	t.Helper()
	var version string
	if err := db.Get(&version, `SELECT COALESCE(MAX(version), '') FROM schema_migrations`); err != nil {
		t.Fatalf("读取迁移版本：%v", err)
	}
	return version
}

// fixtureSchemaObjects 返回按 type/name 排序的 schema 对象（名字与建表语句）。
func fixtureSchemaObjects(t *testing.T, db *persistence.DB) []string {
	t.Helper()
	var rows []struct {
		Type string `db:"type"`
		Name string `db:"name"`
		SQL  string `db:"sql"`
	}
	if err := db.Select(&rows, `SELECT type, name, COALESCE(sql, '') AS sql FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`); err != nil {
		t.Fatalf("读取 schema 对象：%v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Type+"|"+r.Name+"|"+r.SQL)
	}
	return out
}

// fixtureTableCounts 返回每张业务表的行数。
func fixtureTableCounts(t *testing.T, db *persistence.DB, objects []string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, obj := range objects {
		parts := splitSchemaObject(obj)
		if parts[0] != "table" {
			continue
		}
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM "`+parts[1]+`"`); err != nil {
			t.Fatalf("统计表 %s 行数：%v", parts[1], err)
		}
		counts[parts[1]] = n
	}
	return counts
}

// fixtureSequence 返回 sqlite_sequence 的全部内容（自增序列残留会被一并比对）。
func fixtureSequence(t *testing.T, db *persistence.DB) string {
	t.Helper()
	var exists int
	if err := db.Get(&exists, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sqlite_sequence'`); err != nil {
		t.Fatalf("检查 sqlite_sequence：%v", err)
	}
	if exists == 0 {
		return ""
	}
	var rows []struct {
		Name string `db:"name"`
		Seq  int64  `db:"seq"`
	}
	if err := db.Select(&rows, `SELECT name, seq FROM sqlite_sequence ORDER BY name`); err != nil {
		t.Fatalf("读取 sqlite_sequence：%v", err)
	}
	out := ""
	for _, r := range rows {
		out += fmt.Sprintf("%s=%d;", r.Name, r.Seq)
	}
	return out
}

// splitSchemaObject 拆开 fixtureSchemaObjects 产出的 "type|name|sql"。
func splitSchemaObject(obj string) [3]string {
	for i := 0; i < len(obj); i++ {
		if obj[i] == '|' {
			for j := i + 1; j < len(obj); j++ {
				if obj[j] == '|' {
					return [3]string{obj[:i], obj[i+1 : j], obj[j+1:]}
				}
			}
		}
	}
	return [3]string{"", "", ""}
}
