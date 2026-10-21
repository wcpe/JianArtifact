package repository

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住 FR-149 第三批的**查询改写**：把可下推的包含词换成
// `id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)` 候选集条件之后，
// 结果集与 total 必须与旧的纯 LIKE 实现（buildSearchWhere，保留为 oracle）**逐条一致**。
//
// 断言口径：
//   - 等价性一律比较「排序后的 id 列表 + total + facet」，不比较顺序（排序由 searchOrderBy 决定，
//     不在本批变更范围）；
//   - 每个用例都同时跑 oracle（UseFullTextIndex=false）与新实现（true），任一处不同即失败。
//
// 为什么必须有这组测试：trigram 的下限是 3 字符、大小写折叠只覆盖 ASCII、FTS5 有独立语法，
// 任何一处判断失误都会表现为「静默漏结果」——那是最难在实机发现、也是本批最危险的错误模式。

// rewriteFixture 建库并把语料写进 asset（Upsert 会在同一事务内同步 FTS 索引）。
type rewriteFixture struct {
	assets *AssetRepo
	state  *SearchIndexStateRepo
	repo1  int64
	repo2  int64
	repo3  int64
}

// rewriteCorpus 是等价性测试的语料：覆盖大小写、版本号前缀、特殊字符、中文、扩展名与多仓库。
var rewriteCorpus = []string{
	"libs/alpha-1.2.3.jar",
	"libs/alpha-1.2.30.jar",
	"libs/alpha-1.2.3-sources.jar",
	"libs/beta-2.0.0.pom",
	"libs/gamma-3.0.JAR",
	"libs/delta-1.17.12.jar",
	"libs/delta-1.17.120.jar",
	"docs/readme.txt",
	"docs/alpha-notes.md",
	`weird/100%_done.txt`,
	`weird/quote"inside.txt`,
	`weird/a AND b.txt`,
	`weird/star*name.txt`,
	`weird/colon:name.txt`,
	`weird/paren(1).txt`,
	`weird/OR maybe.txt`,
	`weird/NEAR caret.txt`,
	`weird/caret^name.txt`,
	`weird/back\slash.txt`,
	`weird/apostrophe's.txt`,
	"中文/标签-1.0.jar",
	"中文/中文标签-2.0.jar",
	"npm/@scope/pkg",
	"npm/@scope/pkg-extra",
	// 下面两条让「短词」在语料里具备**区分性**：短词 `1.` 只出现在版本号路径上，
	// 而 beta-only 不含它——这样「短词被下推或被丢弃」都会让等价性测试立刻失败。
	"zzz/beta-only.txt",
	"zzz/beta-elsewhere.txt",
}

// newRewriteFixture 准备一份带索引的语料库（三个仓库、语料按序号轮流分配）。
func newRewriteFixture(t *testing.T) *rewriteFixture {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "search-rewrite.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := NewRepoRepo(db)
	var ids []int64
	for _, name := range []string{"rw-one", "rw-two", "rw-three"} {
		id, err := repos.Create(name, "raw", "hosted", "public", "")
		if err != nil {
			t.Fatalf("建仓库 %s：%v", name, err)
		}
		ids = append(ids, id)
	}
	assets := NewAssetRepo(db)
	for i, path := range rewriteCorpus {
		if err := assets.Upsert(ids[i%len(ids)], path, "h", int64(i+1), "application/octet-stream", "s", "m"); err != nil {
			t.Fatalf("写入资产 %s：%v", path, err)
		}
	}
	// 置索引为已完成：等价性对照要在「索引可用」的前提下进行。
	state := NewSearchIndexStateRepo(db)
	if err := state.MarkCompleted(); err != nil {
		t.Fatalf("标记索引完成：%v", err)
	}
	return &rewriteFixture{assets: assets, state: state, repo1: ids[0], repo2: ids[1], repo3: ids[2]}
}

// runIDs 跑一次搜索并返回排序后的 id 列表、total 与 facet 计数合计。
func (fx *rewriteFixture) runIDs(t *testing.T, f SearchFilter, repoIDs []int64) ([]int64, int, int) {
	t.Helper()
	ctx := context.Background()
	assets, total, err := fx.assets.SearchByFilter(ctx, f, repoIDs, "path", "asc", 1000, 0)
	if err != nil {
		t.Fatalf("搜索失败（filter=%+v repoIDs=%v）：%v", f, repoIDs, err)
	}
	facets, err := fx.assets.SearchFacetsByFilter(ctx, f, repoIDs)
	if err != nil {
		t.Fatalf("facet 聚合失败：%v", err)
	}
	ids := make([]int64, 0, len(assets))
	for _, a := range assets {
		ids = append(ids, a.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	facetSum := 0
	for _, facet := range facets {
		facetSum += facet.Count
	}
	return ids, total, facetSum
}

// TestSearchIndexEquivalence 是**双实现对照**主用例：同一查询串在旧 LIKE 实现（oracle）与新
// FTS 实现下，命中的 id 集合、total 与 facet 计数必须完全一致。
func TestSearchIndexEquivalence(t *testing.T) {
	fx := newRewriteFixture(t)

	cases := []struct {
		name    string
		filter  SearchFilter
		repoIDs []int64
	}{
		{name: "单义词", filter: SearchFilter{IncludeTerms: []string{"alpha"}}},
		{name: "多词 AND", filter: SearchFilter{IncludeTerms: []string{"alpha", "jar"}}},
		{name: "多词 AND 无命中组合", filter: SearchFilter{IncludeTerms: []string{"alpha", "pom"}}},
		{name: "版本号子串", filter: SearchFilter{IncludeTerms: []string{"1.17.12"}}},
		{name: "版本号 1.2.3", filter: SearchFilter{IncludeTerms: []string{"1.2.3"}}},
		{name: "排除词", filter: SearchFilter{IncludeTerms: []string{"alpha"}, ExcludeTerms: []string{"1.2.30"}}},
		{name: "纯排除词", filter: SearchFilter{ExcludeTerms: []string{"alpha"}}},
		{name: "扩展名", filter: SearchFilter{IncludeExts: []string{"jar"}}},
		{name: "扩展名与小写口径", filter: SearchFilter{IncludeExts: []string{"JAR"}}},
		{name: "排除扩展名", filter: SearchFilter{ExcludeExts: []string{"jar"}}},
		{name: "扩展名加包含词", filter: SearchFilter{IncludeTerms: []string{"alpha"}, IncludeExts: []string{"jar"}}},
		{name: "引号短语含空格", filter: SearchFilter{IncludeTerms: []string{"OR maybe"}}},
		{name: "引号短语与短词并存", filter: SearchFilter{IncludeTerms: []string{"OR maybe", "or"}}},
		{name: "百分号与下划线", filter: SearchFilter{IncludeTerms: []string{"100%_done"}}},
		{name: "百分号短词", filter: SearchFilter{IncludeTerms: []string{"%"}}},
		{name: "下划线短词", filter: SearchFilter{IncludeTerms: []string{"_d"}}},
		{name: "双引号", filter: SearchFilter{IncludeTerms: []string{`quote"inside`}}},
		{name: "单个双引号", filter: SearchFilter{IncludeTerms: []string{`"`}}},
		{name: "星号", filter: SearchFilter{IncludeTerms: []string{"star*name"}}},
		{name: "FTS 布尔词", filter: SearchFilter{IncludeTerms: []string{"a AND b"}}},
		{name: "单独大写 AND", filter: SearchFilter{IncludeTerms: []string{"AND"}}},
		{name: "冒号", filter: SearchFilter{IncludeTerms: []string{"colon:name"}}},
		{name: "括号", filter: SearchFilter{IncludeTerms: []string{"paren(1)"}}},
		{name: "NEAR", filter: SearchFilter{IncludeTerms: []string{"NEAR caret"}}},
		{name: "脱字符", filter: SearchFilter{IncludeTerms: []string{"caret^name"}}},
		{name: "反斜杠", filter: SearchFilter{IncludeTerms: []string{`back\slash`}}},
		{name: "单引号", filter: SearchFilter{IncludeTerms: []string{"apostrophe's"}}},
		{name: "单字符词", filter: SearchFilter{IncludeTerms: []string{"a"}}},
		{name: "双字符词", filter: SearchFilter{IncludeTerms: []string{"al"}}},
		{name: "两个汉字", filter: SearchFilter{IncludeTerms: []string{"标签"}}},
		{name: "三个汉字", filter: SearchFilter{IncludeTerms: []string{"中文标"}}},
		{name: "短词与长词混合", filter: SearchFilter{IncludeTerms: []string{"a", "alpha"}}},
		{name: "短词具备区分性（短词命中集与长词不同）", filter: SearchFilter{IncludeTerms: []string{"1.", "beta"}}},
		{name: "短词排除掉全部长词命中", filter: SearchFilter{IncludeTerms: []string{"beta", "only"}}},
		{name: "仅短词", filter: SearchFilter{IncludeTerms: []string{"1", "2"}}},
		{name: "大小写互命中（小写查大写）", filter: SearchFilter{IncludeTerms: []string{"gamma"}}},
		{name: "大小写互命中（大写查小写）", filter: SearchFilter{IncludeTerms: []string{"GAMMA"}}},
		{name: "非 ASCII 不做大小写折叠", filter: SearchFilter{IncludeTerms: []string{"标签-1.0"}}},
		{name: "scope 路径", filter: SearchFilter{IncludeTerms: []string{"@scope/pkg"}}},
		{name: "限定单仓库", filter: SearchFilter{IncludeTerms: []string{"alpha"}}, repoIDs: []int64{fx.repo1}},
		{name: "限定多仓库", filter: SearchFilter{IncludeTerms: []string{"alpha"}}, repoIDs: []int64{fx.repo1, fx.repo3}},
		{name: "空条件", filter: SearchFilter{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oracle := tc.filter
			oracle.UseFullTextIndex = false
			gotIDs, gotTotal, gotFacets := fx.runIDs(t, oracle, tc.repoIDs)

			indexed := tc.filter
			indexed.UseFullTextIndex = true
			wantIDs, wantTotal, wantFacets := fx.runIDs(t, indexed, tc.repoIDs)

			if gotTotal != wantTotal {
				t.Fatalf("total 不一致：oracle=%d FTS=%d（filter=%+v）", gotTotal, wantTotal, tc.filter)
			}
			if gotFacets != wantFacets {
				t.Fatalf("facet 计数合计不一致：oracle=%d FTS=%d", gotFacets, wantFacets)
			}
			if len(gotIDs) != len(wantIDs) {
				t.Fatalf("命中数不一致：oracle=%v FTS=%v", gotIDs, wantIDs)
			}
			for i := range gotIDs {
				if gotIDs[i] != wantIDs[i] {
					t.Fatalf("命中 id 不一致：oracle=%v FTS=%v", gotIDs, wantIDs)
				}
			}
		})
	}
}

// TestSearchWhereIndexedPushDown 钉住「哪些查询真的走了索引、哪些必须降级」。
// 只有真的下推了，等价性测试才有意义（否则它只是在拿 LIKE 跟 LIKE 比）。
func TestSearchWhereIndexedPushDown(t *testing.T) {
	cases := []struct {
		name     string
		filter   SearchFilter
		wantPush bool
	}{
		{name: "索引就绪且有长词则下推", filter: SearchFilter{IncludeTerms: []string{"alpha"}, UseFullTextIndex: true}, wantPush: true},
		{name: "索引未就绪不下推", filter: SearchFilter{IncludeTerms: []string{"alpha"}, UseFullTextIndex: false}},
		{name: "无包含词不下推", filter: SearchFilter{IncludeExts: []string{"jar"}, UseFullTextIndex: true}},
		{name: "仅排除词不下推", filter: SearchFilter{ExcludeTerms: []string{"alpha"}, UseFullTextIndex: true}},
		{name: "单字符词不下推", filter: SearchFilter{IncludeTerms: []string{"a"}, UseFullTextIndex: true}},
		{name: "双字符词不下推", filter: SearchFilter{IncludeTerms: []string{"ab"}, UseFullTextIndex: true}},
		{name: "两个汉字不下推", filter: SearchFilter{IncludeTerms: []string{"标签"}, UseFullTextIndex: true}},
		{name: "三个汉字下推", filter: SearchFilter{IncludeTerms: []string{"中文标"}, UseFullTextIndex: true}, wantPush: true},
		{name: "长短混合仍下推长词", filter: SearchFilter{IncludeTerms: []string{"a", "alpha"}, UseFullTextIndex: true}, wantPush: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args := buildSearchWhereIndexed(tc.filter, nil)
			pushed := strings.Contains(where, "asset_path_fts MATCH")
			if pushed != tc.wantPush {
				t.Fatalf("下推判定=%v，期望=%v（where=%q）", pushed, tc.wantPush, where)
			}
			if pushed {
				// 候选集条件必须是「只读 rowid 的子查询」，且 MATCH 参数是字面量短语。
				if !strings.Contains(where, "id IN (SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)") {
					t.Fatalf("候选集条件形态不对：%q", where)
				}
				if len(args) == 0 {
					t.Fatal("缺少 MATCH 参数")
				}
				match, ok := args[0].(string)
				if !ok || !strings.HasPrefix(match, `"`) {
					t.Fatalf("MATCH 参数不是字面量短语：%#v", args[0])
				}
			}
			// 旧实现（oracle）永远不出现 FTS。
			oracleWhere, _ := buildSearchWhere(tc.filter, nil)
			if strings.Contains(oracleWhere, "asset_path_fts") {
				t.Fatalf("oracle 不应引用 FTS：%q", oracleWhere)
			}
		})
	}
}

// TestFTSPhraseLiteralEscaping 钉住 FTS5 匹配串的转义：用户输入必须整体成为字面量短语，
// 内部 `"` 双写，任何 FTS5 语法字符都不得被当成语法。
func TestFTSPhraseLiteralEscaping(t *testing.T) {
	cases := []struct {
		term string
		want string
	}{
		{term: "alpha", want: `"alpha"`},
		{term: "a AND b", want: `"a AND b"`},
		{term: "OR", want: `"OR"`},
		{term: "NOT", want: `"NOT"`},
		{term: "star*", want: `"star*"`},
		{term: "^caret", want: `"^caret"`},
		{term: "colon:name", want: `"colon:name"`},
		{term: "paren(1)", want: `"paren(1)"`},
		{term: `quote"inside`, want: `"quote""inside"`},
		{term: `"`, want: `""""`},
		{term: `a"b"c`, want: `"a""b""c"`},
		{term: "NEAR(a b)", want: `"NEAR(a b)"`},
		{term: "中文 标签", want: `"中文 标签"`},
	}
	for _, tc := range cases {
		t.Run(tc.term, func(t *testing.T) {
			if got := ftsPhraseLiteral(tc.term); got != tc.want {
				t.Fatalf("ftsPhraseLiteral(%q)=%q，期望=%q", tc.term, got, tc.want)
			}
		})
	}
	// 多词用 AND 组合，且每个词都是字面量。
	if got := ftsMatchExpression([]string{"a b", `c"d`}); got != `"a b" AND "c""d"` {
		t.Fatalf("ftsMatchExpression 组合错误：%q", got)
	}
}

// TestSearchIndexEscapeIsSemantic 验证转义后的匹配串在真实库上确实是「按字面量包含」，
// 而不是布尔查询：`a OR b` 只能命中真的包含 `a OR b` 的路径。
func TestSearchIndexEscapeIsSemantic(t *testing.T) {
	fx := newRewriteFixture(t)
	ctx := context.Background()

	// 语料里既有 `weird/a AND b.txt`，也有 `weird/OR maybe.txt`。
	// 若把 `a OR b` 当布尔查询，会命中大量含 a 或 b 的路径。
	f := SearchFilter{IncludeTerms: []string{"a OR b"}, UseFullTextIndex: true}
	assets, total, err := fx.assets.SearchByFilter(ctx, f, nil, "path", "asc", 100, 0)
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if total != 0 || len(assets) != 0 {
		t.Fatalf("`a OR b` 应按字面量匹配（语料里没有该子串），实际命中 total=%d items=%d", total, len(assets))
	}

	// 同一查询在 oracle 下也必须为 0，两侧一致。
	oracle := SearchFilter{IncludeTerms: []string{"a OR b"}}
	_, oracleTotal, err := fx.assets.SearchByFilter(ctx, oracle, nil, "path", "asc", 100, 0)
	if err != nil {
		t.Fatalf("oracle 搜索失败：%v", err)
	}
	if oracleTotal != total {
		t.Fatalf("字面量语义两侧不一致：FTS=%d oracle=%d", total, oracleTotal)
	}
}

// TestSearchDegradesWhenIndexNotReady 验证降级：completed_at 为 NULL（回填未完成或正在重建）时
// 整体走原 LIKE 路径，结果与 oracle 完全一致——「宁可慢而全，不可快而漏」。
func TestSearchDegradesWhenIndexNotReady(t *testing.T) {
	fx := newRewriteFixture(t)
	ctx := context.Background()
	// 模拟「重建中」：把 completed_at 打回 NULL。
	if err := fx.state.Reset(); err != nil {
		t.Fatalf("重置索引状态：%v", err)
	}
	ready, err := fx.assets.SearchIndexReady()
	if err != nil {
		t.Fatalf("读取索引就绪状态：%v", err)
	}
	if ready {
		t.Fatal("重置后索引状态应为未就绪")
	}
	f := SearchFilter{IncludeTerms: []string{"alpha"}, UseFullTextIndex: ready}
	assets, total, err := fx.assets.SearchByFilter(ctx, f, nil, "path", "asc", 100, 0)
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	// 索引已被清空，但降级路径必须仍然给出完整结果。
	if len(assets) == 0 {
		t.Fatal("索引清空期间降级路径应仍能返回结果（不得静默漏结果）")
	}
	oracle := SearchFilter{IncludeTerms: []string{"alpha"}}
	_, oracleTotal, err := fx.assets.SearchByFilter(ctx, oracle, nil, "path", "asc", 100, 0)
	if err != nil {
		t.Fatalf("oracle 搜索失败：%v", err)
	}
	if total != oracleTotal {
		t.Fatalf("降级路径与 oracle 不一致：%d vs %d", total, oracleTotal)
	}
}

// TestSearchIndexReadyReflectsState 钉住就绪判定的两个方向。
func TestSearchIndexReadyReflectsState(t *testing.T) {
	fx := newRewriteFixture(t)
	ready, err := fx.assets.SearchIndexReady()
	if err != nil {
		t.Fatalf("读取就绪状态：%v", err)
	}
	if !ready {
		t.Fatal("标记完成后应为就绪")
	}
	if err := fx.state.Reset(); err != nil {
		t.Fatalf("重置：%v", err)
	}
	if ready, err = fx.assets.SearchIndexReady(); err != nil || ready {
		t.Fatalf("重置后就绪状态=(%v, %v)，期望 (false, nil)", ready, err)
	}
}

// TestSearchPerRepoByFilterMatchesPerRepoLoop 钉住 npm 收口依赖的「按仓库分别取前 N 条」：
// 一条窗口函数语句的结果必须与「逐仓库各搜一次」逐条一致（含每仓独立分页）。
func TestSearchPerRepoByFilterMatchesPerRepoLoop(t *testing.T) {
	fx := newRewriteFixture(t)
	ctx := context.Background()
	f := SearchFilter{IncludeTerms: []string{"a"}, UseFullTextIndex: true}
	repoIDs := []int64{fx.repo1, fx.repo2, fx.repo3}
	const perRepoLimit = 2

	merged, err := fx.assets.SearchPerRepoByFilter(ctx, f, repoIDs, perRepoLimit)
	if err != nil {
		t.Fatalf("按仓库分页查询失败：%v", err)
	}

	// 逐仓库各搜一次，拼成同样的 (repository_id, path) 序列。
	var want []string
	for _, id := range repoIDs {
		items, _, err := fx.assets.SearchByFilter(ctx, f, []int64{id}, "path", "asc", perRepoLimit, 0)
		if err != nil {
			t.Fatalf("逐仓库搜索失败：%v", err)
		}
		for _, a := range items {
			want = append(want, a.Path)
		}
	}
	got := make([]string, 0, len(merged))
	for _, a := range merged {
		got = append(got, a.Path)
	}
	if len(got) != len(want) {
		t.Fatalf("行数不一致：merged=%v 逐仓库=%v", got, want)
	}
	// 逐仓库顺序可能与其在 repoIDs 中的顺序不同（merged 按 repository_id 升序返回），
	// 因此按仓库分组后逐仓比较。
	for _, id := range repoIDs {
		var gotInRepo, wantInRepo []string
		for _, a := range merged {
			if a.RepositoryID == id {
				gotInRepo = append(gotInRepo, a.Path)
			}
		}
		items, _, err := fx.assets.SearchByFilter(ctx, f, []int64{id}, "path", "asc", perRepoLimit, 0)
		if err != nil {
			t.Fatalf("逐仓库搜索失败：%v", err)
		}
		for _, a := range items {
			wantInRepo = append(wantInRepo, a.Path)
		}
		if strings.Join(gotInRepo, "|") != strings.Join(wantInRepo, "|") {
			t.Fatalf("仓库 %d 的前 %d 条不一致：merged=%v 逐仓库=%v", id, perRepoLimit, gotInRepo, wantInRepo)
		}
	}
}

// TestSearchPerRepoByFilterLimitAndEmpty 覆盖 perRepoLimit 与空入参的边界。
func TestSearchPerRepoByFilterLimitAndEmpty(t *testing.T) {
	fx := newRewriteFixture(t)
	ctx := context.Background()
	f := SearchFilter{IncludeTerms: []string{"a"}, UseFullTextIndex: true}
	if items, err := fx.assets.SearchPerRepoByFilter(ctx, f, nil, 5); err != nil || items != nil {
		t.Fatalf("空仓库集合应返回空结果：items=%v err=%v", items, err)
	}
	if items, err := fx.assets.SearchPerRepoByFilter(ctx, f, []int64{fx.repo1}, 0); err != nil || items != nil {
		t.Fatalf("非正 limit 应返回空结果：items=%v err=%v", items, err)
	}
	items, err := fx.assets.SearchPerRepoByFilter(ctx, f, []int64{fx.repo1, fx.repo2}, 1)
	if err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	perRepo := map[int64]int{}
	for _, a := range items {
		perRepo[a.RepositoryID]++
	}
	for id, n := range perRepo {
		if n > 1 {
			t.Fatalf("仓库 %d 返回 %d 条，超过 perRepoLimit=1", id, n)
		}
	}
}

// TestSearchByFilterHonorsContext 验证仓储层真的把 ctx 传给了驱动：已取消的 ctx 必须报错，
// 而不是照常返回结果（否则域层的超时预算就是摆设）。
func TestSearchByFilterHonorsContext(t *testing.T) {
	fx := newRewriteFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := SearchFilter{IncludeTerms: []string{"alpha"}, UseFullTextIndex: true}
	if _, _, err := fx.assets.SearchByFilter(ctx, f, nil, "path", "asc", 10, 0); err == nil {
		t.Fatal("已取消的 ctx 下搜索应返回错误")
	}
	if _, err := fx.assets.SearchFacetsByFilter(ctx, f, nil); err == nil {
		t.Fatal("已取消的 ctx 下 facet 聚合应返回错误")
	}
}
