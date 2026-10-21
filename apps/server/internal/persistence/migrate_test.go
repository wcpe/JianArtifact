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
	// 关键表应存在且可查询。user_group / user_group_member 为 FR-36 引入（迁移 0046）；
	// backup_verification 为 FR-40 恢复演练报告引入（迁移 0047）。
	tables := []string{"user", "api_token", "revoked_token", "repository", "acl", "asset", "user_group", "user_group_member", "migration_task", "setting", "repl_change", "repl_entity_version", "replication_apply_log", "replication_operation_outbox", "replication_relay_record", "replication_relay_blob", "replication_relay_frontier", "replication_operation_receipt", "backup_package", "backup_verification", "backup_import", "backup_upload", "backup_upload_chunk"}
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

// TestMigration0047AddsBackupOriginAndVerification 校验 0047 的三项要求：
// origin 列真的带上了 CHECK（ADD COLUMN 不会静默丢约束）、既有行取到默认值 manual、
// 演练报告随包级联删除（这是「包没了报告无意义」的实现方式，不能只写在注释里）。
func TestMigration0047AddsBackupOriginAndVerification(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}

	// ① 列存在，且表 DDL 里确实带着 CHECK——SQLite 的 ADD COLUMN 允许带 CHECK，
	// 但若哪天被改写或驱动行为变化，约束消失是无声的，这里显式断言。
	var ddl string
	if err := db.Get(&ddl, `SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='table' AND name='backup_package'`); err != nil {
		t.Fatalf("读取 backup_package DDL：%v", err)
	}
	if !strings.Contains(ddl, "origin") || !strings.Contains(ddl, "'manual'") || !strings.Contains(ddl, "'auto'") {
		t.Fatalf("backup_package 的 origin 列未带来源枚举约束，DDL = %s", ddl)
	}

	// ② 默认值与非法值：省略 origin 的既有写入路径必须落到 manual，非法来源被拒。
	if _, err := db.Exec(`INSERT INTO backup_package (package_id, mode, status, created_at)
		VALUES ('bk-default','hot','done','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("插入不带 origin 的包：%v", err)
	}
	var origin string
	if err := db.Get(&origin, `SELECT origin FROM backup_package WHERE package_id='bk-default'`); err != nil {
		t.Fatalf("读取 origin：%v", err)
	}
	if origin != "manual" {
		t.Fatalf("既有写入路径的 origin 默认值 = %q，期望 manual", origin)
	}
	if _, err := db.Exec(`INSERT INTO backup_package (package_id, mode, status, created_at, origin)
		VALUES ('bk-bogus','hot','done','2026-01-01T00:00:00Z','bogus')`); err == nil {
		t.Fatal("非法 origin 应被 CHECK 拒绝")
	}

	// ③ 演练报告：先写一条，再删包，报告应随之消失（ON DELETE CASCADE）。
	if _, err := db.Exec(`INSERT INTO backup_verification
		(package_id, level, ok, started_at, finished_at, duration_ms)
		VALUES ('bk-default','shallow',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:01Z',1000)`); err != nil {
		t.Fatalf("插入演练报告：%v", err)
	}
	if _, err := db.Exec(`DELETE FROM backup_package WHERE package_id='bk-default'`); err != nil {
		t.Fatalf("删除备份包：%v", err)
	}
	var reports int
	if err := db.Get(&reports, `SELECT COUNT(*) FROM backup_verification WHERE package_id='bk-default'`); err != nil {
		t.Fatalf("统计演练报告：%v", err)
	}
	if reports != 0 {
		t.Fatalf("删除包后演练报告应随级联清除，剩余 %d 条", reports)
	}
}

// capacityAggregateSQL 是仪表盘容量聚合里针对 asset 的那条语句，镜像自
// internal/repository/operations_observability_repo.go 的 aggregateCapacity：
// 唯一的引用列是 size，没有任何 WHERE / ORDER BY 子句。persistence 被 repository
// 依赖、无法反向 import，因此只能保持同形状；改那条 SQL 的引用列时必须同步这里。
const capacityAggregateSQL = `SELECT COUNT(*) AS asset_count, COALESCE(SUM(size),0) AS logical_bytes FROM asset`

// repoStatsSQL 是仓库存储配额（FR-41）的单仓库聚合，镜像自
// internal/repository/asset_repo.go 的 repoStatsQuery。它不属于 0048 的服务对象，
// 但必须确认新索引没有把它的计划带偏。
const repoStatsSQL = `SELECT ? AS repository_id, COUNT(*) AS count, COALESCE(SUM(size), 0) AS total_size FROM asset WHERE repository_id = ?`

// TestMigration0048BuildsAssetSizeCoveringIndex 校验 0048 的三项要求：
//  1. 建出**复合**覆盖索引 (repository_id, size)——两条聚合都在整表/整仓读 size；
//  2. 配额查询（每次上传走两遍的热路径）命中该索引且**覆盖**（不回表）；
//  3. 冗余的 idx_asset_repository 已被删除（其前缀被复合索引覆盖）。
//
// 为什么断言计划而不只断言索引存在：索引存在但不被命中等于白占空间和写放大。
// 为什么必须断言 "COVERING"：本查询只引用 size 与 repository_id，若计划退化成
// `SCAN asset USING INDEX ...`（非覆盖），SQLite 要为每一行回表取值，代价比全表扫更高，
// 说明索引列定义已经不对了，必须报错。
//
// 关于统计信息：断言不依赖 ANALYZE。planner 在「顺序扫表」与「顺序扫覆盖索引」之间取舍时
// 依据索引行宽小于表行宽，与 sqlite_stat1 的基数估计无关；这里**故意不建统计信息**，
// 让测试同时锁住「不需要 ANALYZE 也走覆盖索引」这一前提。
//
// 空表也要断言：迁移刚建库时 asset 为 0 行，若计划在空表上退回 "SCAN asset"，
// 说明该优化在「刚升级、还没数据」的窗口里不生效。
func TestMigration0048BuildsAssetSizeCoveringIndex(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	// 索引存在、且恰好是 (repository_id, size) 两列、顺序固定（前导列必须是 repository_id，
	// 否则配额查询的等值定位用不上它）。
	var columns []string
	if err := db.Select(&columns, `SELECT name FROM pragma_index_info('idx_asset_repo_size') ORDER BY seqno`); err != nil {
		t.Fatalf("读取 idx_asset_repo_size 列定义：%v", err)
	}
	if len(columns) != 2 || columns[0] != "repository_id" || columns[1] != "size" {
		t.Fatalf("idx_asset_repo_size 应为 (repository_id, size)，实际列 = %v", columns)
	}

	// 冗余索引必须已被删除：留着它只会让每次 asset 写入多维护一棵被完全覆盖的 B-Tree。
	var legacy int
	if err := db.Get(&legacy, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_asset_repository'`); err != nil {
		t.Fatalf("检查 idx_asset_repository 是否残留：%v", err)
	}
	if legacy != 0 {
		t.Errorf("idx_asset_repository 应已被 0048 删除（其前缀被 idx_asset_repo_size 覆盖），实际仍存在")
	}

	// 空表 + 有数据两种状态都必须命中：空表用迁移刚完成的状态，有数据用插入后的状态。
	// 有数据状态先建仓库：asset.repository_id 是指向 repository(id) 的外键，
	// 连接串开启了 foreign_keys，直接插资产会被约束拒绝。
	if _, err := db.Exec(`INSERT INTO repository (id, name, format, type) VALUES
		(1, 'cap-a', 'raw', 'hosted'),
		(2, 'cap-b', 'raw', 'hosted')`); err != nil {
		t.Fatalf("写测试仓库：%v", err)
	}
	for _, stage := range []string{"空表", "有数据"} {
		if stage == "有数据" {
			if _, err := db.Exec(`INSERT INTO asset (id, repository_id, path, blob_hash, size) VALUES
				(1, 1, 'a.bin', 'h1', 100),
				(2, 1, 'b.bin', 'h2', 2048),
				(3, 2, 'c.bin', 'h3', 4096)`); err != nil {
				t.Fatalf("写测试资产：%v", err)
			}
		}
		plan := strings.Join(explainPlanLines(t, db, capacityAggregateSQL), " | ")
		if !strings.Contains(plan, "SCAN asset USING COVERING INDEX idx_asset_repo_size") {
			t.Errorf("%s：容量聚合未走 idx_asset_repo_size 覆盖索引，计划 = %s", stage, plan)
		}
	}

	// 结果口径不受索引影响：覆盖索引只改变读法，不改变 COUNT/SUM 的语义。
	var got struct {
		Count int64 `db:"asset_count"`
		Bytes int64 `db:"logical_bytes"`
	}
	if err := db.Get(&got, capacityAggregateSQL); err != nil {
		t.Fatalf("执行容量聚合：%v", err)
	}
	if got.Count != 3 || got.Bytes != 6244 {
		t.Fatalf("容量聚合结果 = %+v，期望 count=3 bytes=6244", got)
	}

	// 配额查询（热路径）：必须是覆盖索引扫描。这里**钉死索引名与 COVERING**，
	// 因为 0048 的主要收益就在这条语句（实测 91.95ms → 1.10ms）；若它退化为回表，
	// 说明复合索引的列顺序或内容被改坏了，必须报错而不是放过。
	repoPlan := strings.Join(explainPlanLines(t, db, repoStatsSQL, int64(1), int64(1)), " | ")
	if !strings.Contains(repoPlan, "USING COVERING INDEX idx_asset_repo_size") {
		t.Errorf("单仓库配额聚合未走 idx_asset_repo_size 覆盖索引，计划 = %s", repoPlan)
	}

	// 幂等：重复迁移不得重复建索引或报错。
	if err := db.Migrate(); err != nil {
		t.Fatalf("重复 Migrate：%v", err)
	}
	again := strings.Join(explainPlanLines(t, db, capacityAggregateSQL), " | ")
	if !strings.Contains(again, "SCAN asset USING COVERING INDEX idx_asset_repo_size") {
		t.Errorf("重复迁移后覆盖索引不再命中，计划 = %s", again)
	}
}

// explainPlanLines 返回 EXPLAIN QUERY PLAN 的 detail 列。
// EXPLAIN QUERY PLAN 固定四列，sqlx 要求字段与列一一对应。
func explainPlanLines(t *testing.T, db *DB, query string, args ...any) []string {
	t.Helper()
	var rows []struct {
		ID     int    `db:"id"`
		Parent int    `db:"parent"`
		NotUsd int    `db:"notused"`
		Detail string `db:"detail"`
	}
	if err := db.Select(&rows, "EXPLAIN QUERY PLAN "+query, args...); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN：%v", err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Detail)
	}
	return out
}
