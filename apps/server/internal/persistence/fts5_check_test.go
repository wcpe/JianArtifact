package persistence

import (
	"strings"
	"testing"
)

// TestMigration0047BuildsSearchIndex 校验 0047 的两项结构要求：
//   - asset_path_fts 是 fts5 虚拟表，且是 external content 形态（rowid 取自 asset.id），
//     不能退化成「另存一份路径」的普通 FTS 表——那会重复存百万条路径并让删除后的索引行不会自动消失；
//   - search_index_state 只有一行初始状态（水位 0、未完成），且迁移**不做任何回填**。
//
// 迁移不做回填是刻意的（见迁移文件头与 ADR-0030）：把百万级回填塞进迁移会变成一次长时间
// 持写锁的事务，且失败无法断点恢复。这里用「迁移后历史行仍在索引之外」来钉住该约定。
func TestMigration0047BuildsSearchIndex(t *testing.T) {
	db := openTestDB(t)
	// 先建一个仓库与两条历史资产（模拟升级前的既有数据），再跑迁移。
	if err := db.ensureMigrationTable(); err != nil {
		t.Fatalf("创建迁移表：%v", err)
	}
	files, err := migrationFiles()
	if err != nil {
		t.Fatalf("读取迁移列表：%v", err)
	}
	for _, name := range files {
		version := migrationVersion(name)
		if version == "0047" {
			break
		}
		if err := db.applyMigration(name, version); err != nil {
			t.Fatalf("应用迁移 %s：%v", name, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO repository (name, format, type, visibility) VALUES ('fts','raw','hosted','public')`); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size) VALUES (1, 'legacy/a-1.0.0.jar', 'h1', 1)`); err != nil {
		t.Fatalf("写历史资产：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size) VALUES (1, 'legacy/b-2.0.0.pom', 'h2', 1)`); err != nil {
		t.Fatalf("写历史资产：%v", err)
	}

	// 应用 0047 及其后全部迁移。
	if err := db.Migrate(); err != nil {
		t.Fatalf("升级迁移：%v", err)
	}

	// 虚拟表存在，且 sqlite_master 里登记为 fts5 的 external content 形态。
	var ddl string
	if err := db.Get(&ddl, `SELECT sql FROM sqlite_master WHERE name = 'asset_path_fts'`); err != nil {
		t.Fatalf("查 asset_path_fts 定义：%v", err)
	}
	for _, want := range []string{"VIRTUAL TABLE", "fts5", "content='asset'", "content_rowid='id'", "tokenize='trigram'"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("asset_path_fts 定义缺少 %q：%s", want, ddl)
		}
	}

	// 状态表：单行、水位 0、未完成。
	var state struct {
		ID          int64   `db:"id"`
		LastID      int64   `db:"last_id"`
		TotalRows   int64   `db:"total_rows"`
		CompletedAt *string `db:"completed_at"`
	}
	if err := db.Get(&state, `SELECT id, last_id, total_rows, completed_at FROM search_index_state`); err != nil {
		t.Fatalf("读索引状态：%v", err)
	}
	if state.ID != 1 || state.LastID != 0 || state.TotalRows != 0 || state.CompletedAt != nil {
		t.Fatalf("状态初始行不符：%+v", state)
	}
	if _, err := db.Exec(`INSERT INTO search_index_state (id) VALUES (2)`); err == nil {
		t.Fatal("状态表必须只允许单行（id=1），第二行应被 CHECK 拒绝")
	}

	// 迁移不做回填：两条历史行都还不在索引里（回填由启动后的后台任务负责）。
	var indexed int
	if err := db.Get(&indexed,
		`SELECT COUNT(*) FROM asset WHERE id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)`,
		`"legacy/a"`); err != nil {
		t.Fatalf("检索索引：%v", err)
	}
	if indexed != 0 {
		t.Fatalf("迁移不应回填历史行，实际命中 %d 条", indexed)
	}

	// 幂等：重复 Migrate 不报错、状态表仍只有一行。
	if err := db.Migrate(); err != nil {
		t.Fatalf("重复 Migrate：%v", err)
	}
	var rows int
	if err := db.Get(&rows, `SELECT COUNT(*) FROM search_index_state`); err != nil || rows != 1 {
		t.Fatalf("状态表行数 = %d（err=%v），期望 1", rows, err)
	}
}

// TestCheckFTS5SupportSucceeds 校验启动自检在可用的构建上顺利通过，
// 且不在主库留下探测对象（自检用独立内存库）。
func TestCheckFTS5SupportSucceeds(t *testing.T) {
	if err := CheckFTS5Support(); err != nil {
		t.Fatalf("当前构建应支持 fts5 与 trigram，自检却失败：%v", err)
	}
	// 可重复调用（每次都用独立内存库）。
	if err := CheckFTS5Support(); err != nil {
		t.Fatalf("重复自检失败：%v", err)
	}
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate：%v", err)
	}
	if err := CheckFTS5Support(); err != nil {
		t.Fatalf("主库迁移后自检失败：%v", err)
	}
	// 自检不得在主库残留探测表。
	var leftover int
	if err := db.Get(&leftover, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%probe%'`); err != nil {
		t.Fatalf("查探测残留：%v", err)
	}
	if leftover != 0 {
		t.Fatalf("自检不应在主库留下探测对象，实际 %d 个", leftover)
	}
}
