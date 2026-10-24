package repository_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// openCleanTaskTestDB 建库并跑完全部迁移（复用本包既有范式）。
func openCleanTaskTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "clean.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

// TestCleanTaskSingleConcurrency 钉住单并发守卫：已有 queued/running 时再建任务必须被拒。
//
// 这条守卫的真正难点是**查与插必须同事务**——若先 SELECT 再 INSERT，两个并发请求会同时
// 看到「没有 running」而各自插入成功，守卫形同虚设。用例用串行两次调用覆盖语义，
// 事务内串行化由 SQLite 单写者保证（并发路径见 TestCleanTaskConcurrentCreateOnlyOneWins）。
func TestCleanTaskSingleConcurrency(t *testing.T) {
	t.Parallel()
	db := openCleanTaskTestDB(t)
	repo := repository.NewCleanTaskRepo(db)

	first, err := repo.Create("empty_dirs", 10)
	if err != nil {
		t.Fatalf("首次建任务：%v", err)
	}
	if first.Status != repository.CleanTaskQueued {
		t.Fatalf("新任务状态 = %q，期望 queued", first.Status)
	}

	if _, err := repo.Create("stale_uploads", 1); !errors.Is(err, repository.ErrCleanTaskRunning) {
		t.Fatalf("已有 queued 时应拒绝新任务，实际：%v", err)
	}

	if err := repo.MarkRunning(first.ID); err != nil {
		t.Fatalf("置 running：%v", err)
	}
	if _, err := repo.Create("quarantine", 1); !errors.Is(err, repository.ErrCleanTaskRunning) {
		t.Fatalf("已有 running 时应拒绝新任务，实际：%v", err)
	}

	// 收尾后应能再建。
	if err := repo.MarkSucceeded(first.ID, 10, 0); err != nil {
		t.Fatalf("收尾：%v", err)
	}
	if _, err := repo.Create("quarantine", 1); err != nil {
		t.Fatalf("前一条已结束后应允许新任务，实际：%v", err)
	}
}

// TestCleanTaskConcurrentCreateOnlyOneWins 用并发创建验证守卫在真并发下仍成立。
func TestCleanTaskConcurrentCreateOnlyOneWins(t *testing.T) {
	t.Parallel()
	db := openCleanTaskTestDB(t)
	repo := repository.NewCleanTaskRepo(db)

	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := repo.Create("empty_dirs", 1)
			results <- err
		}()
	}
	var ok, rejected int
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			ok++
		case errors.Is(err, repository.ErrCleanTaskRunning):
			rejected++
		default:
			t.Fatalf("意外错误：%v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("并发创建成功 %d 条，期望恰好 1 条", ok)
	}
	if rejected != n-1 {
		t.Fatalf("被拒 %d 条，期望 %d 条", rejected, n-1)
	}
}

// TestCleanTaskMarkRunningOnlyFromQueued 钉住状态机：running 只能从 queued 跃迁。
//
// 为什么重要：同一条任务若能被重复 MarkRunning，就会有两个执行者同时删同一批文件。
func TestCleanTaskMarkRunningOnlyFromQueued(t *testing.T) {
	t.Parallel()
	db := openCleanTaskTestDB(t)
	repo := repository.NewCleanTaskRepo(db)

	task, err := repo.Create("empty_dirs", 3)
	if err != nil {
		t.Fatalf("建任务：%v", err)
	}
	if err := repo.MarkRunning(task.ID); err != nil {
		t.Fatalf("首次置 running：%v", err)
	}
	if err := repo.MarkRunning(task.ID); !errors.Is(err, repository.ErrCleanTaskRunning) {
		t.Fatalf("重复置 running 应被拒，实际：%v", err)
	}
}

// TestCleanTaskStartupMarksInterrupted 钉住启动收口：残留 queued/running 一律变 interrupted。
//
// 不做的后果很具体：那条 running 永远没人推进，单并发守卫会**永久拒绝**后续所有清理请求，
// 表现为"清理功能突然坏了"，而历史里还留着一条永不结束的任务。
func TestCleanTaskStartupMarksInterrupted(t *testing.T) {
	t.Parallel()
	db := openCleanTaskTestDB(t)
	repo := repository.NewCleanTaskRepo(db)

	running, err := repo.Create("empty_dirs", 5)
	if err != nil {
		t.Fatalf("建任务：%v", err)
	}
	if err := repo.MarkRunning(running.ID); err != nil {
		t.Fatalf("置 running：%v", err)
	}
	// 直接构造一条 queued：用另一库语义上等价——这里先把 running 收口，再建一条 queued。
	affected, err := repo.MarkInterruptedOnStartup()
	if err != nil {
		t.Fatalf("启动收口：%v", err)
	}
	if affected != 1 {
		t.Fatalf("收口影响 %d 行，期望 1 行", affected)
	}
	got, err := repo.Get(running.ID)
	if err != nil {
		t.Fatalf("读任务：%v", err)
	}
	if got.Status != repository.CleanTaskInterrupted {
		t.Fatalf("状态 = %q，期望 interrupted", got.Status)
	}
	if got.FinishedAt == nil {
		t.Fatal("收口后 finished_at 不应为空")
	}
	// 收口之后必须能再建新任务，否则单并发守卫会永久拒绝。
	if _, err := repo.Create("empty_dirs", 1); err != nil {
		t.Fatalf("收口后应允许新任务，实际：%v", err)
	}
}

// TestTrashEntryLifecycle 钉住回收站条目的逐条状态迁移：只允许 staged → 终态。
func TestTrashEntryLifecycle(t *testing.T) {
	t.Parallel()
	db := openCleanTaskTestDB(t)
	repo := repository.NewCleanTaskRepo(db)

	task, err := repo.Create("empty_dirs", 2)
	if err != nil {
		t.Fatalf("建任务：%v", err)
	}
	if err := repo.InsertTrashEntries([]repository.TrashEntry{
		{TaskID: task.ID, Kind: "empty_dirs", OriginalPath: "/data/blobs/0a/ea", TrashPath: "/data/trash/1/x", IsDir: true, Bytes: 0},
	}); err != nil {
		t.Fatalf("写入回收站条目：%v", err)
	}
	staged, total, err := repo.ListTrash("", 10, 0)
	if err != nil {
		t.Fatalf("列回收站：%v", err)
	}
	if total != 1 || len(staged) != 1 {
		t.Fatalf("staged 条目数 = %d/%d，期望 1/1", total, len(staged))
	}

	if err := repo.MarkTrashResolved(staged[0].ID, repository.TrashRestored); err != nil {
		t.Fatalf("恢复：%v", err)
	}
	// 已恢复的不该能再被彻底删除。
	if err := repo.MarkTrashResolved(staged[0].ID, repository.TrashPurged); err != nil {
		t.Fatalf("对已恢复条目再迁移不应报错（只影响 0 行）：%v", err)
	}
	entry, err := repo.GetTrash(staged[0].ID)
	if err != nil {
		t.Fatalf("读条目：%v", err)
	}
	if entry.Status != repository.TrashRestored {
		t.Fatalf("状态 = %q，期望 restored（staged → 终态只发生一次）", entry.Status)
	}

	if err := repo.MarkTrashResolved(staged[0].ID, "bogus"); err == nil {
		t.Fatal("非法目标状态应报错")
	}
}

// TestTrashRetentionCutoff 钉住保留期筛选：只挑出 created_at 早于 cutoff 的 staged 条目。
//
// cutoff 的语义是「保留期到期时刻」，命中的条件是 `created_at < cutoff`。因此用例构造
// **两条不同时间的条目**（30 天前 / 刚刚）再看同一 cutoff 命中谁；拿"未来 cutoff"去断言
// "不该命中"是把语义搞反了——未来 cutoff 本来就该命中所有既有条目。
func TestTrashRetentionCutoff(t *testing.T) {
	t.Parallel()
	db := openCleanTaskTestDB(t)
	repo := repository.NewCleanTaskRepo(db)

	task, err := repo.Create("stale_uploads", 2)
	if err != nil {
		t.Fatalf("建任务：%v", err)
	}
	if err := repo.InsertTrashEntries([]repository.TrashEntry{
		{TaskID: task.ID, Kind: "stale_uploads", OriginalPath: "/data/backup-uploads/old", TrashPath: "/data/trash/1/a", IsDir: true, Bytes: 100},
		{TaskID: task.ID, Kind: "stale_uploads", OriginalPath: "/data/backup-uploads/new", TrashPath: "/data/trash/1/b", IsDir: true, Bytes: 200},
	}); err != nil {
		t.Fatalf("写入：%v", err)
	}
	// 把第一条改成 30 天前，第二条保持"刚刚"。
	if _, err := db.Exec(
		`UPDATE trash_entry SET created_at = datetime('now', '-30 days') WHERE original_path LIKE '%/old'`); err != nil {
		t.Fatalf("回填旧条目时间：%v", err)
	}

	// 保留期 7 天：只有远古那条到期。
	expired, err := repo.ExpiredStagedTrash(time.Now().Add(-7*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("按保留期筛选：%v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("7 天保留期应只命中 1 条，实际 %d 条", len(expired))
	}
	if expired[0].OriginalPath != "/data/backup-uploads/old" {
		t.Fatalf("命中的应是旧条目，实际 %q", expired[0].OriginalPath)
	}

	// 极短保留期：两条都到期。
	expiredAll, err := repo.ExpiredStagedTrash(time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("按保留期筛选：%v", err)
	}
	if len(expiredAll) != 2 {
		t.Fatalf("极短保留期应命中全部 2 条，实际 %d 条", len(expiredAll))
	}
}
