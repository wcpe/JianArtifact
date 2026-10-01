package persistence

import (
	"path/filepath"
	"strings"
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

// TestMigration0045BuildsCleanupIndexes 校验 0045 的三项要求：
// 索引真的建出来、真的被 FR-41 清理查询命中、重复迁移后依然存在。
//
// 索引存在但不被 planner 使用等于没建，因此这里不止查 sqlite_master，还用
// EXPLAIN QUERY PLAN 断言计划里出现索引名而不是 SCAN。查询文本镜像自
// internal/repository/storage_cleanup_repo.go：38-45（终态候选集）与 96-98（按 updated_at
// 淘汰缓存资产）——persistence 被 repository 依赖，无法反向 import 那两个常量，
// 此处只能保持同形状；改清理 SQL 的 WHERE / ORDER BY 时必须同步这里。
func TestMigration0045BuildsCleanupIndexes(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	assertCleanupIndexes(t, db)

	// 幂等：重复迁移不得重复建索引或报错。
	if err := db.Migrate(); err != nil {
		t.Fatalf("重复 Migrate：%v", err)
	}
	assertCleanupIndexes(t, db)
}

// assertCleanupIndexes 断言两个索引存在且被清理查询的查询计划命中。
func assertCleanupIndexes(t *testing.T, db *DB) {
	t.Helper()
	cases := []struct {
		name  string
		index string
		query string
		args  []any
	}{
		{
			// 镜像 storage_cleanup_repo.go:39-45：status 只认终态 + updated_at 早于 cutoff。
			name:  "裁剪终态操作候选集",
			index: "idx_asset_mutation_status_updated",
			query: `SELECT id FROM asset_mutation
				WHERE status IN ('completed','rolled_back')
				  AND updated_at < ?
				  AND NOT EXISTS (
				    SELECT 1 FROM blob_quarantine q
				    WHERE q.operation_id = asset_mutation.id AND q.status NOT IN ('deleted','restored')
				  )`,
			args: []any{"2026-01-01 00:00:00"},
		},
		{
			// 镜像 storage_cleanup_repo.go:97-98：仓库内按 updated_at 选最旧的超期资产。
			name:  "淘汰超期缓存资产",
			index: "idx_asset_repo_updated",
			query: `SELECT id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at
				FROM asset WHERE repository_id=? AND updated_at < ? ORDER BY updated_at, path LIMIT ?`,
			args: []any{int64(1), "2026-01-01 00:00:00", 100},
		},
	}
	for _, tc := range cases {
		var exists int
		if err := db.Get(&exists, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, tc.index); err != nil {
			t.Fatalf("查索引 %s：%v", tc.index, err)
		}
		if exists != 1 {
			t.Fatalf("%s：索引 %s 未随迁移建出", tc.name, tc.index)
		}

		// EXPLAIN QUERY PLAN 固定四列，sqlx 要求字段与列一一对应。
		var rows []struct {
			ID     int    `db:"id"`
			Parent int    `db:"parent"`
			NotUsd int    `db:"notused"`
			Detail string `db:"detail"`
		}
		if err := db.Select(&rows, "EXPLAIN QUERY PLAN "+tc.query, tc.args...); err != nil {
			t.Fatalf("%s：EXPLAIN：%v", tc.name, err)
		}
		plan := ""
		for _, row := range rows {
			plan += row.Detail + " | "
		}
		if !strings.Contains(plan, "USING INDEX "+tc.index) && !strings.Contains(plan, "USING COVERING INDEX "+tc.index) {
			t.Errorf("%s：查询计划未命中索引 %s，计划 = %s", tc.name, tc.index, plan)
		}
		if strings.Contains(plan, "SCAN asset_mutation") || strings.Contains(plan, "SCAN asset ") {
			t.Errorf("%s：查询计划退回全表扫，计划 = %s", tc.name, plan)
		}
	}
}
