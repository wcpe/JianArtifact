package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// fakeCleanup 记录调用参数，便于断言"接口层没自作主张改语义"。
type fakeCleanup struct {
	previewKind  string
	previewKeep  int
	execOpts     domain.ExecuteOptions
	execCalled   bool
	previewCalls int
	execErr      error
}

func (f *fakeCleanup) Preview(kind string, keepLast int) (domain.CleanupPreview, error) {
	f.previewCalls++
	f.previewKind = kind
	f.previewKeep = keepLast
	return domain.CleanupPreview{
		Kind:       kind,
		Items:      []domain.CleanupCandidate{{Path: "blobs/0a/ea", IsDir: true}},
		ItemCount:  1,
		FreedBytes: 0,
	}, nil
}

func (f *fakeCleanup) Execute(opts domain.ExecuteOptions) (*repository.CleanTask, error) {
	f.execCalled = true
	f.execOpts = opts
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &repository.CleanTask{ID: 7, Kind: opts.Kind, Status: repository.CleanTaskSucceeded, ProcessedItems: 1, TotalItems: 1}, nil
}

// fakeCleanTasks 提供任务查询桩。
type fakeCleanTasks struct {
	items []repository.CleanTask
	total int
}

func (f *fakeCleanTasks) Get(id int64) (*repository.CleanTask, error) {
	for i := range f.items {
		if f.items[i].ID == id {
			return &f.items[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

func (f *fakeCleanTasks) List(limit, offset int) ([]repository.CleanTask, int, error) {
	return f.items, f.total, nil
}

func newCleanupAPIEnv(t *testing.T, cleanup CleanupController, tasks CleanTaskController) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin"})
		c.Next()
	})
	h := NewHandlers(Deps{Cleanup: cleanup, CleanTasks: tasks})
	r.POST("/api/v1/maintenance/cleanup/preview", h.PreviewCleanup)
	r.POST("/api/v1/maintenance/cleanup", h.ExecuteCleanup)
	r.GET("/api/v1/maintenance/clean-tasks", h.ListCleanTasks)
	r.GET("/api/v1/maintenance/clean-tasks/:id", h.GetCleanTask)
	return r
}

// TestPreviewCleanupIsReadOnly 钉住接口层：预览端点**绝不触发执行**。
//
// 变异验证：让 PreviewCleanup 顺手调一次 Execute（"反正已经算出来了"），本用例必须转红。
func TestPreviewCleanupIsReadOnly(t *testing.T) {
	t.Parallel()
	cleanup := &fakeCleanup{}
	r := newCleanupAPIEnv(t, cleanup, &fakeCleanTasks{})

	body, _ := json.Marshal(map[string]any{"kind": "empty_dirs"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（体：%s）", rec.Code, rec.Body.String())
	}
	if cleanup.previewCalls != 1 {
		t.Fatalf("预览调用次数 = %d，期望 1", cleanup.previewCalls)
	}
	if cleanup.execCalled {
		t.Fatal("预览端点绝不能触发执行")
	}
	if cleanup.previewKind != "empty_dirs" {
		t.Fatalf("透传 kind = %q，期望 empty_dirs", cleanup.previewKind)
	}
}

// TestExecuteCleanupReturnsTaskID 钉住执行端点：202 + taskId，且原样透传 kinds/items/keepLast。
func TestExecuteCleanupReturnsTaskID(t *testing.T) {
	t.Parallel()
	cleanup := &fakeCleanup{}
	r := newCleanupAPIEnv(t, cleanup, &fakeCleanTasks{})

	body, _ := json.Marshal(map[string]any{
		"kind":     "pre_restore",
		"items":    []string{"pre-restore-20260101-000000"},
		"keepLast": 2,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（体：%s）", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if out["taskId"] != float64(7) {
		t.Fatalf("taskId = %v，期望 7", out["taskId"])
	}
	if cleanup.execOpts.Kind != "pre_restore" || cleanup.execOpts.KeepLast != 2 || len(cleanup.execOpts.Items) != 1 {
		t.Fatalf("透传参数不符：%+v", cleanup.execOpts)
	}
}

// TestExecuteCleanupRunningConflict 钉住单并发冲突映射为 409 cleanup_running。
func TestExecuteCleanupRunningConflict(t *testing.T) {
	t.Parallel()
	cleanup := &fakeCleanup{execErr: repository.ErrCleanTaskRunning}
	r := newCleanupAPIEnv(t, cleanup, &fakeCleanTasks{})

	body, _ := json.Marshal(map[string]any{"kind": "empty_dirs"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（体：%s）", rec.Code, rec.Body.String())
	}
	var resp Error
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析错误响应：%v", err)
	}
	if resp.Error.Code != "cleanup_running" {
		t.Fatalf("错误码 = %q，期望 cleanup_running", resp.Error.Code)
	}
}

// TestListCleanTasksEmptyIsArray 钉住空清单返回 []（不是 null），避免前端渲染崩。
func TestListCleanTasksEmptyIsArray(t *testing.T) {
	t.Parallel()
	r := newCleanupAPIEnv(t, &fakeCleanup{}, &fakeCleanTasks{items: nil, total: 0})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/maintenance/clean-tasks", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var out struct {
		Items []repository.CleanTask `json:"items"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if out.Items == nil {
		t.Fatal("items 应为空数组而不是 null")
	}
	if out.Total != 0 {
		t.Fatalf("total = %d，期望 0", out.Total)
	}
}
