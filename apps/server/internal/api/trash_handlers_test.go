package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

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

// registerTrashRoutes 挂载回收站端点（与 main.go 的注册一致）。
func registerTrashRoutes(r *gin.Engine, h *Handlers) {
	r.GET("/api/v1/maintenance/trash", h.ListTrash)
	r.POST("/api/v1/maintenance/trash/restore", h.RestoreTrash)
	r.POST("/api/v1/maintenance/trash/purge", h.PurgeTrash)
	r.POST("/api/v1/maintenance/trash/purge-all", h.PurgeAllTrash)
}

// newTrashEnv 按给定依赖组装路基；管理员主体带用户名，便于审计断言 actor。
func newTrashEnv(t *testing.T, deps Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("auth.principal", adminPrincipal())
		c.Next()
	})
	registerTrashRoutes(r, NewHandlers(deps))
	return r
}

func newTrashAPIEnv(t *testing.T, trash TrashController) *gin.Engine {
	t.Helper()
	return newTrashEnv(t, Deps{Trash: trash})
}

// newTrashAuditEnv 组装带**真实审计仓储**的环境：审计断言要看记录是否真的落库。
func newTrashAuditEnv(t *testing.T, trash TrashController) (*gin.Engine, *repository.AuditLogRepo) {
	t.Helper()
	auditLogs := repository.NewAuditLogRepo(openAPITestDB(t))
	return newTrashEnv(t, Deps{Trash: trash, AuditLogs: auditLogs}), auditLogs
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

// TestTrashMutationsAudited 钉住三个破坏性端点各自落一条可区分的审计：
// 恢复 / 彻底删除 / 清空必须能分开筛——合并成一个动作后，审计读者只能靠 detail 猜语义。
//
// 变异验证：把任一处的 AuditLog 调用删掉、或把三个动作写成同一个字面量，本用例必须转红。
func TestTrashMutationsAudited(t *testing.T) {
	t.Parallel()
	trash := &fakeTrash{}
	r, auditLogs := newTrashAuditEnv(t, trash)

	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("构造请求体：%v", err)
			}
			reader = bytes.NewReader(raw)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, reader)
		if reader != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		r.ServeHTTP(rec, req)
		return rec
	}

	// 恢复：请求 2 条，桩回报恢复 1 条 + 冲突 1 条。
	if rec := post("/api/v1/maintenance/trash/restore", map[string]any{"ids": []int64{9, 10}}); rec.Code != http.StatusOK {
		t.Fatalf("恢复状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	// 彻底删除：请求 2 条。
	if rec := post("/api/v1/maintenance/trash/purge", map[string]any{"ids": []int64{11, 12}}); rec.Code != http.StatusOK {
		t.Fatalf("彻底删除状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	// 清空：无请求体。
	if rec := post("/api/v1/maintenance/trash/purge-all", nil); rec.Code != http.StatusOK {
		t.Fatalf("清空状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}

	cases := []struct {
		action     string
		wantDetail []string
	}{
		{"maintenance.trash.restore", []string{"条目数=2", "已恢复=1", "冲突跳过=1"}},
		{"maintenance.trash.purge", []string{"条目数=2", "已删除=2", "释放字节=100"}},
		{"maintenance.trash.purge_all", []string{"条目数=3", "释放字节=300"}},
	}
	for _, tc := range cases {
		entries, err := auditLogs.List(repository.AuditFilter{Action: tc.action})
		if err != nil {
			t.Fatalf("查审计 %s：%v", tc.action, err)
		}
		if len(entries) != 1 {
			t.Fatalf("%s 应写 1 条审计，实际 %d 条", tc.action, len(entries))
		}
		entry := entries[0]
		if entry.EntityType != "maintenance" || entry.EntityKey != "trash" ||
			entry.Result != "ok" || entry.Actor != "admin" {
			t.Fatalf("%s 审计内容不符：%+v", tc.action, entry)
		}
		for _, want := range tc.wantDetail {
			if !strings.Contains(entry.Detail, want) {
				t.Fatalf("%s 审计详情应含 %q，实际 %q", tc.action, want, entry.Detail)
			}
		}
	}
}

// TestTrashRejectedNotAudited 钉住被拒的请求不写审计：
// 空 ids 是「一次误发请求」，不是「谁彻底删了什么」，记下来会让审计与事实不符。
func TestTrashRejectedNotAudited(t *testing.T) {
	t.Parallel()
	trash := &fakeTrash{}
	r, auditLogs := newTrashAuditEnv(t, trash)

	body, _ := json.Marshal(map[string]any{"ids": []int64{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/trash/purge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（体：%s）", rec.Code, rec.Body.String())
	}
	entries, err := auditLogs.List(repository.AuditFilter{})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("被拒的彻底删除不得写审计，实际 %d 条：%+v", len(entries), entries)
	}
}
