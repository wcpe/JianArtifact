package domain_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住的后台回填行为：全量回填、中断续跑、幂等重写、完成即跳过、
// 完成/重建互斥（409 的领域侧判定）。
//
// 断言口径与生产查询同形：命中判断一律「MATCH 只取 rowid」，不读 FTS 表列值——external
// content 表在有孤儿索引行时读列值会报 267（见 repository/search_index_repo.go 文件头第 4 条）。

// indexServiceFixture 打开临时库、建仓库并绕开索引直接写 N 条历史资产
// （模拟 0049 之前写入、尚未回填的行），返回服务、库与仓库 id。
func indexServiceFixture(t *testing.T, rows int) (*domain.SearchIndexService, *persistence.DB, int64) {
	t.Helper()
	db := newTestDB(t)
	repoID, err := repository.NewRepoRepo(db).Create("backfill-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for i := 0; i < rows; i++ {
		// 路径里带序号，便于逐条断言「在不在索引里」。
		if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
			VALUES (?, ?, 'h', 1, 'text/plain', '', '')`,
			repoID, fmt.Sprintf("legacy/pkg-%04d-1.0.0.jar", i)); err != nil {
			t.Fatalf("写入历史行 %d：%v", i, err)
		}
	}
	svc := domain.NewSearchIndexService(repository.NewSearchIndexStateRepo(db))
	return svc, db, repoID
}

// indexHitCount 用生产形态（MATCH + 回表）统计某个子串命中的 asset 行数。
func indexHitCount(t *testing.T, db *persistence.DB, term string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM asset WHERE id IN
		(SELECT rowid FROM asset_path_fts WHERE asset_path_fts MATCH ?)`, `"`+term+`"`); err != nil {
		t.Fatalf("按 %q 统计命中：%v", term, err)
	}
	return n
}

// indexState 读回填状态（completed_at 用一个可判空的字符串指针表达）。
func indexState(t *testing.T, db *persistence.DB) repository.SearchIndexState {
	t.Helper()
	state, err := repository.NewSearchIndexStateRepo(db).Load()
	if err != nil {
		t.Fatalf("读回填状态：%v", err)
	}
	return state
}

// TestSearchIndexServiceBackfillsHistory 验证最基本的一条：历史未索引行被全部灌入索引，
// 结束后 completed_at 已置、last_id 等于最大 id、indexed 与 total 相等。
func TestSearchIndexServiceBackfillsHistory(t *testing.T) {
	const rows = 120
	svc, db, _ := indexServiceFixture(t, rows)

	// 回填前：索引里一条也没有（迁移不回填）。
	if got := indexHitCount(t, db, "pkg-"); got != 0 {
		t.Fatalf("回填前索引命中 = %d，期望 0", got)
	}

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("回填：%v", err)
	}

	if got := indexHitCount(t, db, "pkg-"); got != rows {
		t.Fatalf("回填后命中 = %d，期望 %d", got, rows)
	}
	// 逐条抽样：首、中、尾都在索引里（防止「只灌了前面一段」）。
	for _, i := range []int{0, rows / 2, rows - 1} {
		if got := indexHitCount(t, db, fmt.Sprintf("pkg-%04d", i)); got != 1 {
			t.Fatalf("pkg-%04d 命中 = %d，期望 1", i, got)
		}
	}
	state := indexState(t, db)
	if !state.Completed() {
		t.Fatal("回填后 completed_at 应已置")
	}
	var maxID int64
	if err := db.Get(&maxID, `SELECT MAX(id) FROM asset`); err != nil {
		t.Fatalf("取最大 id：%v", err)
	}
	if state.LastID != maxID {
		t.Fatalf("last_id = %d，期望等于最大 id %d", state.LastID, maxID)
	}
	if state.TotalRows != rows {
		t.Fatalf("total_rows = %d，期望 %d", state.TotalRows, rows)
	}

	status, err := svc.Status()
	if err != nil {
		t.Fatalf("读状态快照：%v", err)
	}
	if !status.Ready || status.Indexed != rows || status.Total != rows || status.LastID != maxID {
		t.Fatalf("状态快照不符：%+v", status)
	}
}

// TestSearchIndexServiceStatusReportsDrift 校验状态快照里的漂移口径：
//
// 索引正确性只靠「所有 asset 写入必经仓储层」这一约定，而回填只跑一次；一旦有旁路写入
// （运维脚本 / 手工 SQL / 未来的迁移），漂移就是**永久静默**的。因此状态必须能报出来，
// 并且让 ready 说实话——否则用户唯一能感知到的现象就是「某些制品搜不到」。
//
// 两个方向分别断言：漏索引（危险，会让 ready 转 false）与孤儿索引行（只浪费空间，ready
// 保持 true）。孤儿行不能把 ready 打下来：它由仓库级联删除天然产生，否则删一次仓库就让
// ready 永久为 false。
func TestSearchIndexServiceStatusReportsDrift(t *testing.T) {
	const rows = 20
	svc, db, repoID := indexServiceFixture(t, rows)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("回填：%v", err)
	}

	status, err := svc.Status()
	if err != nil {
		t.Fatalf("读状态快照：%v", err)
	}
	if status.IndexRows != rows || status.AssetRows != rows || status.MissingRows != 0 || status.OrphanRows != 0 {
		t.Fatalf("正常回填后不应有漂移：%+v", status)
	}
	if !status.Ready || status.MissingIndexRows() {
		t.Fatalf("正常回填后 ready 应为 true：%+v", status)
	}

	// 旁路写入：直接插 asset 行、不写索引。回填已完成，因此这条漂移不会自愈。
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
		VALUES (?, 'side/door-1.0.0.jar', 'h', 1, 'text/plain', '', '')`, repoID); err != nil {
		t.Fatalf("旁路写入资产行：%v", err)
	}
	status, err = svc.Status()
	if err != nil {
		t.Fatalf("读状态快照：%v", err)
	}
	if status.IndexRows != rows || status.AssetRows != rows+1 || status.MissingRows != 1 || status.OrphanRows != 0 {
		t.Fatalf("旁路写入后应报 1 行漏索引：%+v", status)
	}
	if !status.MissingIndexRows() || status.Ready {
		t.Fatalf("存在漏索引时 ready 必须为 false，否则状态端点仍报「一切正常」：%+v", status)
	}

	// 反方向（孤儿索引行）：资产行已消失、索引条目残留，是仓库级联删除的形状。
	// 它只让索引偏大、不丢结果，因此不把 ready 打下来——否则删一次仓库就会让 ready 永久为
	// false，真出问题时这个信号早已被忽略。
	if _, err := db.Exec(`DELETE FROM asset WHERE path = 'side/door-1.0.0.jar'`); err != nil {
		t.Fatalf("清掉旁路行：%v", err)
	}
	if _, err := db.Exec(`DELETE FROM asset WHERE path = 'legacy/pkg-0000-1.0.0.jar'`); err != nil {
		t.Fatalf("删一条已索引的资产行制造孤儿：%v", err)
	}
	status, err = svc.Status()
	if err != nil {
		t.Fatalf("读状态快照：%v", err)
	}
	if status.IndexRows != rows || status.AssetRows != rows-1 || status.OrphanRows != 1 || status.MissingRows != 0 {
		t.Fatalf("孤儿索引行应报 1 行：%+v", status)
	}
	if status.MissingIndexRows() || !status.Ready {
		t.Fatalf("孤儿索引行不应把 ready 打下来（不丢结果，重建即可清理）：%+v", status)
	}

	// 正负抵消：同时漏一行与孤儿一行。取总数差额会得到 0，旧口径因此看不见漏索引；
	// 分开计数后 MissingRows 仍必须为 1，ready 仍必须为 false。
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
		VALUES (?, 'side/late-2.0.0.jar', 'h', 1, 'text/plain', '', '')`, repoID); err != nil {
		t.Fatalf("再插一条未索引的资产行：%v", err)
	}
	status, err = svc.Status()
	if err != nil {
		t.Fatalf("读状态快照：%v", err)
	}
	if status.IndexRows != status.AssetRows {
		t.Fatalf("本例前提是两侧行数相等（差额为 0），实际 %+v", status)
	}
	if status.MissingRows != 1 || status.OrphanRows != 1 {
		t.Fatalf("漏索引与孤儿行同时存在时应分别计数（1/1），实际 %+v", status)
	}
	if !status.MissingIndexRows() || status.Ready {
		t.Fatalf("正负抵消时 ready 仍必须为 false（否则漏索引被孤儿行掩盖）：%+v", status)
	}
}

// TestSearchIndexServiceEmptyInstanceBecomesReady 空实例也应立刻标记完成：
// 否则「全新部署」会永远显示 ready=false，把 is-ready 变成一个无意义信号。
func TestSearchIndexServiceEmptyInstanceBecomesReady(t *testing.T) {
	svc, db, _ := indexServiceFixture(t, 0)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("空库回填：%v", err)
	}
	state := indexState(t, db)
	if !state.Completed() {
		t.Fatal("空库回填后应标记完成")
	}
	if state.LastID != 0 || state.TotalRows != 0 {
		t.Fatalf("空库状态不符：%+v", state)
	}
}

// TestSearchIndexServiceSkipsWhenCompleted 验证「已完成即退出（零开销）」：
// 完成之后新出现的未索引行不会被这个一次性任务捡起来（那些行走在线同步），
// 若这里偷偷重跑一次全表，等于每次启动都做一次无意义全扫。
func TestSearchIndexServiceSkipsWhenCompleted(t *testing.T) {
	const rows = 10
	svc, db, repoID := indexServiceFixture(t, rows)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("首次回填：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
		VALUES (?, 'late/new-9.9.9.jar', 'h', 1, 'text/plain', '', '')`, repoID); err != nil {
		t.Fatalf("补写一条未索引行：%v", err)
	}

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("再次运行：%v", err)
	}
	if got := indexHitCount(t, db, "new-9.9.9"); got != 0 {
		t.Fatalf("已完成状态下不应再回填，实际命中 %d", got)
	}
}

// scriptedStore 包装真实仓储，用于**确定性地**在中途注入取消：
//   - 把单批行数压小，让「多批」成为可控事实；
//   - 在第 N 批提交后取消上下文，使回填停在精确的水位上（不靠 sleep 抢时序）。
type scriptedStore struct {
	*repository.SearchIndexStateRepo
	batchSize  int
	cancelAt   int // 第 cancelAt 批提交后取消；0 表示不取消
	batches    int
	cancel     context.CancelFunc
	cancelledT bool
}

func (s *scriptedStore) FetchBatch(after, upper int64, limit int) ([]repository.SearchIndexRow, error) {
	if s.batchSize > 0 && limit > s.batchSize {
		limit = s.batchSize
	}
	return s.SearchIndexStateRepo.FetchBatch(after, upper, limit)
}

func (s *scriptedStore) WriteBatch(rows []repository.SearchIndexRow, lastID int64) error {
	if err := s.SearchIndexStateRepo.WriteBatch(rows, lastID); err != nil {
		return err
	}
	s.batches++
	if s.cancelAt > 0 && s.batches >= s.cancelAt && s.cancel != nil {
		s.cancel()
		s.cancelledT = true
	}
	return nil
}

// cancelAfterStore 构造一个「第 n 批之后取消」的服务与库。
func cancelAfterStore(t *testing.T, rows, batchSize, n int) (*domain.SearchIndexService, *persistence.DB, *scriptedStore, context.Context) {
	t.Helper()
	db := newTestDB(t)
	repoID, err := repository.NewRepoRepo(db).Create("resume-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
			VALUES (?, ?, 'h', 1, 'text/plain', '', '')`,
			repoID, fmt.Sprintf("resume/pkg-%04d-1.0.0.jar", i)); err != nil {
			t.Fatalf("写入历史行 %d：%v", i, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := &scriptedStore{
		SearchIndexStateRepo: repository.NewSearchIndexStateRepo(db),
		batchSize:            batchSize,
		cancelAt:             n,
		cancel:               cancel,
	}
	return domain.NewSearchIndexService(store), db, store, ctx
}

// TestSearchIndexServiceResumesFromWatermark 验证中断续跑：中途取消 → 水位停在已提交的批次、
// completed_at 仍为空 → 重启后从该水位接着跑完 → 不重复、不遗漏。
func TestSearchIndexServiceResumesFromWatermark(t *testing.T) {
	const rows = 50
	const batchSize = 10
	const stopAfter = 2 // 第 2 批提交后取消
	svc, db, store, ctx := cancelAfterStore(t, rows, batchSize, stopAfter)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("第一次回填（将在中途被取消）：%v", err)
	}
	if !store.cancelledT {
		t.Fatal("测试装置未按预期取消上下文（batchSize/cancelAt 配置有误）")
	}

	interrupted := indexState(t, db)
	if interrupted.Completed() {
		t.Fatal("中途取消不应标记完成")
	}
	if want := int64(batchSize * stopAfter); interrupted.LastID != want {
		t.Fatalf("中断后水位 = %d，期望 %d（已提交的批次）", interrupted.LastID, want)
	}
	if got := indexHitCount(t, db, "pkg-"); got != batchSize*stopAfter {
		t.Fatalf("中断后已入索引 = %d，期望 %d", got, batchSize*stopAfter)
	}

	// 重启：同一个库，新的上下文，不带取消脚本。
	restarted := domain.NewSearchIndexService(repository.NewSearchIndexStateRepo(db))
	if err := restarted.Run(context.Background()); err != nil {
		t.Fatalf("续跑：%v", err)
	}

	if got := indexHitCount(t, db, "pkg-"); got != rows {
		t.Fatalf("续跑完成后命中 = %d，期望 %d（不遗漏）", got, rows)
	}
	// 不重复：每一条只能命中一行（同一 rowid 重复写入不产生第二条命中）。
	for _, i := range []int{0, batchSize * stopAfter, rows - 1} {
		if got := indexHitCount(t, db, fmt.Sprintf("pkg-%04d", i)); got != 1 {
			t.Fatalf("pkg-%04d 命中 = %d，期望 1（不重复）", i, got)
		}
	}
	var docs int64
	if err := db.Get(&docs, `SELECT COUNT(*) FROM asset_path_fts_docsize`); err != nil {
		t.Fatalf("统计索引条目数：%v", err)
	}
	if docs != rows {
		t.Fatalf("索引条目数 = %d，期望 %d（不重复）", docs, rows)
	}
	final := indexState(t, db)
	if !final.Completed() {
		t.Fatal("续跑完成后应标记完成")
	}
	var maxID int64
	if err := db.Get(&maxID, `SELECT MAX(id) FROM asset`); err != nil {
		t.Fatalf("取最大 id：%v", err)
	}
	if final.LastID != maxID {
		t.Fatalf("最终水位 = %d，期望 %d", final.LastID, maxID)
	}
}

// TestSearchIndexServiceRewriteIsIdempotent 验证幂等：把已完成状态人为改回「未完成且水位归零」
// 再跑一次，等于对同一批行重复回填；命中数不得增加（这是回填与在线写入交叠时的必要语义）。
func TestSearchIndexServiceRewriteIsIdempotent(t *testing.T) {
	const rows = 30
	svc, db, _ := indexServiceFixture(t, rows)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("首次回填：%v", err)
	}
	// 只重置水位与完成标记，**不动索引**——模拟「索引已有条目、水位被回退」的重复回填。
	if _, err := db.Exec(`UPDATE search_index_state SET last_id = 0, completed_at = NULL WHERE id = 1`); err != nil {
		t.Fatalf("回退水位：%v", err)
	}
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("重复回填：%v", err)
	}
	if got := indexHitCount(t, db, "pkg-"); got != rows {
		t.Fatalf("重复回填后命中 = %d，期望 %d", got, rows)
	}
	var docs int64
	if err := db.Get(&docs, `SELECT COUNT(*) FROM asset_path_fts_docsize`); err != nil {
		t.Fatalf("统计索引条目数：%v", err)
	}
	if docs != rows {
		t.Fatalf("索引条目数 = %d，期望 %d（同一 rowid 重复插入不得产生第二条）", docs, rows)
	}
}

// TestSearchIndexServiceRebuildAfterDrift 验证重建：索引被清空（漂移）后，Rebuild 会清空索引、
// 重置水位并在后台重灌，最终恢复完整覆盖。
func TestSearchIndexServiceRebuildAfterDrift(t *testing.T) {
	const rows = 40
	svc, db, _ := indexServiceFixture(t, rows)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("首次回填：%v", err)
	}
	// 人为制造漂移：直接清空索引但保留完成标记（相当于「索引被外部弄丢了」）。
	store := repository.NewSearchIndexStateRepo(db)
	if err := store.Reset(); err != nil {
		t.Fatalf("制造漂移：%v", err)
	}
	if err := store.MarkCompleted(); err != nil {
		t.Fatalf("标记完成：%v", err)
	}
	if got := indexHitCount(t, db, "pkg-"); got != 0 {
		t.Fatalf("漂移后索引命中 = %d，期望 0", got)
	}

	if err := svc.Rebuild(); err != nil {
		t.Fatalf("受理重建：%v", err)
	}
	// 重建在后台进行：轮询状态直到完成（这是运维看到的同一条信号）。
	waitForReady(t, svc, rows)
	if got := indexHitCount(t, db, "pkg-"); got != rows {
		t.Fatalf("重建后命中 = %d，期望 %d", got, rows)
	}
	state := indexState(t, db)
	if !state.Completed() {
		t.Fatal("重建后应标记完成")
	}
}

// flakyStore 让前 failures 次索引写入返回错误，用于验证「单批失败不推进水位 + 指数退避重试」。
type flakyStore struct {
	*repository.SearchIndexStateRepo
	failures int
	failed   int
}

func (s *flakyStore) WriteBatch(rows []repository.SearchIndexRow, lastID int64) error {
	if s.failed < s.failures {
		s.failed++
		return errors.New("模拟索引写入失败")
	}
	return s.SearchIndexStateRepo.WriteBatch(rows, lastID)
}

// flakyFixture 用给定的失败次数构造服务与库（20 条历史行，单批即可覆盖）。
func flakyFixture(t *testing.T, failures int) (*domain.SearchIndexService, *persistence.DB, *flakyStore) {
	t.Helper()
	db := newTestDB(t)
	repoID, err := repository.NewRepoRepo(db).Create("retry-hosted", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := db.Exec(`INSERT INTO asset (repository_id, path, blob_hash, size, content_type, sha1, md5)
			VALUES (?, ?, 'h', 1, 'text/plain', '', '')`,
			repoID, fmt.Sprintf("retry/pkg-%04d-1.0.0.jar", i)); err != nil {
			t.Fatalf("写入历史行 %d：%v", i, err)
		}
	}
	store := &flakyStore{SearchIndexStateRepo: repository.NewSearchIndexStateRepo(db), failures: failures}
	return domain.NewSearchIndexService(store), db, store
}

// TestSearchIndexServiceRetriesTransientFailure 验证瞬时失败后重试成功：全部行都进索引，
// 且水位推进到最大 id——重试不能以「跳过这一批」为代价。
func TestSearchIndexServiceRetriesTransientFailure(t *testing.T) {
	svc, db, store := flakyFixture(t, 2) // 前两次写入失败，第三次成功
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("带瞬时失败的回填应最终成功：%v", err)
	}
	if store.failed != 2 {
		t.Fatalf("实际失败次数 = %d，期望 2（重试次数与预期不符）", store.failed)
	}
	if got := indexHitCount(t, db, "retry/"); got != 20 {
		t.Fatalf("重试后命中 = %d，期望 20（不得漏行）", got)
	}
	state := indexState(t, db)
	if !state.Completed() {
		t.Fatal("重试成功后应标记完成")
	}
	var maxID int64
	if err := db.Get(&maxID, `SELECT MAX(id) FROM asset`); err != nil {
		t.Fatalf("取最大 id：%v", err)
	}
	if state.LastID != maxID {
		t.Fatalf("水位 = %d，期望 %d", state.LastID, maxID)
	}
}

// TestSearchIndexServiceGivesUpWithoutAdvancingWatermark 验证持续失败时：不推进水位、
// 不置完成（ready 保持 false，搜索继续走降级路径），本次运行返回错误——
// 下次启动会从同一水位重试，因此绝不漏行。
func TestSearchIndexServiceGivesUpWithoutAdvancingWatermark(t *testing.T) {
	svc, db, _ := flakyFixture(t, 100) // 一直失败
	err := svc.Run(context.Background())
	if err == nil {
		t.Fatal("持续失败时 Run 应返回错误")
	}
	state := indexState(t, db)
	if state.Completed() {
		t.Fatal("持续失败不应标记完成")
	}
	if state.LastID != 0 {
		t.Fatalf("持续失败不应推进水位，实际 last_id = %d", state.LastID)
	}
	if got := indexHitCount(t, db, "retry/"); got != 0 {
		t.Fatalf("持续失败时不应有索引行，实际命中 %d", got)
	}
}

// waitForReady 轮询状态直到「已完成且已回填到期望行数」（重建是异步受理的 202 语义，
// 测试用轮询对齐运维观测口径）。带 wantIndexed 的理由：重建刚受理时状态可能仍是
// 「重建前」的快照，只看 ready 会提前返回。
func waitForReady(t *testing.T, svc *domain.SearchIndexService, wantIndexed int64) {
	t.Helper()
	for i := 0; i < 600; i++ {
		status, err := svc.Status()
		if err != nil {
			t.Fatalf("读状态：%v", err)
		}
		if status.Ready && status.Indexed == wantIndexed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待回填完成超时（3 秒），期望 ready=true 且 indexed=%d", wantIndexed)
}

// blockingStore 让回填停在「已受理但仍在跑」的状态，用于确定性地覆盖并发互斥（409 的领域侧）。
type blockingStore struct {
	*repository.SearchIndexStateRepo
	started chan struct{}
	release chan struct{}
}

func (s *blockingStore) Reset() error {
	close(s.started) // 通知测试「后台任务已经开始」
	<-s.release      // 一直阻塞到测试放行
	return s.SearchIndexStateRepo.Reset()
}

// TestSearchIndexServiceRejectsConcurrentRebuild 验证回填与重建不可并跑：
// 正在运行时再次受理重建必须返回 ErrSearchIndexRunning（HTTP 侧映射 409）。
func TestSearchIndexServiceRejectsConcurrentRebuild(t *testing.T) {
	db := newTestDB(t)
	store := &blockingStore{
		SearchIndexStateRepo: repository.NewSearchIndexStateRepo(db),
		started:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	svc := domain.NewSearchIndexService(store)

	if err := svc.Rebuild(); err != nil {
		t.Fatalf("第一次受理重建：%v", err)
	}
	<-store.started // 等后台任务进入 Reset（此时 running 已置位）
	if err := svc.Rebuild(); err != domain.ErrSearchIndexRunning {
		t.Fatalf("运行中重复受理应返回 ErrSearchIndexRunning，实际 %v", err)
	}
	if err := svc.Run(context.Background()); err != domain.ErrSearchIndexRunning {
		t.Fatalf("运行中直接 Run 也应被拒绝，实际 %v", err)
	}
	close(store.release)
	waitForReady(t, svc, 0)
}
