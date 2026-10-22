package repository

import (
	"testing"
)

// 本文件钉住第二批的**回填状态**语义：水位推进、完成标记、重置（清空索引），
// 以及三条「清空 external content 表」的实测差异——其中两条是坑，必须留在测试里，
// 否则后来者很容易把 Reset 改成 `DELETE FROM asset_path_fts` 而看不出问题。
//
// 断言口径与 search_index_repo_test.go 一致：生产查询形态是「MATCH 只取 rowid + 回 asset
// 取真值」，因此命中判断一律用 matchRowIDs（同一包内复用）。

// indexDocCount 返回 FTS5 索引里真实存在的文档数（直读影子表 asset_path_fts_docsize）。
//
// 这里必须读影子表而不是 `SELECT COUNT(*) FROM asset_path_fts`：后者在 external content 表上
// 返回的是**内容表**行数（见 TestCountStarOnExternalContentCountsContentRows），量不出索引条目。
// 影子表只用于测试观测，不要引进生产代码。
func indexDocCount(t *testing.T, repo *SearchIndexStateRepo) int64 {
	t.Helper()
	var n int64
	if err := repo.db.Get(&n, `SELECT COUNT(*) FROM asset_path_fts_docsize`); err != nil {
		t.Fatalf("统计索引条目数：%v", err)
	}
	return n
}

// seedLegacyAssets 绕过仓储直接写 asset，模拟 0049 之前写入、尚未回填的历史行。
func seedLegacyAssets(t *testing.T, assets *AssetRepo, repoID int64, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := assets.DB().Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
			VALUES (?, ?, 'h', 1, 'text/plain', '', '')`, repoID, path); err != nil {
			t.Fatalf("写入历史行 %s：%v", path, err)
		}
	}
}

// TestSearchIndexStateLoadSeededRow 校验状态读取口径：迁移写入的初始行可读，且 Completed()
// 在 completed_at 为 NULL 时为 false（表里没有 done 列）。
func TestSearchIndexStateLoadSeededRow(t *testing.T) {
	assets, _, _ := searchIndexFixture(t)
	repo := NewSearchIndexStateRepo(assets.DB())

	state, err := repo.Load()
	if err != nil {
		t.Fatalf("读状态：%v", err)
	}
	if state.LastID != 0 || state.TotalRows != 0 || state.CompletedAt != nil || state.Completed() {
		t.Fatalf("初始状态不符：%+v", state)
	}
	if state.UpdatedAt == "" {
		t.Fatal("updated_at 不应为空（迁移用 datetime('now') 兜底）")
	}
}

// TestSearchIndexStateWriteBatchAdvancesWatermark 校验「同事务写索引 + 推进水位」：
// 提交后索引可按子串命中、水位等于本批最后一条 id、updated_at 被刷新。
func TestSearchIndexStateWriteBatchAdvancesWatermark(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()
	repo := NewSearchIndexStateRepo(db)

	seedLegacyAssets(t, assets, repoID, []string{"bf/a-1.0.0.jar", "bf/b-2.0.0.jar", "bf/c-3.0.0.jar"})
	before, err := repo.Load()
	if err != nil {
		t.Fatalf("读初始状态：%v", err)
	}

	upper, total, err := repo.SnapshotBounds()
	if err != nil {
		t.Fatalf("取上界：%v", err)
	}
	if total != 3 {
		t.Fatalf("总行数快照 = %d，期望 3", total)
	}
	if err := repo.SaveBounds(total); err != nil {
		t.Fatalf("记录总量：%v", err)
	}
	rows, err := repo.FetchBatch(0, upper, 2)
	if err != nil {
		t.Fatalf("取第一批：%v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("第一批行数 = %d，期望 2（limit 生效）", len(rows))
	}
	lastOfBatch := rows[len(rows)-1].ID
	if err := repo.WriteBatch(rows, lastOfBatch); err != nil {
		t.Fatalf("写第一批：%v", err)
	}

	after, err := repo.Load()
	if err != nil {
		t.Fatalf("读推进后状态：%v", err)
	}
	if after.LastID != lastOfBatch {
		t.Fatalf("水位 = %d，期望 %d", after.LastID, lastOfBatch)
	}
	if after.TotalRows != 3 {
		t.Fatalf("total_rows = %d，期望 3", after.TotalRows)
	}
	if after.UpdatedAt < before.UpdatedAt {
		t.Fatalf("updated_at 未刷新：%q -> %q", before.UpdatedAt, after.UpdatedAt)
	}
	if after.Completed() {
		t.Fatal("只写了第一批，不应标记完成")
	}
	// 第一批的两行已入索引，第三行尚未（水位之后的行不会被 count 到）。
	if got := matchRowIDs(t, db, "1.0.0"); len(got) != 1 {
		t.Fatalf("第一批应命中 1 条，实际 %v", got)
	}
	if got := matchRowIDs(t, db, "3.0.0"); len(got) != 0 {
		t.Fatalf("未回填的第三行不应命中，实际 %v", got)
	}
	if n, err := repo.CountIndexed(after.LastID); err != nil || n != 2 {
		t.Fatalf("CountIndexed(水位) = %d（err=%v），期望 2", n, err)
	}

	// 第二批：续跑口径——从水位之后继续，取空批后置完成。
	rest, err := repo.FetchBatch(after.LastID, upper, 2000)
	if err != nil {
		t.Fatalf("取第二批：%v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("第二批行数 = %d，期望 1", len(rest))
	}
	if err := repo.WriteBatch(rest, rest[len(rest)-1].ID); err != nil {
		t.Fatalf("写第二批：%v", err)
	}
	if err := repo.MarkCompleted(); err != nil {
		t.Fatalf("标记完成：%v", err)
	}
	final, err := repo.Load()
	if err != nil {
		t.Fatalf("读最终状态：%v", err)
	}
	if !final.Completed() {
		t.Fatal("MarkCompleted 之后 Completed() 应为 true")
	}
	if got := matchRowIDs(t, db, "3.0.0"); len(got) != 1 {
		t.Fatalf("第三行应已命中 1 条，实际 %v", got)
	}
	if n := indexDocCount(t, repo); n != 3 {
		t.Fatalf("索引条目数 = %d，期望 3", n)
	}
}

// TestSearchIndexStateWriteBatchIsIdempotent 校验同一批次重复写入不产生重复命中：
// 这是回填与在线写入交叠时的必要语义（同一行可能被写两次）。
func TestSearchIndexStateWriteBatchIsIdempotent(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()
	repo := NewSearchIndexStateRepo(db)

	seedLegacyAssets(t, assets, repoID, []string{"idem/a-1.2.3.jar", "idem/b-1.2.3.jar"})
	upper, _, err := repo.SnapshotBounds()
	if err != nil {
		t.Fatalf("取上界：%v", err)
	}
	rows, err := repo.FetchBatch(0, upper, 2000)
	if err != nil {
		t.Fatalf("取批：%v", err)
	}
	for i := 0; i < 3; i++ {
		if err := repo.WriteBatch(rows, rows[len(rows)-1].ID); err != nil {
			t.Fatalf("第 %d 次写同一批：%v", i+1, err)
		}
	}
	got := matchRowIDs(t, db, "1.2.3")
	if len(got) != 2 {
		t.Fatalf("重复写同一批后命中 = %v，期望 2 条（不重复）", got)
	}
	if n := indexDocCount(t, repo); n != 2 {
		t.Fatalf("索引条目数 = %d，期望 2（同一 rowid 只算一次）", n)
	}
}

// TestSearchIndexStateResetClearsIndexAndWatermark 校验重建用的重置：索引清空、水位与总量归零、
// completed_at 回到 NULL，而 asset 真源一行不动。
func TestSearchIndexStateResetClearsIndexAndWatermark(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()
	repo := NewSearchIndexStateRepo(db)

	seedLegacyAssets(t, assets, repoID, []string{"rs/a-1.0.0.jar", "rs/b-2.0.0.jar"})
	upper, total, err := repo.SnapshotBounds()
	if err != nil {
		t.Fatalf("取上界：%v", err)
	}
	rows, err := repo.FetchBatch(0, upper, 2000)
	if err != nil {
		t.Fatalf("取批：%v", err)
	}
	if err := repo.SaveBounds(total); err != nil {
		t.Fatalf("记录总量：%v", err)
	}
	if err := repo.WriteBatch(rows, rows[len(rows)-1].ID); err != nil {
		t.Fatalf("写批：%v", err)
	}
	if err := repo.MarkCompleted(); err != nil {
		t.Fatalf("标记完成：%v", err)
	}
	if n := indexDocCount(t, repo); n != 2 {
		t.Fatalf("重置前索引条目数 = %d，期望 2", n)
	}

	if err := repo.Reset(); err != nil {
		t.Fatalf("重置：%v", err)
	}
	state, err := repo.Load()
	if err != nil {
		t.Fatalf("读重置后状态：%v", err)
	}
	if state.LastID != 0 || state.TotalRows != 0 || state.CompletedAt != nil {
		t.Fatalf("重置后状态不符：%+v", state)
	}
	if n := indexDocCount(t, repo); n != 0 {
		t.Fatalf("重置后索引条目数 = %d，期望 0", n)
	}
	if got := matchRowIDs(t, db, "1.0.0"); len(got) != 0 {
		t.Fatalf("重置后不应命中，实际 %v", got)
	}
	var assetN int
	if err := db.Get(&assetN, `SELECT COUNT(*) FROM asset`); err != nil {
		t.Fatalf("数 asset 真源：%v", err)
	}
	if assetN != 2 {
		t.Fatalf("重置不得触碰 asset 真源：行数 = %d，期望 2", assetN)
	}
	// 重置后必须能重新灌入（清空不该把索引写成不可写状态）。
	if err := repo.WriteBatch(rows, rows[len(rows)-1].ID); err != nil {
		t.Fatalf("重置后重新写批：%v", err)
	}
	if got := matchRowIDs(t, db, "1.0.0"); len(got) != 1 {
		t.Fatalf("重置并重灌后应命中 1 条，实际 %v", got)
	}
}

// TestSearchIndexStateResetClearsOrphanEntries 校验重置能清掉**孤儿索引行**（asset 行已随仓库
// 级联删除而消失、索引行残留）。这正是「重建」要修的最典型状态，也是 `DELETE FROM` 会静默
// 清不干净的那种输入。
func TestSearchIndexStateResetClearsOrphanEntries(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()
	repo := NewSearchIndexStateRepo(db)

	seedLegacyAssets(t, assets, repoID, []string{"orphan/keep-1.0.0.jar", "orphan/gone-2.0.0.jar"})
	upper, _, err := repo.SnapshotBounds()
	if err != nil {
		t.Fatalf("取上界：%v", err)
	}
	rows, err := repo.FetchBatch(0, upper, 2000)
	if err != nil {
		t.Fatalf("取批：%v", err)
	}
	if err := repo.WriteBatch(rows, rows[len(rows)-1].ID); err != nil {
		t.Fatalf("写批：%v", err)
	}
	// 直接删内容行，留下孤儿索引行（模拟仓库级联删除留下的残留）。
	if _, err := db.Exec(`DELETE FROM asset WHERE path = 'orphan/gone-2.0.0.jar'`); err != nil {
		t.Fatalf("删内容行制造孤儿：%v", err)
	}
	if got := matchRowIDs(t, db, "gone-2.0.0"); len(got) != 1 {
		t.Fatalf("孤儿索引行应仍被 MATCH 命中（这正是需要重建的漂移），实际 %v", got)
	}

	if err := repo.Reset(); err != nil {
		t.Fatalf("重置：%v", err)
	}
	if got := matchRowIDs(t, db, "gone-2.0.0"); len(got) != 0 {
		t.Fatalf("重置后孤儿索引行应被清掉，实际仍命中 %v", got)
	}
	if got := matchRowIDs(t, db, "keep-1.0.0"); len(got) != 0 {
		t.Fatalf("重置后普通索引行也应被清掉，实际仍命中 %v", got)
	}
	if n := indexDocCount(t, repo); n != 0 {
		t.Fatalf("重置后索引条目数 = %d，期望 0", n)
	}
}

// TestDeleteFromFTSLeavesEntriesBehindWithOrphans 钉住一个**必须记住的坑**：
// `DELETE FROM asset_path_fts` 在存在孤儿索引行时会「报成功但只清一半」——语句在孤儿行处
// 提前停住，Exec 返回 nil 错误，孤儿自身与其后条目全部留下。
//
// 因此重建的清空只能用 'delete-all'（searchIndexClearAll）。本用例存在的意义就是把这条实测
// 结论固化成回归断言：若有人把 Reset 改成 DELETE FROM，TestSearchIndexStateResetClearsOrphanEntries
// 与本用例会一起失败。
func TestDeleteFromFTSLeavesEntriesBehindWithOrphans(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()

	seedLegacyAssets(t, assets, repoID, []string{"del/a-1.0.0.jar", "del/b-2.0.0.jar", "del/c-3.0.0.jar"})
	// 手工灌入三行索引（顺序与 id 一致），随后删掉中间一行内容行制造孤儿。
	if _, err := db.Exec(`INSERT INTO asset_path_fts(rowid, path)
		SELECT id, path FROM asset WHERE path LIKE 'del/%'`); err != nil {
		t.Fatalf("灌入索引：%v", err)
	}
	if _, err := db.Exec(`DELETE FROM asset WHERE path = 'del/b-2.0.0.jar'`); err != nil {
		t.Fatalf("删中间行制造孤儿：%v", err)
	}
	before := indexDocCount(t, NewSearchIndexStateRepo(db))
	if before != 3 {
		t.Fatalf("灌入后索引条目数 = %d，期望 3", before)
	}

	if _, err := db.Exec(`DELETE FROM asset_path_fts`); err != nil {
		t.Fatalf("DELETE FROM asset_path_fts 不应报错（这正是它危险的地方）：%v", err)
	}
	after := indexDocCount(t, NewSearchIndexStateRepo(db))
	if after == 0 {
		t.Fatal("实测结论已变化：DELETE FROM 现在能清干净了，需要重新评估 Reset 的实现与注释")
	}
	// 孤儿条目仍在索引里：这是「报成功但只清一半」的直接证据。
	if got := matchRowIDs(t, db, "b-2.0.0"); len(got) != 1 {
		t.Fatalf("孤儿条目应仍留在索引里（DELETE FROM 没清掉它），实际命中 %v", got)
	}
	// 而 'delete-all' 才真正清空。
	if err := searchIndexClearAll(db); err != nil {
		t.Fatalf("delete-all：%v", err)
	}
	if n := indexDocCount(t, NewSearchIndexStateRepo(db)); n != 0 {
		t.Fatalf("delete-all 后索引条目数 = %d，期望 0", n)
	}
}

// TestCountStarOnExternalContentCountsContentRows 钉住 external content 表的 COUNT(*) 语义：
// 它数的是**内容表**行数，不是索引条目数（索引被清空后仍报全量）。
//
// 这条决定了状态端点 `indexed` 的口径必须按水位回 asset 真源计数（见 CountIndexed 的注释），
// 不能用 `COUNT(*) FROM asset_path_fts` 冒充「已索引行数」。
func TestCountStarOnExternalContentCountsContentRows(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()
	repo := NewSearchIndexStateRepo(db)

	seedLegacyAssets(t, assets, repoID, []string{"cnt/a-1.0.0.jar", "cnt/b-2.0.0.jar"})
	var countStar int
	if err := db.Get(&countStar, `SELECT COUNT(*) FROM asset_path_fts`); err != nil {
		t.Fatalf("COUNT(*) FROM asset_path_fts：%v", err)
	}
	if countStar != 2 {
		t.Fatalf("索引为空时 COUNT(*) = %d，期望 2（数的是内容表行数）", countStar)
	}
	if n := indexDocCount(t, repo); n != 0 {
		t.Fatalf("此时索引条目数应为 0，实际 %d", n)
	}
	if indexed, err := repo.CountIndexed(0); err != nil || indexed != 0 {
		t.Fatalf("CountIndexed(0) = %d（err=%v），期望 0", indexed, err)
	}
	if indexed, err := repo.CountIndexed(1 << 62); err != nil || indexed != 2 {
		t.Fatalf("CountIndexed(极大水位) = %d（err=%v），期望 2", indexed, err)
	}
}

// TestMeasureDriftDetectsMissingAndOrphanEntries 钉住漂移原语的三个口径，以及「为什么数
// docsize 影子表」这条实测结论：
//
//   - 漏索引（资产行在、索引里没有）→ MissingRows > 0。这是 completed_at 置位之后任何旁路
//     写入（运维脚本 / 手工 SQL / 未来迁移）留下的**永久静默**状态：回填不会再跑，用户只
//     看到「某些制品搜不到」，所以必须能被动观察；
//   - 孤儿索引行（资产行已删、条目残留）→ OrphanRows > 0，来自仓库级联删除；
//   - 两者一致 → 两个计数都为 0；
//   - **两个方向分开计数**：漏 k 行 + 孤儿 k 行时，旧的单差额口径恰好抵消为 0，会把漏索引
//     藏起来；本用例专门固定这个反例。
//
// 计数手段的选择依据见 MeasureDrift 的注释；本用例顺带把「asset_path_fts_data 不等于文档数」
// 现场再证一次，防止后来者把它当成行数来源。
func TestMeasureDriftDetectsMissingAndOrphanEntries(t *testing.T) {
	assets, _, repoID := searchIndexFixture(t)
	db := assets.DB()
	repo := NewSearchIndexStateRepo(db)

	seedLegacyAssets(t, assets, repoID, []string{
		"drift/a-1.0.0.jar", "drift/b-2.0.0.jar", "drift/c-3.0.0.jar",
	})

	// 空索引 + 3 条资产：三行全是「缺失」。
	drift, err := repo.MeasureDrift()
	if err != nil {
		t.Fatalf("取漂移快照：%v", err)
	}
	if drift.IndexRows != 0 || drift.AssetRows != 3 || drift.MissingRows != 3 ||
		drift.OrphanRows != 0 || !drift.HasMissing() {
		t.Fatalf("空索引时应报 3 行缺失，实际 %+v", drift)
	}
	// 同一时刻 asset_path_fts_data 已经有结构段行：它不是文档数，不能作为行数来源。
	var dataRows int64
	if err := db.Get(&dataRows, `SELECT COUNT(*) FROM asset_path_fts_data`); err != nil {
		t.Fatalf("数 asset_path_fts_data：%v", err)
	}
	if dataRows == drift.IndexRows {
		t.Fatalf("实测结论已变化：asset_path_fts_data 行数(%d)竟等于文档数(%d)，需重新评估 MeasureDrift 的实现",
			dataRows, drift.IndexRows)
	}

	// 构造「asset 有行但索引缺失」：只给前两行写索引，第三行故意漏掉。
	if _, err := db.Exec(`INSERT INTO asset_path_fts(rowid, path)
		SELECT id, path FROM asset WHERE path <> 'drift/c-3.0.0.jar'`); err != nil {
		t.Fatalf("灌入部分索引：%v", err)
	}
	drift, err = repo.MeasureDrift()
	if err != nil {
		t.Fatalf("取漂移快照：%v", err)
	}
	if drift.IndexRows != 2 || drift.AssetRows != 3 || drift.MissingRows != 1 ||
		drift.OrphanRows != 0 || !drift.HasMissing() {
		t.Fatalf("漏掉一行时应报 MissingRows=1，实际 %+v", drift)
	}

	// 补上漏掉的那一行：漂移归零。
	if _, err := db.Exec(`INSERT INTO asset_path_fts(rowid, path)
		SELECT id, path FROM asset WHERE path = 'drift/c-3.0.0.jar'`); err != nil {
		t.Fatalf("补齐索引：%v", err)
	}
	drift, err = repo.MeasureDrift()
	if err != nil {
		t.Fatalf("取漂移快照：%v", err)
	}
	if drift.HasMissing() || drift.OrphanRows != 0 || drift.IndexRows != 3 || drift.AssetRows != 3 {
		t.Fatalf("索引与资产一致时不应报漂移，实际 %+v", drift)
	}

	// 反方向：直接删 asset 行、留下索引行（仓库级联删除的形状）→ 孤儿计数。
	if _, err := db.Exec(`DELETE FROM asset WHERE path = 'drift/b-2.0.0.jar'`); err != nil {
		t.Fatalf("删内容行制造孤儿：%v", err)
	}
	drift, err = repo.MeasureDrift()
	if err != nil {
		t.Fatalf("取漂移快照：%v", err)
	}
	if drift.IndexRows != 3 || drift.AssetRows != 2 || drift.OrphanRows != 1 ||
		drift.MissingRows != 0 || drift.HasMissing() {
		t.Fatalf("孤儿索引行应报 OrphanRows=1，实际 %+v", drift)
	}

	// 正负抵消：再漏掉一条资产行（drift/a），同时保留一条孤儿 → 索引 3 行、资产 2 行，
	// 若只看总数差额会得到 0，旧口径因此完全看不见漏索引。分开计数后 MissingRows=1、
	// OrphanRows=1 各自成立。
	if _, err := db.Exec(`DELETE FROM asset_path_fts WHERE rowid = (
		SELECT id FROM asset WHERE path = 'drift/a-1.0.0.jar')`); err != nil {
		t.Fatalf("删一条索引条目制造漏索引：%v", err)
	}
	drift, err = repo.MeasureDrift()
	if err != nil {
		t.Fatalf("取漂移快照：%v", err)
	}
	if drift.IndexRows != 2 || drift.AssetRows != 2 || drift.MissingRows != 1 || drift.OrphanRows != 1 {
		t.Fatalf("漏索引与孤儿行同时存在时应分别计数（1/1），实际 %+v", drift)
	}
	if !drift.HasMissing() {
		t.Fatalf("正负抵消时仍必须报告漏索引，实际 %+v", drift)
	}
}
