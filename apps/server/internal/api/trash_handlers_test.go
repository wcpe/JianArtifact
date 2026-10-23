package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// fakeTrash 记录调用参数，便于断言接口层如实透传、不自作主张。
type fakeTrash struct {
	entries      []repository.TrashEntry
	total        int
	stats        repository.TrashStats
	restoreIDs   []int64
	purgeIDs     []int64
	purgeAllCall int
}

func (f *fakeTrash) List(limit, offset int) ([]repository.TrashEntry, int, error) {
	return f.entries, f.total, nil
}

func (f *fakeTrash) Stats() (repository.TrashStats, error) { return f.stats, nil }

func (f *fakeTrash) Restore(ids []int64) (domain.TrashRestoreResult, error) {
	f.restoreIDs = ids
	return domain.TrashRestoreResult{
		Restored: 1,
		Conflicts: []domain.TrashConflict{
			{ID: 9, OriginalPath: "/data/backup-uploads/up-9", Reason: "目标位置已存在，未覆盖"},
		},
	}, nil
}

func (f *fakeTrash) Purge(ids []int64) (domain.TrashPurgeResult, error) {
	f.purgeIDs = ids
	return domain.TrashPurgeResult{Purged: len(ids), FreedBytes: 100}, nil
}

func (f *fakeTrash) PurgeAll() (domain.TrashPurgeResult, error) {
	f.purgeAllCall++
	return domain.TrashPurgeResult{Purged: 3, FreedBytes: 300}, nil
}

func newTrashAPIEnv(t *testing.T, trash TrashController) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin"})
		c.Next()
	})
	h := NewHandlers(Deps{Trash: trash})
	r.GET("/api/v1/maintenance/trash", h.ListTrash)
	r.POST("/api/v1/maintenance/trash/restore", h.RestoreTrash)
	r.POST("/api/v1/maintenance/trash/purge", h.PurgeTrash)
	r.POST("/api/v1/maintenance/trash/purge-all", h.PurgeAllTrash)
	return r
}

// TestListTrashReportsStats 钉住列表同时回占用：界面要显示"回收站占了多少"。
func TestListTrashReportsStats(t *testing.T) {
	t.Parallel()
	trash := &fakeTrash{entries: nil, total: 0, stats: repository.TrashStats{Entries: 0, Bytes: 0}}
	r := newTrashAPIEnv(t, trash)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/maintenance/trash", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []repository.TrashEntry `json:"items"`
		Stats repository.TrashStats   `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if out.Items == nil {
		t.Fatal("items 应为空数组而不是 null")
	}
	if out.Stats.Entries != 0 {
		t.Fatalf("stats.entries = %d，期望 0", out.Stats.Entries)
	}
}

// TestRestoreTrashSurfacesConflicts 钉住恢复接口如实回传冲突（而不是把冲突说成失败或静默吞掉）。
//
// 冲突是**保护性跳过**：目标已存在时我们选择不覆盖。若不回传，用户只看到"恢复了 0 条"
// 却不知道原因，会反复点击。
func TestRestoreTrashSurfacesConflicts(t *testing.T) {
	t.Parallel()
	trash := &fakeTrash{}
	r := newTrashAPIEnv(t, trash)

	body, _ := json.Marshal(map[string]any{"ids": []int64{9}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/trash/restore", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	if len(trash.restoreIDs) != 1 || trash.restoreIDs[0] != 9 {
		t.Fatalf("透传 ids 不符：%v", trash.restoreIDs)
	}
	var out struct {
		Restored  int                    `json:"restored"`
		Conflicts []domain.TrashConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if out.Restored != 1 {
		t.Fatalf("restored = %d，期望 1", out.Restored)
	}
	if len(out.Conflicts) != 1 || out.Conflicts[0].ID != 9 {
		t.Fatalf("冲突未如实回传：%+v", out.Conflicts)
	}
}

// TestPurgeTrashRequiresIDs 钉住空 ids 被拒（避免一次误发请求变成"清空"）。
func TestPurgeTrashRequiresIDs(t *testing.T) {
	t.Parallel()
	trash := &fakeTrash{}
	r := newTrashAPIEnv(t, trash)

	body, _ := json.Marshal(map[string]any{"ids": []int64{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/trash/purge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（体：%s）", rec.Code, rec.Body.String())
	}
	if len(trash.purgeIDs) != 0 {
		t.Fatalf("空 ids 不应调用 Purge，实际调用 %d 次", len(trash.purgeIDs))
	}
}

// TestPurgeAllTrashNoBody 钉住清空端点无需请求体也能工作（前端只发空 POST）。
func TestPurgeAllTrashNoBody(t *testing.T) {
	t.Parallel()
	trash := &fakeTrash{}
	r := newTrashAPIEnv(t, trash)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/trash/purge-all", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	if trash.purgeAllCall != 1 {
		t.Fatalf("PurgeAll 调用次数 = %d，期望 1", trash.purgeAllCall)
	}
}
