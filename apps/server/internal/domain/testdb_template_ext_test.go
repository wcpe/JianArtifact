package domain_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件是 testdb_template_test.go（package domain）在外部测试包一侧的镜像：
// 两者共用同一个测试二进制，模板由 package domain 的 TestMain 准备一次，
// 这里只负责读取模板路径并为当前用例复制出独立的一份。
//
// 为什么不放在一个文件里：domain 与 domain_test 是两个不同的包，测试辅助函数
// 无法跨包共享；模板的生成与等价性断言留在 package domain 一侧（见该文件的说明）。
const testDBTemplateEnv = "JIAN_TEST_MIGRATED_DB_TEMPLATE"

// migratedTestDBPath 把「刚迁移完的空库」复制到 dir/name 并返回其路径。
// dir 必须已存在；每个调用者都应传入自己的临时目录，副本之间互不共享。
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
