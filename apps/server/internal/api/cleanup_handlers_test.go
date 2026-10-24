package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

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

// registerCleanupRoutes 挂载清理相关端点（与 main.go 的注册一致）。
func registerCleanupRoutes(r *gin.Engine, h *Handlers) {
	r.POST("/api/v1/maintenance/cleanup/preview", h.PreviewCleanup)
	r.POST("/api/v1/maintenance/cleanup", h.ExecuteCleanup)
	r.GET("/api/v1/maintenance/clean-tasks", h.ListCleanTasks)
	r.GET("/api/v1/maintenance/clean-tasks/:id", h.GetCleanTask)
}

// newCleanupEnv 按给定依赖组装路基；管理员主体带用户名，便于审计断言 actor。
func newCleanupEnv(t *testing.T, deps Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("auth.principal", adminPrincipal())
		c.Next()
	})
	registerCleanupRoutes(r, NewHandlers(deps))
	return r
}

func newCleanupAPIEnv(t *testing.T, cleanup CleanupController, tasks CleanTaskController) *gin.Engine {
	t.Helper()
	return newCleanupEnv(t, Deps{Cleanup: cleanup, CleanTasks: tasks})
}

// newCleanupAuditEnv 组装带**真实审计仓储**的环境：审计断言要看记录是否真的落库，
// 用桩只能证明"调用过"，证明不了写进去的内容对不对。
func newCleanupAuditEnv(t *testing.T, cleanup CleanupController, tasks CleanTaskController) (*gin.Engine, *repository.AuditLogRepo) {
	t.Helper()
	auditLogs := repository.NewAuditLogRepo(openAPITestDB(t))
	return newCleanupEnv(t, Deps{Cleanup: cleanup, CleanTasks: tasks, AuditLogs: auditLogs}), auditLogs
}

// TestPreviewCleanupIsReadOnly 钉住接口层：预览端点**绝不触发执行**。
//
// 变异验证：让 PreviewCleanup 顺手调一次 Execute（"反正已经算出来了"），本用例必须转红。
func TestPreviewCleanupIsReadOnly(t *testing.T) {
	t.Parallel()
	cleanup := &fakeCleanup{}
	r, auditLogs := newCleanupAuditEnv(t, cleanup, &fakeCleanTasks{})

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
	// 只读不写审计：预览会被反复点击（切类别、调保留数），记下来只会把审计变成访问日志。
	if entries, err := auditLogs.List(repository.AuditFilter{}); err != nil {
		t.Fatalf("查审计：%v", err)
	} else if len(entries) != 0 {
		t.Fatalf("只读预览不得写审计，实际 %d 条：%+v", len(entries), entries)
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

// TestExecuteCleanupAudited 钉住受理成功即落一条 maintenance.cleanup 审计：
// detail 必须写清 kind、请求删除项数与是否整类（规格 0.13.0 审计小节的三项要求）。
//
// 变异验证：把 AuditLog 调用删掉，或用 req.Kind 之外的来源拼 detail，本用例必须转红。
func TestExecuteCleanupAudited(t *testing.T) {
	t.Parallel()
	cleanup := &fakeCleanup{}
	r, auditLogs := newCleanupAuditEnv(t, cleanup, &fakeCleanTasks{})

	// 定向清理：items 非空 → 范围记「定向」，请求项数即 len(items)。
	body, _ := json.Marshal(map[string]any{
		"kind":  "pre_restore",
		"items": []string{"pre-restore-20260101-000000", "pre-restore-20260102-000000"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（体：%s）", rec.Code, rec.Body.String())
	}

	entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceCleanup})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("受理成功应写 1 条审计，实际 %d 条", len(entries))
	}
	entry := entries[0]
	if entry.Action != "maintenance.cleanup" || entry.EntityType != "maintenance" ||
		entry.EntityKey != "cleanup" || entry.Result != "ok" || entry.Actor != "admin" {
		t.Fatalf("审计内容不符：%+v", entry)
	}
	for _, want := range []string{"类型=pre_restore", "请求项数=2", "范围=定向", "任务=7"} {
		if !strings.Contains(entry.Detail, want) {
			t.Fatalf("审计详情应含 %q，实际 %q", want, entry.Detail)
		}
	}
	// 入口只回答"谁发起了什么"：不得把 items 路径抄进 detail（任务表已逐项留痕）。
	if strings.Contains(entry.Detail, "pre-restore-2026010") {
		t.Fatalf("审计详情不应包含具体条目路径，实际 %q", entry.Detail)
	}

	// 整类清理：items 为空 → 范围记「整类」，即使请求项数为 0 也能读出真实范围。
	body, _ = json.Marshal(map[string]any{"kind": "empty_dirs"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("整类清理状态码 = %d，期望 202（体：%s）", rec.Code, rec.Body.String())
	}
	entries, err = auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceCleanup})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("两次受理应写 2 条审计，实际 %d 条", len(entries))
	}
	// 列表按时间倒序返回，但不依赖下标：直接统计两种范围的条数。
	var wholeCategory, targeted int
	for _, e := range entries {
		if strings.Contains(e.Detail, "范围=整类") {
			wholeCategory++
		}
		if strings.Contains(e.Detail, "范围=定向") {
			targeted++
		}
	}
	if wholeCategory != 1 || targeted != 1 {
		t.Fatalf("两次受理应各记一条整类/定向，实际 整类=%d 定向=%d：%+v", wholeCategory, targeted, entries)
	}
}

// TestExecuteCleanupRejectedNotAudited 钉住被拒的受理不写审计：审计只记「已执行的动作」。
//
// 单并发冲突（409）与参数非法（400）都不是「谁删了什么」，记下来会让审计与事实不符。
func TestExecuteCleanupRejectedNotAudited(t *testing.T) {
	t.Parallel()
	cleanup := &fakeCleanup{execErr: repository.ErrCleanTaskRunning}
	r, auditLogs := newCleanupAuditEnv(t, cleanup, &fakeCleanTasks{})

	body, _ := json.Marshal(map[string]any{"kind": "empty_dirs"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（体：%s）", rec.Code, rec.Body.String())
	}

	entries, err := auditLogs.List(repository.AuditFilter{})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("被拒的清理不得写审计，实际 %d 条：%+v", len(entries), entries)
	}
}
