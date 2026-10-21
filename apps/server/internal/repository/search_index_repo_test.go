package repository

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住 FR-149 第一批的索引同步行为：asset 的全部写入路径（UpsertWithTime /
// DeleteByPath / 资产操作的 apply 与回滚）都必须与 asset_path_fts 同事务维护索引，
// 且 external content 表的几条反直觉规则（改路径的 UPDATE 不重建索引、删除要先删索引
// 再删行、'delete' 命令对未入索引的 rowid 会报错）不被后续改动踩回去。
//
// 断言口径一律用「按 rowid 回表」的查询形态（MATCH 只取 rowid），这也是生产查询的形态：
// 直接读 FTS 表的列值在有孤儿索引行时会报 267，不适合做通用断言。

// searchIndexFixture 打开临时库并迁移，返回仓储与一个已建仓库的 id。
func searchIndexFixture(t *testing.T) (*AssetRepo, *AssetMutationRepo, int64) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "search-index.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repoID, err := NewRepoRepo(db).Create("fts-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	return NewAssetRepo(db), NewAssetMutationRepo(db), repoID
}

// matchRowIDs 返回 FTS 索引中按子串命中的 rowid（升序），用 MATCH + 字面量转义，
// 与生产查询同形：FTS5 的匹配串有独立语法，词必须整体包成字符串字面量。
func matchRowIDs(t *testing.T, db *persistence.DB, term string) []int64 {
	t.Helper()
	var ids []int64
	// 引号双写转义，避免测试词里的特殊字符被当成 FTS5 语法。
	literal := `"` + replaceAllQuotes(term) + `"`
	if err := db.Select(&ids,
		`SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ? ORDER BY rowid`, literal); err != nil {
		t.Fatalf("按 %q 检索索引：%v", term, err)
	}
	return ids
}

func replaceAllQuotes(s string) string {
	out := make([]rune, 0, len(s)+2)
	for _, r := range s {
		if r == '"' {
			out = append(out, '"')
		}
		out = append(out, r)
	}
	return string(out)
}

// TestSearchIndexFollowsAssetInsertAndDelete 验证最基本的正/负两向：
// 新增资产后索引可按子串命中且 rowid 等于 asset.id；删除资产后不再命中。
func TestSearchIndexFollowsAssetInsertAndDelete(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()

	if err := assets.Upsert(repoID, "libs/alpha-1.2.3.jar", "h-alpha", 10, "application/java-archive", "s", "m"); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	stored, err := assets.GetByPath(repoID, "libs/alpha-1.2.3.jar")
	if err != nil {
		t.Fatalf("读回资产：%v", err)
	}
	// 另一条无关资产：确保命中不是「命中全部」。
	if err := assets.Upsert(repoID, "libs/beta-2.0.0.pom", "h-beta", 5, "text/xml", "s", "m"); err != nil {
		t.Fatalf("写入无关资产：%v", err)
	}

	got := matchRowIDs(t, db, "1.2.3")
	if len(got) != 1 || got[0] != stored.ID {
		t.Fatalf("新增后按子串命中 rowid = %v，期望仅 [%d]（asset.id）", got, stored.ID)
	}
	// 同一结论再用「按列值过滤」的形态复核一次：索引里确实有一行，且它归属该资产。
	// 注意这种形态要求索引无孤儿行（孤儿行会让读列值报 267），只适合一致性测试，不适合生产查询。
	var likeIDs []int64
	if err := db.Select(&likeIDs, `SELECT rowid FROM asset_path_fts WHERE path LIKE ?`, "%1.2.3%"); err != nil {
		t.Fatalf("按列值查索引：%v", err)
	}
	if len(likeIDs) != 1 || likeIDs[0] != stored.ID {
		t.Fatalf("按列值命中 rowid = %v，期望仅 [%d]", likeIDs, stored.ID)
	}
	// 生产查询形态：候选集驱动 + 回表取真值。
	var candidates int
	if err := db.Get(&candidates,
		`SELECT COUNT(*) FROM asset WHERE repository_id = ? AND id IN
			(SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)`, repoID, `"1.2.3"`); err != nil {
		t.Fatalf("候选集查询：%v", err)
	}
	if candidates != 1 {
		t.Fatalf("候选集命中 = %d，期望 1", candidates)
	}

	if err := assets.DeleteByPath(repoID, "libs/alpha-1.2.3.jar"); err != nil {
		t.Fatalf("删除资产：%v", err)
	}
	if got := matchRowIDs(t, db, "1.2.3"); len(got) != 0 {
		t.Fatalf("删除后索引仍命中 rowid = %v，期望无命中", got)
	}
	// 按列值形态同样不应命中（删除把索引行真正移除了，而不是留着让它变成孤儿）。
	if err := db.Select(&likeIDs, `SELECT rowid FROM asset_path_fts WHERE path LIKE ?`, "%1.2.3%"); err != nil {
		t.Fatalf("删除后按列值查索引：%v", err)
	}
	if len(likeIDs) != 0 {
		t.Fatalf("删除后按列值仍命中 rowid = %v，期望无命中", likeIDs)
	}
	// 无关资产不受影响。
	if got := matchRowIDs(t, db, "2.0.0"); len(got) != 1 {
		t.Fatalf("无关资产应仍命中 1 条，实际 %v", got)
	}
}

// TestSearchIndexRepeatedUpsertKeepsSingleEntry 验证「同一资产重复 upsert 不产生重复索引行」：
// path 是冲突键、覆盖写时 path 未变，因此索引内容无需变更，命中数必须恒为 1。
func TestSearchIndexRepeatedUpsertKeepsSingleEntry(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()

	var wantID int64
	for i := 0; i < 3; i++ {
		if err := assets.Upsert(repoID, "libs/gamma-9.9.9.jar", fmt.Sprintf("h-%d", i), int64(10+i), "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("第 %d 次 Upsert：%v", i+1, err)
		}
		stored, err := assets.GetByPath(repoID, "libs/gamma-9.9.9.jar")
		if err != nil {
			t.Fatalf("第 %d 次读回：%v", i+1, err)
		}
		if i == 0 {
			wantID = stored.ID
		}
		if stored.ID != wantID {
			t.Fatalf("第 %d 次覆盖写改变了 id：%d -> %d（索引会因此错位）", i+1, wantID, stored.ID)
		}
		if stored.Size != int64(10+i) {
			t.Fatalf("第 %d 次覆盖写未生效：size=%d", i+1, stored.Size)
		}
	}

	if got := matchRowIDs(t, db, "9.9.9"); len(got) != 1 || got[0] != wantID {
		t.Fatalf("重复 upsert 后命中 rowid = %v，期望仅 [%d]", got, wantID)
	}
}

// TestSearchIndexLegacyUnindexedRowIsMaintainable 回归：迁移刚完成、历史行尚未回填（asset 有行、
// asset_path_fts 无索引行）时，覆盖写与删除都必须正常成功。
//
// 这条守的是 FTS5 'delete' 命令的坑：该命令要求 rowid 确实在索引内，否则 SQLite 报
// 267 `database disk image is malformed` 并污染同一连接上针对该表的后续操作。历史行在回填
// 完成前正是「有内容行、无索引行」，若用 'delete' 命令，删除任意历史制品都会失败。
func TestSearchIndexLegacyUnindexedRowIsMaintainable(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()

	// 绕过仓储直接写 asset，模拟 0049 之前写入、尚未回填的历史行。
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
		VALUES (?, ?, ?, ?, ?, '', '')`, repoID, "legacy/x-1.0.0.jar", "h-legacy", int64(3), "text/plain"); err != nil {
		t.Fatalf("写入历史行：%v", err)
	}
	if got := matchRowIDs(t, db, "legacy/x"); len(got) != 0 {
		t.Fatalf("历史行不应已入索引，实际命中 %v", got)
	}

	// 覆盖写历史行：必须成功，且仍不产生索引行（水位回填负责补，在线路径不猜测）。
	if err := assets.Upsert(repoID, "legacy/x-1.0.0.jar", "h-legacy-2", 4, "text/plain", "", ""); err != nil {
		t.Fatalf("覆盖写历史行：%v", err)
	}
	// 删除历史行：必须成功（不因 FTS 'delete' 越界而失败）。
	if err := assets.DeleteByPath(repoID, "legacy/x-1.0.0.jar"); err != nil {
		t.Fatalf("删除历史行：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "legacy/x-1.0.0.jar"); err == nil {
		t.Fatal("历史行删除后不应仍能读到")
	}

	// 删除不存在的路径仍返回 ErrNotFound（不改既有语义）。
	if err := assets.DeleteByPath(repoID, "legacy/x-1.0.0.jar"); err != ErrNotFound {
		t.Fatalf("删除不存在路径应返回 ErrNotFound，实际：%v", err)
	}
}

// TestSearchIndexPrimitiveKeepsEntrySingleAndRemovable 钉住两条原语约定：
//   - 对同一 rowid 重复插入不产生重复命中（回填与在线写入交叠时的幂等要求）；
//   - 移除对「从未入索引」的行也成功，且移除后读列值的查询依然健康（不留残留、不报 267）。
func TestSearchIndexPrimitiveKeepsEntrySingleAndRemovable(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()

	if err := assets.Upsert(repoID, "prim/alpha-1.0.0.jar", "h-1", 1, "text/plain", "", ""); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
	stored, err := assets.GetByPath(repoID, "prim/alpha-1.0.0.jar")
	if err != nil {
		t.Fatalf("读回资产：%v", err)
	}
	// 重复写入同一 (rowid, path)：命中数必须恒为 1。
	for i := 0; i < 2; i++ {
		if err := searchIndexInsert(db, stored.ID, stored.Path); err != nil {
			t.Fatalf("第 %d 次重复写索引：%v", i+1, err)
		}
	}
	if got := matchRowIDs(t, db, "alpha"); len(got) != 1 || got[0] != stored.ID {
		t.Fatalf("重复写后命中 rowid = %v，期望仅 [%d]", got, stored.ID)
	}

	// 移除后：命中消失，且索引整体仍一致（读列值的查询不报 267）。
	if err := searchIndexDelete(db, stored.ID, stored.Path); err != nil {
		t.Fatalf("移除索引行：%v", err)
	}
	if got := matchRowIDs(t, db, "alpha"); len(got) != 0 {
		t.Fatalf("移除后不应命中，实际 %v", got)
	}
	var columnRead int
	if err := db.Get(&columnRead, `SELECT COUNT(*) FROM asset_path_fts WHERE path LIKE ?`, "%alpha%"); err != nil {
		t.Fatalf("移除后读列值失败（索引可能被写坏）：%v", err)
	}
	if columnRead != 0 {
		t.Fatalf("移除后按列值不应命中，实际 %d", columnRead)
	}
}

// TestSearchIndexFollowsMutationApplyAndRollback 覆盖资产操作引擎的两条路径：
// 提交（applyMutationItem：新增 / 删除 / 同路径覆盖写）与回滚（restoreMutationItem）后，
// 索引都必须与 asset 真源一致。
func TestSearchIndexFollowsMutationApplyAndRollback(t *testing.T) {
	assets, mutations, repoID := searchIndexFixture(t)
	db := assets.DB()

	// 预置两条前镜像：一条将被删除、一条将被同路径覆盖写。
	for _, seed := range []struct{ path, hash string }{
		{"m/gone-1.0.0.jar", "h-gone"},
		{"m/same-3.0.0.jar", "h-same-1"},
	} {
		if err := assets.Upsert(repoID, seed.path, seed.hash, 1, "text/plain", "", ""); err != nil {
			t.Fatalf("预置 %s：%v", seed.path, err)
		}
	}

	items := []AssetMutationItem{
		{Ordinal: 0, RepositoryID: repoID, Path: "m/new-2.0.0.jar",
			After: &Asset{RepositoryID: repoID, Path: "m/new-2.0.0.jar", BlobHash: "h-new", Size: 2, ContentType: "text/plain"}},
		{Ordinal: 1, RepositoryID: repoID, Path: "m/gone-1.0.0.jar",
			Before: &Asset{RepositoryID: repoID, Path: "m/gone-1.0.0.jar", BlobHash: "h-gone", Size: 1, ContentType: "text/plain"}},
		{Ordinal: 2, RepositoryID: repoID, Path: "m/same-3.0.0.jar",
			Before: &Asset{RepositoryID: repoID, Path: "m/same-3.0.0.jar", BlobHash: "h-same-1", Size: 1, ContentType: "text/plain"},
			After:  &Asset{RepositoryID: repoID, Path: "m/same-3.0.0.jar", BlobHash: "h-same-2", Size: 7, ContentType: "text/plain"}},
		// 路径变更（先插新路径、再删旧路径）：After.Path 与 item.Path 不同，走 applyMutationItem 的
		// 重命名分支。两个 rowid 各一条索引行，旧路径的词必须随之消失。
		{Ordinal: 3, RepositoryID: repoID, Path: "m/old-4.0.0.jar",
			Before: &Asset{RepositoryID: repoID, Path: "m/old-4.0.0.jar", BlobHash: "h-old", Size: 1, ContentType: "text/plain"},
			After:  &Asset{RepositoryID: repoID, Path: "m/renamed-5.0.0.jar", BlobHash: "h-old", Size: 1, ContentType: "text/plain"}},
	}
	// 重命名分支的载体行需要预先存在。
	if err := assets.Upsert(repoID, "m/old-4.0.0.jar", "h-old", 1, "text/plain", "", ""); err != nil {
		t.Fatalf("预置重命名载体：%v", err)
	}
	const opID = "op-search-index"
	if err := mutations.Begin(opID, items); err != nil {
		t.Fatalf("登记操作：%v", err)
	}
	if err := mutations.Complete(opID, items); err != nil {
		t.Fatalf("提交操作：%v", err)
	}
	if got := matchRowIDs(t, db, "new-2.0.0"); len(got) != 1 {
		t.Fatalf("新增后应命中 1 条，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "gone-1.0.0"); len(got) != 0 {
		t.Fatalf("删除后不应命中，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "same-3.0.0"); len(got) != 1 {
		t.Fatalf("同路径覆盖写后应命中 1 条（不得重复），实际 %v", got)
	}
	if got := matchRowIDs(t, db, "renamed-5.0.0"); len(got) != 1 {
		t.Fatalf("路径变更后新路径应命中 1 条，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "old-4.0.0"); len(got) != 0 {
		t.Fatalf("路径变更后旧路径不应再命中（旧 rowid 的索引行必须清掉），实际 %v", got)
	}

	if err := mutations.RollbackCompleted(opID, items, "测试回滚"); err != nil {
		t.Fatalf("回滚操作：%v", err)
	}
	if got := matchRowIDs(t, db, "new-2.0.0"); len(got) != 0 {
		t.Fatalf("回滚后新增行应从索引消失，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "gone-1.0.0"); len(got) != 1 {
		t.Fatalf("回滚后删除行应重新命中 1 条，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "same-3.0.0"); len(got) != 1 {
		t.Fatalf("回滚后同路径覆盖写应命中 1 条（不得重复），实际 %v", got)
	}
	if got := matchRowIDs(t, db, "renamed-5.0.0"); len(got) != 0 {
		t.Fatalf("回滚后重命名产生的新路径应从索引消失，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "old-4.0.0"); len(got) != 1 {
		t.Fatalf("回滚后旧路径应重新命中 1 条，实际 %v", got)
	}
	if _, err := assets.GetByPath(repoID, "m/gone-1.0.0.jar"); err != nil {
		t.Fatalf("回滚后行应存在：%v", err)
	}
	if _, err := assets.GetByPath(repoID, "m/old-4.0.0.jar"); err != nil {
		t.Fatalf("回滚后旧路径行应存在：%v", err)
	}
}

// TestSearchIndexStateSeededByMigration 校验状态表初始行：水位归零、完成时刻为空，
// 且迁移不做任何回填（此时索引里没有历史行）。
func TestSearchIndexStateSeededByMigration(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()

	if err := assets.Upsert(repoID, "seed/a-1.0.0.jar", "h", 1, "text/plain", "", ""); err != nil {
		t.Fatalf("写入资产：%v", err)
	}
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
}
