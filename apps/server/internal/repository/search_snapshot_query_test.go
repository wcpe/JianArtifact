package repository

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// 本文件钉住「一次搜索只开一个只读快照」这条结构性约束。
//
// 为什么需要专门验证：SearchAggregateByFilter 与「依次调用三个搜索方法」的**结果通常相同**
// ——没有并发写入时，三份快照读到的就是同一份数据，所以结果对得上不能证明改造达成目的。
// 快照数才是唯一可区分的量，因此这里直接观测两件事：
//
//  1. searchSnapshot 的调用次数（searchSnapshotProbe）——合并入口必须只开 1 次；
//  2. 四条语句是否都跑在那份快照的**同一个连接**上——用「连接池上限为 1」的库验证：
//     任何一条语句若绕过 tx 走连接池，池里唯一的连接正被这份事务占着，语句只能等到
//     ctx 预算耗尽后失败（这正是 db.go 强调过的那条编码约束）。

// newSingleConnSnapshotFixture 建一个「连接池上限为 1」的库并写入聚合语料。
//
// 池上限 1 是刻意的：它是把「事务内语句偷偷走连接池」从「静默读到旧快照」变回
// 「当场失败」的唯一构造方式（见 persistence.OpenWithMaxConns 的注释）。
func newSingleConnSnapshotFixture(t *testing.T, paths ...string) (*AssetRepo, int64) {
	t.Helper()
	db, err := persistence.OpenWithMaxConns(filepath.Join(t.TempDir(), "snapshot.db"), 1)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repoID, err := NewRepoRepo(db).Create("snap-maven", "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	assets := NewAssetRepo(db)
	for _, path := range paths {
		if err := assets.Upsert(repoID, path, "h", 1, "application/java-archive", "s", "m"); err != nil {
			t.Fatalf("写入资产 %s：%v", path, err)
		}
	}
	return assets, repoID
}

// countSnapshots 统计 fn 执行期间开启的搜索快照数。
func countSnapshots(t *testing.T, fn func()) int {
	t.Helper()
	prev := searchSnapshotProbe
	count := 0
	searchSnapshotProbe = func() { count++ }
	t.Cleanup(func() { searchSnapshotProbe = prev })
	fn()
	return count
}

// TestSearchAggregateByFilterUsesSingleSnapshot 是本次改造的核心断言：
// 计数、分页、facet 聚合与聚合扫描四条语句必须由**一次**快照覆盖，且结果与逐次调用
// 各搜索方法逐条一致（改造不得改变任何结果语义）。
func TestSearchAggregateByFilterUsesSingleSnapshot(t *testing.T) {
	assets, repoID := newSingleConnSnapshotFixture(t,
		"com/example/foo/1.0.0/foo-1.0.0.jar",
		"com/example/foo/1.0.0/foo-1.0.0.pom",
		"com/example/foo/2.0.0/foo-2.0.0.jar",
		"com/example/bar/1.0.0/bar-1.0.0.jar",
	)
	ctx := context.Background()
	filter := SearchFilter{IncludeTerms: []string{"com/example"}}
	repoIDs := []int64{repoID}

	var res *SearchAggregateResult
	// 单连接池：四条语句若有一条跑在 tx 之外，这里会等到 ctx 预算耗尽后报错，
	// 用例直接失败（不是概率性失败）。
	snapshots := countSnapshots(t, func() {
		var err error
		res, err = assets.SearchAggregateByFilter(ctx, SearchAggregateQuery{
			Filter:   filter,
			RepoIDs:  repoIDs,
			FacetIDs: repoIDs,
			Sort:     "path",
			Order:    "asc",
			Limit:    3,
			Offset:   0,
			MaxRows:  100,
		})
		if err != nil {
			t.Fatalf("合并搜索失败：%v", err)
		}
	})
	if snapshots != 1 {
		t.Fatalf("开启的搜索快照数 = %d，期望 1（四条查询必须共用同一份只读快照）", snapshots)
	}

	// 对照：逐次调用三个方法会开 3 份快照——这正是合并入口要消除的行为，
	// 同时用来证明上面的计数确实能区分「1 份」与「多份」。
	separate := countSnapshots(t, func() {
		if _, _, err := assets.SearchByFilter(ctx, filter, repoIDs, "path", "asc", 3, 0); err != nil {
			t.Fatalf("分页搜索失败：%v", err)
		}
		if _, err := assets.SearchFacetsByFilter(ctx, filter, repoIDs); err != nil {
			t.Fatalf("facet 统计失败：%v", err)
		}
		if _, _, err := assets.SearchAllByFilter(ctx, filter, repoIDs, 100); err != nil {
			t.Fatalf("聚合扫描失败：%v", err)
		}
	})
	if separate != 3 {
		t.Fatalf("逐次调用开启的快照数 = %d，期望 3（该对照用于证明计数有效）", separate)
	}

	// 语义等价：合并入口的每一项都必须与逐次调用的结果逐条一致。
	wantItems, wantTotal, err := assets.SearchByFilter(ctx, filter, repoIDs, "path", "asc", 3, 0)
	if err != nil {
		t.Fatalf("对照分页搜索失败：%v", err)
	}
	wantFacets, err := assets.SearchFacetsByFilter(ctx, filter, repoIDs)
	if err != nil {
		t.Fatalf("对照 facet 统计失败：%v", err)
	}
	wantRows, wantTruncated, err := assets.SearchAllByFilter(ctx, filter, repoIDs, 100)
	if err != nil {
		t.Fatalf("对照聚合扫描失败：%v", err)
	}
	if res.Total != wantTotal {
		t.Fatalf("total = %d，期望 %d", res.Total, wantTotal)
	}
	if !equalStrings(pathsOf(res.Items), pathsOf(wantItems)) {
		t.Fatalf("分页结果 = %v，期望 %v", pathsOf(res.Items), pathsOf(wantItems))
	}
	if len(res.Facets) != len(wantFacets) || res.Facets[0] != wantFacets[0] {
		t.Fatalf("facet 结果 = %+v，期望 %+v", res.Facets, wantFacets)
	}
	if !equalStrings(pathsOf(res.Rows), pathsOf(wantRows)) || res.Truncated != wantTruncated {
		t.Fatalf("聚合扫描结果 = %v（truncated=%v），期望 %v（truncated=%v）",
			pathsOf(res.Rows), res.Truncated, pathsOf(wantRows), wantTruncated)
	}
	// 内部一致性：未截断时聚合扫描行数必须等于精确计数（同一份快照的直接推论）。
	if !res.Truncated && len(res.Rows) != res.Total {
		t.Fatalf("未截断时扫描行数 = %d，期望等于 total=%d", len(res.Rows), res.Total)
	}
}

// TestSearchAggregateByFilterSkipAssetRows 钉住「无主查询条件」分支：
// 只做 facet 统计，仍只开一份快照，且不发计数/分页/扫描语句。
func TestSearchAggregateByFilterSkipAssetRows(t *testing.T) {
	assets, repoID := newSingleConnSnapshotFixture(t, "com/example/foo/1.0.0/foo-1.0.0.jar")
	ctx := context.Background()

	var res *SearchAggregateResult
	snapshots := countSnapshots(t, func() {
		var err error
		res, err = assets.SearchAggregateByFilter(ctx, SearchAggregateQuery{
			Filter:        SearchFilter{IncludeTerms: []string{"com/example"}},
			RepoIDs:       []int64{repoID},
			FacetIDs:      []int64{repoID},
			SkipAssetRows: true,
		})
		if err != nil {
			t.Fatalf("跳过主查询的合并搜索失败：%v", err)
		}
	})
	if snapshots != 1 {
		t.Fatalf("开启的搜索快照数 = %d，期望 1", snapshots)
	}
	if res.Total != 0 || len(res.Items) != 0 || len(res.Rows) != 0 || res.Truncated {
		t.Fatalf("跳过主查询时不应产出计数/分页/扫描：total=%d items=%d rows=%d truncated=%v",
			res.Total, len(res.Items), len(res.Rows), res.Truncated)
	}
	// facet 仍然要统计（与逐次调用时的语义一致）。
	if len(res.Facets) != 1 || res.Facets[0].Count != 1 {
		t.Fatalf("facet 统计 = %+v，期望单仓库 1 条", res.Facets)
	}
}

// TestSearchAggregateByFilterTruncatesRows 钉住合并入口的扫描预算口径：
// 超过 MaxRows 时 truncated=true 且行数恰为 MaxRows，total 仍是精确值（下界语义）。
func TestSearchAggregateByFilterTruncatesRows(t *testing.T) {
	assets, repoID := newSingleConnSnapshotFixture(t,
		"com/example/foo/1.0.0/foo-1.0.0.jar",
		"com/example/foo/1.0.0/foo-1.0.0.pom",
		"com/example/foo/2.0.0/foo-2.0.0.jar",
	)
	ctx := context.Background()
	res, err := assets.SearchAggregateByFilter(ctx, SearchAggregateQuery{
		Filter:   SearchFilter{IncludeTerms: []string{"com/example"}},
		RepoIDs:  []int64{repoID},
		FacetIDs: []int64{repoID},
		Sort:     "path",
		Order:    "asc",
		Limit:    10,
		MaxRows:  2,
	})
	if err != nil {
		t.Fatalf("合并搜索失败：%v", err)
	}
	if !res.Truncated {
		t.Fatal("超过扫描预算时必须标记 truncated")
	}
	if len(res.Rows) != 2 {
		t.Fatalf("截断后扫描行数 = %d，期望 2", len(res.Rows))
	}
	if res.Total != 3 {
		t.Fatalf("total = %d，期望 3（不受扫描预算影响）", res.Total)
	}
	if len(res.Items) != 3 {
		t.Fatalf("分页行数 = %d，期望 3（分页不受扫描预算影响）", len(res.Items))
	}
}
