package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// 编译期断言：真实调度器直接满足接口，wiring 只需一行赋值（无需适配器）。
var _ MaintenanceJobController = (*scheduler.Scheduler)(nil)

// fakeMaintenanceJobs 是可注入的调度器桩：状态快照与按名触发结果由用例预设，
// 使接口层的鉴权 / 状态码 / 审计行为可脱离真实调度器验证。
type fakeMaintenanceJobs struct {
	mu        sync.Mutex
	statuses  []scheduler.JobStatus
	runErrs   map[string]error
	triggered []string
}

func (f *fakeMaintenanceJobs) Status() []scheduler.JobStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]scheduler.JobStatus(nil), f.statuses...)
}

func (f *fakeMaintenanceJobs) Run(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggered = append(f.triggered, name)
	return f.runErrs[name]
}

// triggeredNames 返回已受理的触发名序列（副本）。
func (f *fakeMaintenanceJobs) triggeredNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.triggered...)
}

// serveMaintenanceRoute 经生成的 ServerInterfaceWrapper 发起请求（测试口径即线上口径）。
func serveMaintenanceRoute(h *Handlers, principal *auth.Principal, method, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("auth.principal", principal)
		}
	})
	siw := ServerInterfaceWrapper{
		Handler: h,
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			c.JSON(statusCode, gin.H{"msg": err.Error()})
		},
	}
	router.GET("/api/v1/maintenance/jobs", siw.ListMaintenanceJobs)
	router.POST("/api/v1/maintenance/jobs/:name/run", siw.RunMaintenanceJob)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// 运维作业面仅管理员可见：匿名 401、普通用户 403（读与触发同边界）。
func TestMaintenanceJobsRequireAdmin(t *testing.T) {
	fake := &fakeMaintenanceJobs{}
	h := NewHandlers(Deps{MaintenanceJobs: fake})

	cases := []struct {
		name      string
		principal *auth.Principal
		method    string
		path      string
		want      int
	}{
		{"匿名读清单", nil, http.MethodGet, "/api/v1/maintenance/jobs", http.StatusUnauthorized},
		{"普通用户读清单", userPrincipal(), http.MethodGet, "/api/v1/maintenance/jobs", http.StatusForbidden},
		{"匿名触发作业", nil, http.MethodPost, "/api/v1/maintenance/jobs/blob-gc/run", http.StatusUnauthorized},
		{"普通用户触发作业", userPrincipal(), http.MethodPost, "/api/v1/maintenance/jobs/blob-gc/run", http.StatusForbidden},
	}
	for _, tc := range cases {
		rec := serveMaintenanceRoute(h, tc.principal, tc.method, tc.path)
		if rec.Code != tc.want {
			t.Errorf("%s：应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	// 越权请求一律不得落地触发，也不得写审计。
	if got := fake.triggeredNames(); len(got) != 0 {
		t.Fatalf("越权请求不得触发作业，实际触发 %v", got)
	}
}

// 清单形状：字段与契约 MaintenanceJob 一一对应；未执行过的作业时间与错误必须是 null
// （缺省），不能落成零值时间 "0001-01-01T00:00:00Z"。
func TestListMaintenanceJobsReturnsStatusShape(t *testing.T) {
	startedAt := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(3 * time.Second)
	fake := &fakeMaintenanceJobs{statuses: []scheduler.JobStatus{
		{Name: "blob-gc", Interval: 24 * time.Hour},
		{
			Name: "storage-cleanup", Interval: 90 * time.Minute, Runs: 5, Failures: 1,
			Running: true, LastStart: startedAt, LastFinish: finishedAt, LastError: "隔离区目录不可读",
		},
	}}
	h := NewHandlers(Deps{MaintenanceJobs: fake})

	rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员读清单应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析清单：%v", err)
	}
	jobs, ok := payload["jobs"].([]any)
	if !ok || len(jobs) != 2 {
		t.Fatalf("清单应含 2 个作业（按注册顺序），得 %v", payload["jobs"])
	}

	// 未执行过的作业：计数为 0、running false，时间与错误缺省（null）。
	never := jobs[0].(map[string]any)
	if never["name"] != "blob-gc" || never["intervalSeconds"].(float64) != 86400 ||
		never["running"].(bool) || never["runs"].(float64) != 0 || never["failures"].(float64) != 0 {
		t.Fatalf("首个作业字段不符：%v", never)
	}
	for _, key := range []string{"lastStartedAt", "lastFinishedAt", "lastError"} {
		if value, exists := never[key]; exists && value != nil {
			t.Fatalf("未执行过的作业 %s 应为 null/缺省，得 %v", key, value)
		}
	}

	// 已执行过且失败过的作业：状态如实回显。
	ran := jobs[1].(map[string]any)
	if ran["name"] != "storage-cleanup" || ran["intervalSeconds"].(float64) != 5400 ||
		!ran["running"].(bool) || ran["runs"].(float64) != 5 || ran["failures"].(float64) != 1 ||
		ran["lastError"] != "隔离区目录不可读" {
		t.Fatalf("第二个作业字段不符：%v", ran)
	}
	if ran["lastStartedAt"] != "2026-09-30T10:00:00Z" || ran["lastFinishedAt"] != "2026-09-30T10:00:03Z" {
		t.Fatalf("执行时间应按 UTC RFC3339 回显：%v", ran)
	}

	// 契约要求 jobs 恒为数组：无作业时序列化为 []，前端无需区分两种"没有作业"。
	empty := NewHandlers(Deps{MaintenanceJobs: &fakeMaintenanceJobs{}})
	rec = serveMaintenanceRoute(empty, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/jobs")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"jobs":[]}` {
		t.Fatalf("空清单应返回 200 与 {\"jobs\":[]}，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 成功触发：202 + {name, started:true}，受理后写一条 maintenance.job_run 审计。
func TestRunMaintenanceJobAcceptedAndAudited(t *testing.T) {
	db := openAPITestDB(t)
	auditLogs := repository.NewAuditLogRepo(db)
	fake := &fakeMaintenanceJobs{}
	h := NewHandlers(Deps{MaintenanceJobs: fake, AuditLogs: auditLogs})

	rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/maintenance/jobs/storage-cleanup/run")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("触发应 202，得 %d：%s", rec.Code, rec.Body.String())
	}
	var result MaintenanceJobRunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析触发结果：%v", err)
	}
	if result.Name != "storage-cleanup" || !result.Started {
		t.Fatalf("触发结果应为 {name:storage-cleanup started:true}，得 %+v", result)
	}
	if got := fake.triggeredNames(); len(got) != 1 || got[0] != "storage-cleanup" {
		t.Fatalf("应恰好受理一次触发，实际 %v", got)
	}

	entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceJobRun})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("触发成功应写 1 条审计，实际 %d 条", len(entries))
	}
	entry := entries[0]
	if entry.Action != "maintenance.job_run" || entry.EntityType != "maintenance_job" ||
		entry.EntityKey != "storage-cleanup" || entry.Result != "ok" || entry.Actor != "admin" {
		t.Fatalf("审计内容不符：%+v", entry)
	}
}

// 未知作业名 404（not_found）、运行中 409（job_running）：都可判别，且都不写审计。
func TestRunMaintenanceJobErrorMapping(t *testing.T) {
	auditLogs := repository.NewAuditLogRepo(openAPITestDB(t))
	fake := &fakeMaintenanceJobs{runErrs: map[string]error{
		"ghost": scheduler.ErrJobNotFound,
		"busy":  scheduler.ErrJobRunning,
	}}
	h := NewHandlers(Deps{MaintenanceJobs: fake, AuditLogs: auditLogs})

	cases := []struct {
		name     string
		path     string
		want     int
		wantCode string
	}{
		{"未知作业名", "/api/v1/maintenance/jobs/ghost/run", http.StatusNotFound, "not_found"},
		{"作业运行中", "/api/v1/maintenance/jobs/busy/run", http.StatusConflict, "job_running"},
	}
	for _, tc := range cases {
		rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodPost, tc.path)
		if rec.Code != tc.want {
			t.Errorf("%s：应 %d，得 %d：%s", tc.name, tc.want, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s：解析错误响应：%v", tc.name, err)
			continue
		}
		if body.Error.Code != tc.wantCode || body.Error.Message == "" {
			t.Errorf("%s：错误码应为 %s，得 %+v", tc.name, tc.wantCode, body.Error)
		}
	}

	// 被拒的触发不得写审计（只有成功受理才记）。
	entries, err := auditLogs.List(repository.AuditFilter{Action: auditActionMaintenanceJobRun})
	if err != nil {
		t.Fatalf("查审计：%v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("被拒的触发不得写审计，实际 %d 条", len(entries))
	}
}

// 未接线调度器时端点明确不可用（503），不静默返回空清单或假成功。
func TestMaintenanceJobsUnavailableWithoutScheduler(t *testing.T) {
	h := NewHandlers(Deps{})
	if rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/jobs"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线时读清单应 503，得 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/maintenance/jobs/blob-gc/run"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线时触发应 503，得 %d：%s", rec.Code, rec.Body.String())
	}
}

// 触发失败但非可判别错误（调度器内部异常）→ 500，且不回显内部细节。
func TestRunMaintenanceJobUnexpectedErrorHidesDetail(t *testing.T) {
	fake := &fakeMaintenanceJobs{runErrs: map[string]error{"weird": errors.New("内部：blob 根目录 /srv/data 不可写")}}
	h := NewHandlers(Deps{MaintenanceJobs: fake})

	rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/maintenance/jobs/weird/run")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("内部异常应 500，得 %d：%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/srv/data") || strings.Contains(body, "blob 根目录") {
		t.Fatalf("错误响应不得回显内部细节（含文件系统路径），得 %s", body)
	}
}

// 端到端串联真实调度器：手动触发 → 作业真的跑一轮 → 清单如实回显（runs=1、已结束）。
func TestRunMaintenanceJobTriggersRealScheduler(t *testing.T) {
	ran := make(chan struct{})
	sched := scheduler.New()
	// 间隔 1 小时且不 Start：只可能被手动触发跑起来，排除周期触发的干扰。
	sched.Register("manual-only", time.Hour, func(context.Context) error {
		close(ran)
		return nil
	})
	h := NewHandlers(Deps{MaintenanceJobs: sched})

	rec := serveMaintenanceRoute(h, adminPrincipal(), http.MethodPost, "/api/v1/maintenance/jobs/manual-only/run")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("触发真实调度器应 202，得 %d：%s", rec.Code, rec.Body.String())
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("受理后作业未执行")
	}

	rec = serveMaintenanceRoute(h, adminPrincipal(), http.MethodGet, "/api/v1/maintenance/jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("读清单应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var list MaintenanceJobList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析清单：%v", err)
	}
	if len(list.Jobs) != 1 {
		t.Fatalf("清单应含 1 个作业，得 %d", len(list.Jobs))
	}
	job := list.Jobs[0]
	if job.Name != "manual-only" || job.IntervalSeconds != 3600 || job.Runs != 1 || job.Failures != 0 {
		t.Fatalf("触发后清单应回显真实状态：%+v", job)
	}
	if job.LastStartedAt == nil || job.LastFinishedAt == nil || job.LastError != nil {
		t.Fatalf("执行过的作业应带时间且无错误：%+v", job)
	}
}
