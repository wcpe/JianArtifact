package repository_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTestSyncLog 打开临时 SQLite、迁移并返回 SyncLogRepo。
func newTestSyncLog(t *testing.T) *repository.SyncLogRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "sync-log.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewSyncLogRepo(db)
}

// TestSyncLogStartFinish 开始写进行中记录（success NULL），结束更新成功状态与统计（含实体构成）。
func TestSyncLogStartFinish(t *testing.T) {
	logs := newTestSyncLog(t)

	id, err := logs.Start("https://repo.example.com", 5)
	if err != nil {
		t.Fatalf("Start：%v", err)
	}

	// 进行中：success 应为 nil（NULL），finished_at 为空。
	items, err := logs.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应有一条记录，得 %d", len(items))
	}
	if items[0].Success != nil {
		t.Errorf("进行中 success 应为 nil，得 %v", *items[0].Success)
	}
	if items[0].FinishedAt != nil {
		t.Errorf("进行中 finished_at 应为 nil，得 %q", *items[0].FinishedAt)
	}

	// 结束：成功 + 统计 + 实体构成。
	counts := map[string]int{"asset": 5, "repository": 2, "user": 1}
	if err := logs.Finish(id, true, 8, 8, 8, 0, 3, counts, ""); err != nil {
		t.Fatalf("Finish：%v", err)
	}

	items, err = logs.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应有一条记录，得 %d", len(items))
	}
	e := items[0]
	if e.Success == nil || !*e.Success {
		t.Errorf("成功记录 success 应为 true，得 %v", e.Success)
	}
	if e.FinishedAt == nil || *e.FinishedAt == "" {
		t.Error("完成记录 finished_at 不应为空")
	}
	if e.FromSeq != 5 || e.ToSeq != 8 {
		t.Errorf("水位应 5→8，得 %d→%d", e.FromSeq, e.ToSeq)
	}
	if e.Changes != 8 || e.Applied != 8 || e.Failed != 0 || e.Blobs != 3 {
		t.Errorf("统计不符：changes=%d applied=%d failed=%d blobs=%d", e.Changes, e.Applied, e.Failed, e.Blobs)
	}
	// 实体构成 JSON 往返。
	var got map[string]int
	if err := json.Unmarshal([]byte(e.EntityCounts), &got); err != nil {
		t.Fatalf("entityCounts 非 JSON：%v", err)
	}
	if len(got) != 3 || got["asset"] != 5 || got["repository"] != 2 || got["user"] != 1 {
		t.Errorf("entityCounts 不符：%v", got)
	}
}

// TestSyncLogFail 失败记录：success=false 且带错误摘要。
func TestSyncLogFail(t *testing.T) {
	logs := newTestSyncLog(t)
	id, err := logs.Start("https://repo.example.com", 0)
	if err != nil {
		t.Fatalf("Start：%v", err)
	}
	if err := logs.Finish(id, false, 3, 3, 2, 1, 1, map[string]int{"user": 3}, "拉取复制变更失败：HTTP 401"); err != nil {
		t.Fatalf("Finish：%v", err)
	}
	items, err := logs.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	e := items[0]
	if e.Success == nil || *e.Success {
		t.Errorf("失败记录 success 应为 false，得 %v", e.Success)
	}
	if !strings.Contains(e.ErrorText, "HTTP 401") {
		t.Errorf("错误摘要不符：%q", e.ErrorText)
	}
	if e.Failed != 1 || e.Applied != 2 {
		t.Errorf("失败统计不符：applied=%d failed=%d", e.Applied, e.Failed)
	}
}

// TestSyncLogListOrderAndPaging 列表按开始时间倒序 + 分页。
func TestSyncLogListOrderAndPaging(t *testing.T) {
	logs := newTestSyncLog(t)
	// 写入 3 条（Start 时间递增，List 应倒序返回最新在前）。
	for i := 0; i < 3; i++ {
		id, err := logs.Start("https://repo.example.com", int64(i))
		if err != nil {
			t.Fatalf("Start：%v", err)
		}
		if err := logs.Finish(id, true, int64(i), 0, 0, 0, 0, nil, ""); err != nil {
			t.Fatalf("Finish：%v", err)
		}
	}
	items, err := logs.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) != 3 {
		t.Fatalf("应有 3 条，得 %d", len(items))
	}
	// 倒序：最新（ToSeq=2）在前。
	if items[0].ToSeq != 2 || items[2].ToSeq != 0 {
		t.Errorf("倒序不符：%d, %d, %d", items[0].ToSeq, items[1].ToSeq, items[2].ToSeq)
	}
	// 分页：limit=2 只返回前 2 条。
	page, err := logs.List(2, 0)
	if err != nil {
		t.Fatalf("List 分页：%v", err)
	}
	if len(page) != 2 {
		t.Errorf("limit=2 应返回 2 条，得 %d", len(page))
	}
	// offset=1 跳过第一条。
	page2, err := logs.List(10, 1)
	if err != nil {
		t.Fatalf("List offset：%v", err)
	}
	if len(page2) != 2 || page2[0].ToSeq != 1 {
		t.Errorf("offset=1 应返回后 2 条且首条 ToSeq=1，得 %d", page2[0].ToSeq)
	}
	n, err := logs.Count()
	if err != nil {
		t.Fatalf("Count：%v", err)
	}
	if n != 3 {
		t.Errorf("Count 应为 3，得 %d", n)
	}
}

// TestSyncLogDelete 删除记录（空同步不留痕：进行中记录被移除后列表为空）。
func TestSyncLogDelete(t *testing.T) {
	logs := newTestSyncLog(t)
	id, err := logs.Start("https://repo.example.com", 3)
	if err != nil {
		t.Fatalf("Start：%v", err)
	}
	// 删除（模拟空同步移除进行中记录）。
	if err := logs.Delete(id); err != nil {
		t.Fatalf("Delete：%v", err)
	}
	items, err := logs.List(10, 0)
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if len(items) != 0 {
		t.Errorf("删除后应无记录，得 %d", len(items))
	}
	n, err := logs.Count()
	if err != nil {
		t.Fatalf("Count：%v", err)
	}
	if n != 0 {
		t.Errorf("删除后 Count 应为 0，得 %d", n)
	}
}
