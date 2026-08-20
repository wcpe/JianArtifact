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
	tables := []string{"user", "api_token", "revoked_token", "repository", "acl", "asset", "migration_task", "setting", "repl_change", "repl_entity_version", "replication_apply_log"}
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
	if version != "0014" {
		t.Errorf("迁移版本 = %q，期望 0014", version)
	}
	// 重复迁移不应产生多余记录。
	var applied int
	if err := db.Get(&applied, "SELECT COUNT(*) FROM schema_migrations"); err != nil {
		t.Fatalf("统计迁移记录：%v", err)
	}
	if applied != 14 {
		t.Errorf("已应用迁移数 = %d，期望 14", applied)
	}
}

// TestReplicationEntityVersionMigrationBackfillsLatest 验证升级时从既有变更日志回填最新 LWW 版本。
func TestReplicationEntityVersionMigrationBackfillsLatest(t *testing.T) {
	db := openTestDB(t)
	if err := db.ensureMigrationTable(); err != nil {
		t.Fatalf("创建迁移表：%v", err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatalf("读取迁移：%v", err)
	}
	for _, name := range files {
		version := migrationVersion(name)
		if version == "0013" {
			break
		}
		if err := db.applyMigration(name, version); err != nil {
			t.Fatalf("应用迁移 %s：%v", name, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO repl_change (node_id, op, entity_type, entity_key, data, ts) VALUES
		('node-a', 'put', 'user', 'user:alice', '{}', '2026-08-13T00:00:01Z'),
		('node-b', 'put', 'user', 'user:alice', '{}', '2026-08-13T00:00:02Z'),
		('node-c', 'put', 'user', 'user:alice', '{}', '2026-08-13T00:00:02Z')`); err != nil {
		t.Fatalf("写入旧变更日志：%v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("升级迁移：%v", err)
	}
	var got struct {
		NodeID string `db:"node_id"`
		TS     string `db:"ts"`
	}
	if err := db.Get(&got, `SELECT node_id, ts FROM repl_entity_version WHERE entity_type='user' AND entity_key='user:alice'`); err != nil {
		t.Fatalf("读取回填版本：%v", err)
	}
	if got.NodeID != "node-c" || got.TS != "2026-08-13T00:00:02Z" {
		t.Fatalf("回填版本不符：%+v", got)
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
