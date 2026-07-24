package persistence

import (
	"path/filepath"
	"testing"
)

// openTestDB 在临时目录建库并连接，测试结束自动关闭。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateCreatesSchema(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	// 关键表应存在且可查询。
	tables := []string{"user", "api_token", "revoked_token", "repository", "acl", "asset", "migration_task"}
	for _, tbl := range tables {
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM "+tbl); err != nil {
			t.Errorf("查询表 %s：%v", tbl, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("首次 Migrate：%v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("重复 Migrate：%v", err)
	}
	version, err := db.CurrentVersion()
	if err != nil {
		t.Fatalf("CurrentVersion：%v", err)
	}
	if version != "0005" {
		t.Errorf("迁移版本 = %q，期望 0005", version)
	}
	// 重复迁移不应产生多余记录。
	var applied int
	if err := db.Get(&applied, "SELECT COUNT(*) FROM schema_migrations"); err != nil {
		t.Fatalf("统计迁移记录：%v", err)
	}
	if applied != 5 {
		t.Errorf("已应用迁移数 = %d，期望 5", applied)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	// 外键开启时，插入引用不存在 user 的 api_token 应失败。
	_, err := db.Exec(`INSERT INTO api_token (user_id, name, token_digest) VALUES (999, 'x', 'd')`)
	if err == nil {
		t.Fatal("期望外键约束拒绝插入，却成功了")
	}
}
