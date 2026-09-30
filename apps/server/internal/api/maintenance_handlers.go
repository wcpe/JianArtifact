package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// 维护作业触发审计动作（FR-41）。取值写进审计库后不可随意更名：
// 前端 auditAction 文案与按 action 的筛选都依赖这个字面量。
const auditActionMaintenanceJobRun = "maintenance.job_run"

// ListMaintenanceJobs 返回本进程已注册的周期作业清单与运行状态（仅管理员）。
//
// 只读：不触发任何作业、不修改任何状态。清单由调度器注册决定
// （间隔 <= 0 的作业不注册），因此"某作业不在清单里"等价于"该作业在本实例被禁用"。
func (h *Handlers) ListMaintenanceJobs(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.maintenanceJobs == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "作业调度器未启用")
		return
	}
	statuses := h.maintenanceJobs.Status()
	// 预分配保证空清单序列化为 []（而不是 null），前端无需区分两种"没有作业"。
	jobs := make([]MaintenanceJob, 0, len(statuses))
	for _, st := range statuses {
		jobs = append(jobs, toAPIMaintenanceJob(st))
	}
	c.JSON(http.StatusOK, MaintenanceJobList{Jobs: jobs})
}

// RunMaintenanceJob 手动触发一次周期作业（仅管理员）。
//
// 返回 202 只表示**已受理**，不等待作业执行完成；触发后是否跑完、是否失败，
// 请查 GET /api/v1/maintenance/jobs 的 running / failures / lastError。
// 本接口不改变既有调度语义：作业正在运行（含本接口触发的那一轮）时不排队、不并发重入。
func (h *Handlers) RunMaintenanceJob(c *gin.Context, name MaintenanceJobNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.maintenanceJobs == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "作业调度器未启用")
		return
	}
	switch err := h.maintenanceJobs.Run(name); {
	case err == nil:
	case errors.Is(err, scheduler.ErrJobNotFound):
		auth.WriteError(c, http.StatusNotFound, "not_found", "作业未注册或已禁用")
		return
	case errors.Is(err, scheduler.ErrJobRunning):
		auth.WriteError(c, http.StatusConflict, "job_running", "作业正在运行，请稍后再试")
		return
	default:
		// 其余失败不向调用方回显内部细节（硬约束：错误响应不回显内部实现信息）。
		auth.WriteError(c, http.StatusInternalServerError, "internal_error", "触发作业失败")
		return
	}
	h.AuditLog(c, auditActionMaintenanceJobRun, "maintenance_job", name, "", "来源=手动触发", "ok")
	c.JSON(http.StatusAccepted, MaintenanceJobRunResult{Name: name, Started: true})
}

// toAPIMaintenanceJob 把调度器状态映射为契约类型。
// 时间与错误字段在契约里是 nullable：未执行过必须**缺省**（序列化为 null），
// 否则 time.Time 零值会变成 "0001-01-01T00:00:00Z"，前端会把它当成真实时间展示。
func toAPIMaintenanceJob(st scheduler.JobStatus) MaintenanceJob {
	job := MaintenanceJob{
		Name:            st.Name,
		IntervalSeconds: int64(st.Interval / time.Second),
		Running:         st.Running,
		Runs:            st.Runs,
		Failures:        st.Failures,
	}
	if !st.LastStart.IsZero() {
		startedAt := st.LastStart.UTC()
		job.LastStartedAt = &startedAt
	}
	if !st.LastFinish.IsZero() {
		finishedAt := st.LastFinish.UTC()
		job.LastFinishedAt = &finishedAt
	}
	if st.LastError != "" {
		lastError := st.LastError
		job.LastError = &lastError
	}
	return job
}
