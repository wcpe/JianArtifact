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
	tables := []string{"user", "api_token", "revoked_token", "repository", "acl", "asset", "migration_task", "setting", "repl_change", "repl_entity_version", "replication_apply_log", "replication_operation_outbox", "replication_relay_record", "replication_relay_blob", "replication_relay_frontier", "replication_operation_receipt", "backup_package", "backup_import", "backup_upload", "backup_upload_chunk"}
	for _, tbl := range tables {
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM "+tbl); err != nil {
			t.Errorf("查询表 %s：%v", tbl, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	files, err := migrationFiles()
	if err != nil {
		t.Fatalf("读取迁移列表：%v", err)
	}
	if len(files) == 0 {
		t.Fatal("迁移列表不能为空")
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("首次 Migrate：%v", err)
	}
	var firstApplied int
	if err := db.Get(&firstApplied, "SELECT COUNT(*) FROM schema_migrations"); err != nil {
		t.Fatalf("统计首次迁移记录：%v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("重复 Migrate：%v", err)
	}
	version, err := db.CurrentVersion()
	if err != nil {
		t.Fatalf("CurrentVersion：%v", err)
	}
	if want := migrationVersion(files[len(files)-1]); version != want {
		t.Errorf("迁移版本 = %q，期望 %s", version, want)
	}
	// 重复迁移不应产生多余记录，且首次应完整应用当前嵌入的迁移列表。
	var applied int
	if err := db.Get(&applied, "SELECT COUNT(*) FROM schema_migrations"); err != nil {
		t.Fatalf("统计迁移记录：%v", err)
	}
	if applied != firstApplied || applied != len(files) {
		t.Errorf("已应用迁移数 = %d，首次 = %d，迁移列表 = %d", applied, firstApplied, len(files))
	}
}

func TestMigration0033DiscardsUnverifiedDevelopmentRelayHistory(t *testing.T) {
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
		if version == "0033" {
			break
		}
		if err := db.applyMigration(name, version); err != nil {
			t.Fatalf("应用迁移 %s：%v", name, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO replication_relay_record
		(stream_generation, source_node, source_seq, record_type, record_json, created_at)
		VALUES ('old-generation','old-root',1,'change','{}',datetime('now'))`); err != nil {
		t.Fatalf("写旧 relay record：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO replication_relay_blob
		(stream_generation, source_node, source_seq, blob_hash) VALUES ('old-generation','old-root',1,'old-hash')`); err != nil {
		t.Fatalf("写旧 relay blob：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO replication_relay_frontier (stream_generation, forwardable_seq) VALUES ('old-generation',1)`); err != nil {
		t.Fatalf("写旧 relay frontier：%v", err)
	}
	for key, value := range map[string]string{
		"repl:watermark:https://old.example":         "1",
		"repl:stream_generation:https://old.example": "old-generation",
		"repl:stream_valid:https://old.example":      "true",
	} {
		if _, err := db.Exec(`INSERT INTO setting (key,value) VALUES (?,?)`, key, value); err != nil {
			t.Fatalf("写旧父流 setting：%v", err)
		}
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("升级到 0033：%v", err)
	}
	for _, table := range []string{"replication_relay_record", "replication_relay_blob", "replication_relay_frontier"} {
		var count int
		if err := db.Get(&count, `SELECT COUNT(*) FROM `+table); err != nil || count != 0 {
			t.Fatalf("0033 必须清空未经 provenance 验证的 %s：count=%d err=%v", table, count, err)
		}
	}
	var settings int
	if err := db.Get(&settings, `SELECT COUNT(*) FROM setting WHERE key LIKE 'repl:watermark:%' OR key LIKE 'repl:stream_generation:%' OR key LIKE 'repl:stream_valid:%'`); err != nil || settings != 0 {
		t.Fatalf("0033 必须清除旧父边 lineage 并强制水位 0 重建：count=%d err=%v", settings, err)
	}
	var originColumn int
	if err := db.Get(&originColumn, `SELECT COUNT(*) FROM pragma_table_info('asset_mutation') WHERE name='origin'`); err != nil || originColumn != 1 {
		t.Fatalf("0033 必须登记 received intent 来源：count=%d err=%v", originColumn, err)
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
