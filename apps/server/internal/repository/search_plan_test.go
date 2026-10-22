package repository

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住 FTS 候选集下推的**执行计划**。
//
// 为什么必须有这一组断言：候选集条件 `id IN (SELECT rowid FROM asset_path_fts WHERE
// asset_path_fts MATCH ?)` 的结果正确性由 search_rewrite_test.go 的等价性用例保证，但
// 「不再全表扫描」这一**性能收益**本身没有任何信号——SQLite 对非相关 IN 子查询有两种计划：
//
//   - FTS 驱动：先取候选 rowid，再按主键逐行回表（`SEARCH asset USING INTEGER PRIMARY KEY
//     (rowid=?)`）。行集由索引决定，与 asset 表规模无关，这才是下推的收益来源。
//   - 退化为扫表：把子查询物化成临时表后从头扫 asset（`SCAN asset ...`）。结果仍然正确
//     （后置 LIKE 还在），但收益归零，且任何功能测试都发现不了。
//
// 一旦哪天有人「顺手」把条件改成带 OR 的形态、或写成语义等价但计划更差的 EXISTS，收益就会
// 静默消失。因此这里直接把计划钉死：出现 `SCAN asset`（asset 作为驱动表被整体扫描）即失败。
//
// 实测记录（modernc.org/sqlite v1.60.1，5 万行语料，有/无 ANALYZE 结果一致）：
//
//	现状 IN 子查询 : SEARCH asset USING INTEGER PRIMARY KEY (rowid=?)
//	                LIST SUBQUERY 1 | SCAN asset_path_fts VIRTUAL TABLE INDEX 0:M1
//	                [USE TEMP B-TREE FOR ORDER BY]                      ← 已是最优，不改
//	EXISTS 变体     : SCAN asset USING INDEX idx_asset_path              ← **更差**（索引全扫）
//	                SCAN asset_path_fts EXISTS VIRTUAL TABLE INDEX 0:=M1
//	OR 内联开关      : SCAN asset USING INDEX idx_asset_path (+ BLOOM FILTER) ← **更差**
//	降级纯 LIKE      : SCAN asset USING INDEX idx_asset_path
//
// 结论：现有形态已经走 FTS 驱动，无需改写 SQL；本文件的用例只负责把这一结论固定下来。
// 高选择性检索词的运行期差距很大（5 万行命中 5 行时 COUNT 168µs vs 24.2ms），
// 所以计划退化不是纸面问题。

// planFixture 建一个带索引、已标记就绪的资产库，供计划断言使用。
func planFixture(t *testing.T, rows int) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "search-plan.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repoID, err := NewRepoRepo(db).Create("plan-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 批量灌数据（比逐条 Upsert 快得多），随后用 'rebuild' 一次把索引灌满。
	if _, err := db.Exec(`
		INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < ?)
		SELECT ?, CASE WHEN i % 4 = 0
				THEN 'libs/alpha/pkg-' || printf('%06d', i) || '-1.0.0.jar'
				ELSE 'libs/other/pkg-' || printf('%06d', i) || '-1.0.0.jar' END,
			'h', 1, 'application/octet-stream', '', '' FROM seq`, rows, repoID); err != nil {
		t.Fatalf("灌数据：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset_path_fts(asset_path_fts) VALUES('rebuild')`); err != nil {
		t.Fatalf("重建索引：%v", err)
	}
	if err := NewSearchIndexStateRepo(db).MarkCompleted(); err != nil {
		t.Fatalf("标记索引完成：%v", err)
	}
	return db
}

// explainPlan 返回 EXPLAIN QUERY PLAN 的 detail 列（与 migrate_test.go 的 explainPlanLines 同形，
// 但本包不依赖 persistence 包的测试辅助，故各自保留一份）。
func explainPlan(t *testing.T, db *persistence.DB, query string, args ...any) []string {
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

// fullScanAsset 匹配「asset 作为驱动表被整体扫描」的计划行。
//
// 用 \b 收尾是为了不把 `SCAN asset_path_fts ...` 误判成扫 asset：asset 之后紧跟下划线时
// 属于另一个标识符（同属一个词，\b 不成立）。这一条同时覆盖 `SCAN asset`（扫表）与
// `SCAN asset USING INDEX idx_asset_path`（索引全扫）—— 两者都是 O(asset 行数)，
// 都意味着候选集下推没有换来任何收益。
var fullScanAsset = regexp.MustCompile(`\bSCAN asset\b`)

// ftsDrivenPlan 断言计划由 FTS 候选集驱动：出现 asset 的主键回表、且没有整体扫描 asset。
func ftsDrivenPlan(t *testing.T, label string, lines []string) {
	t.Helper()
	plan := strings.Join(lines, " | ")
	if m := fullScanAsset.FindString(plan); m != "" {
		t.Errorf("%s：候选集下推未生效，asset 被整体扫描（命中 %q），计划 = %s", label, m, plan)
	}
	// 主键回表是「按候选 rowid 逐行取数」的证据；缺了它说明行集不是由索引给出的。
	if !strings.Contains(plan, "SEARCH asset USING INTEGER PRIMARY KEY (rowid=?)") {
		t.Errorf("%s：未按候选 rowid 回表，计划 = %s", label, plan)
	}
	// FTS 子查询必须真的被扫描（而不是被优化掉）。
	if !strings.Contains(plan, "SCAN asset_path_fts VIRTUAL TABLE INDEX") {
		t.Errorf("%s：计划里没有扫描 asset_path_fts，计划 = %s", label, plan)
	}
}

// TestSearchPlanIsDrivenByFTSIndex 是**计划回归**主用例：四个搜索入口在索引就绪时都必须
// 走 FTS 驱动的计划。逐一覆盖是因为它们各自拼了一条完整语句，任何一条单独退化都要被抓住。
func TestSearchPlanIsDrivenByFTSIndex(t *testing.T) {
	db := planFixture(t, 2000)

	where, args := buildSearchWhereIndexed(SearchFilter{IncludeTerms: []string{"alpha"}, UseFullTextIndex: true}, nil)
	if !strings.Contains(where, "asset_path_fts MATCH") {
		t.Fatalf("前置条件不成立：本次 WHERE 没有下推候选集，where=%q", where)
	}
	const cols = `id, repository_id, path, blob_hash, size, content_type, sha1, md5, created_at, updated_at`

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "计数",
			query: `SELECT COUNT(*) FROM asset WHERE ` + where,
			args:  args,
		},
		{
			name: "分页",
			query: `SELECT ` + cols + ` FROM asset WHERE ` + where +
				` ORDER BY ` + searchOrderBy("path", "asc") + ` LIMIT 51 OFFSET 0`,
			args: args,
		},
		{
			name: "按大小排序",
			query: `SELECT ` + cols + ` FROM asset WHERE ` + where +
				` ORDER BY ` + searchOrderBy("size", "desc") + ` LIMIT 51`,
			args: args,
		},
		{
			name: "聚合扫描",
			query: `SELECT ` + cols + ` FROM asset WHERE ` + where +
				` ORDER BY path ASC, id ASC LIMIT 5001`,
			args: args,
		},
		{
			name: "仓库聚合",
			query: `SELECT repository_id, COUNT(*) AS count FROM asset WHERE ` + where +
				` GROUP BY repository_id ORDER BY count DESC`,
			args: args,
		},
		{
			name: "建议候选",
			query: `SELECT repository_id, path FROM asset WHERE ` + where +
				` ORDER BY path ASC, id ASC LIMIT 11`,
			args: args,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ftsDrivenPlan(t, tc.name, explainPlan(t, db, tc.query, tc.args...))
		})
	}
}

// TestSearchPlanStaysFTSDrivenAfterAnalyze 确认统计信息存在时计划不变。
//
// 为什么单独一条：ANALYZE 之后 SQLite 会按真实行数估算代价，计划有可能从这里翻到
// 「扫 asset 更便宜」。生产库不定期跑 ANALYZE，只在无统计信息时成立的计划断言等于没断言。
func TestSearchPlanStaysFTSDrivenAfterAnalyze(t *testing.T) {
	db := planFixture(t, 2000)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("ANALYZE：%v", err)
	}
	where, args := buildSearchWhereIndexed(SearchFilter{IncludeTerms: []string{"alpha"}, UseFullTextIndex: true}, nil)
	ftsDrivenPlan(t, "ANALYZE 后计数", explainPlan(t, db, `SELECT COUNT(*) FROM asset WHERE `+where, args...))
}

// TestSearchPlanStaysFTSDrivenForLowSelectivityTerm 确认「命中率很高」的检索词同样不翻转。
//
// 低选择性词（本例命中 3/4 的行）会让「扫 asset」在代价上显得不亏，是最容易把计划翻过去
// 的输入。计划在 prepare 期定下、与运行时命中数无关，但值得用最坏输入钉一次。
func TestSearchPlanStaysFTSDrivenForLowSelectivityTerm(t *testing.T) {
	db := planFixture(t, 2000)
	// 语料里四分之三的路径都含 "pkg"，本身就是低选择性词。
	where, args := buildSearchWhereIndexed(SearchFilter{IncludeTerms: []string{"pkg"}, UseFullTextIndex: true}, nil)
	ftsDrivenPlan(t, "低选择性词计数", explainPlan(t, db, `SELECT COUNT(*) FROM asset WHERE `+where, args...))

	var total int
	if err := db.Get(&total, `SELECT COUNT(*) FROM asset WHERE `+where, args...); err != nil {
		t.Fatalf("执行计数：%v", err)
	}
	if total < 1000 {
		t.Fatalf("语料未覆盖低选择性场景：命中 %d 行", total)
	}
}
