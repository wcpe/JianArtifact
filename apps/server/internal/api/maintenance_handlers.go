package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// 维护作业触发审计动作。取值写进审计库后不可随意更名：
// 前端 auditAction 文案与按 action 的筛选都依赖这个字面量。
const auditActionMaintenanceJobRun = "maintenance.job_run"

// 一键重启的审计动作。同前：入库后不可随意更名。
const auditActionMaintenanceServiceRestart = "maintenance.service_restart"

// restartDelay 是受理一键重启后、到真正开始重启（关停 + 自替换）之间的延迟。
//
// 为什么必须有这段延迟（本功能的核心顺序契约）：重启会关停服务并换掉本进程的映像。
// 若"先重启再回响应"，202 根本发不出去——前端只能看到连接被重置，
// 既分不清"重启成功"还是"网络抖动"，也拿不到 command / supported 这些降级信息。
// 因此 handler 先起一个延迟触发的后台任务，再**立刻**写出 202；
// 1.8 秒足够响应送达客户端并由前端完成"已受理"的渲染，又不至于让管理员白等。
const restartDelay = 1800 * time.Millisecond

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

// RestartService 一键重启本节点（仅管理员）。
//
// 执行路径是**进程自替换**：本进程本就有权执行自己的二进制（systemd 的 ExecStart 就是它），
// syscall.Exec 只是"同一进程换新映像"，**不引入任何新权限**——因此不需要 sudo / polkit /
// sudoers，也不受 NoNewPrivileges 影响。响应里的 command 是**手工等效命令**（给运维复制执行），
// 不是本进程将要执行的命令。
//
// 响应契约（前端按此实现，不可随意改动）：
//   - 能力预检通过 → **202** + supported:true + command + scope + delayMs，
//     重启在 delayMs 之后开始（响应先送达，见 restartDelay 的说明）；
//   - 预检不通过（平台不支持自替换 / argv[0] 不可执行 / 单元名或作用域配置非法 /
//     docker 作用域 / 触发点未接线）→ **200** + supported:false + reason + command。
//
// 为什么降级走 200 而不是 4xx/5xx：supported:false 是**正常的业务结果**，
// 不是错误。前端要拿它渲染"本机不支持自动重启，请手动执行这条命令"的降级 UI；
// 用错误码返回会把可操作的信息（command）埋进错误路径，调用方反而更难处理。
// 本接口也**绝不假装成功**：没真正受理就一定如实回 supported:false。
//
// 安全：单元名只来自环境变量（domain 侧还会做字符集白名单校验），
// **绝不**从请求体读取再拼进命令——那是一条直通命令注入的路径（见 restart_service.go）。
// 注意该白名单保护的是 command 这条**会被复制到终端粘贴执行**的展示串，不是自替换本身
// （自替换用的是本进程自己的 os.Args，与单元名无关）。
func (h *Handlers) RestartService(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.restart == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "一键重启未启用")
		return
	}

	// 同步预检：把"能不能重启"在响应之前一次定下来。
	// 后台失败对调用方不可观测（进程随即换映像、响应早已发出），因此必须前移。
	plan := h.restart.Probe()
	result := h.restartResult(plan)
	if plan.Supported {
		delayMs := int64(restartDelay / time.Millisecond)
		result.DelayMs = &delayMs
		// 先排定后台重启，再写响应：两个动作都在本函数内完成，
		// 但真正的重启（关停 + 自替换）要等 restartDelay 才开始，响应因此必然先送达。
		h.restart.RestartAfter(plan, restartDelay)
		h.AuditLog(c, auditActionMaintenanceServiceRestart, "maintenance", "service", "",
			"来源=手动触发 受理=是 作用域="+h.restart.Scope()+" 命令="+result.Command, "ok")
		c.JSON(http.StatusAccepted, result)
		return
	}

	// 降级路径：reason 用指针赋值（契约里是可选字段，零值会序列化成空串）。
	h.AuditLog(c, auditActionMaintenanceServiceRestart, "maintenance", "service", "",
		"来源=手动触发 受理=否 原因="+plan.Reason, "failed")
	c.JSON(http.StatusOK, result)
}

// GetRestartCapability 只读探测本节点的重启能力（仅管理员）。
//
// 存在的理由（本端点最重要的一条约束）：`command` 是**手工等效命令**，随部署形态而变
// （root / 非 root 的 systemd-user / docker），只有后端知道本机该敲哪一条。界面此前硬编码
// `sudo systemctl restart <unit>`，而自带单元带 `NoNewPrivileges=true`，该 flag 会阻止
// sudo 提权——于是同一台机器上同时显示两条命令，照做的那条必然失败。命令的真源只能是后端。
//
// 本判据已与执行路径解耦：自动重启走**进程自替换**（不执行任何命令、不需要任何权限），
// 而 command 只用于"手工重启"与降级出路，因此它不参与自动重启的可用性判定。
//
// **为什么不能复用 POST 来探测：`POST` 在 supported:true 时会当场排定重启
// （见 RestartService 里的 h.restart.RestartAfter）。前端若"打开弹窗即调用它取 command"，
// 就变成"打开页面即重启"。** 因此本 handler **只调 Probe、绝不调 RestartAfter**，
// 把"读能力"与"触发重启"在接口层彻底分开：读操作无论被调用多少次、被谁轮询，都不会
// 让进程重启。这也是本端点不写审计的原因——只读探测不改变任何状态，
// 与 ListMaintenanceJobs 一致，读操作不落审计。
func (h *Handlers) GetRestartCapability(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.restart == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "一键重启未启用")
		return
	}

	// 与 POST 同一套探测口径（同一个 Probe、同一个结果映射），因此本端点回显的 command
	// 与 POST 受理后真正执行的命令同源：界面拿它渲染可复制命令，照抄就能在这台机器上跑通。
	// 刻意**不**设置 delayMs：没有受理就没有延迟可言（契约里该字段本就可选）。
	c.JSON(http.StatusOK, h.restartResult(h.restart.Probe()))
}

// restartResult 把一次预检结论映射为契约类型，供"只读探测"与"受理重启"两个 handler 共用。
//
// 抽出来是为了避免两处漂移：两个端点回显的 supported / command / scope 必须逐字一致，
// 否则界面（用 GET 探测）与后端实际执行（POST 的 plan）会各说一套。
// reason 与 delayMs 由调用方按语义补充：reason 仅在探测失败时有意义，
// delayMs 仅在真正受理时有意义。
func (h *Handlers) restartResult(plan domain.RestartPlan) MaintenanceRestartResult {
	result := MaintenanceRestartResult{
		Supported: plan.Supported,
		// command 与执行同源：两者都取本次预检的同一个结论，
		// 避免"响应展示一条、后台跑另一条"这种事后无从复现的失效。
		Command: plan.Command,
	}
	if scope := h.restart.Scope(); scope != "" {
		s := MaintenanceRestartResultScope(scope)
		result.Scope = &s
	}
	if !plan.Supported {
		// 降级路径：reason 用指针赋值（契约里是可选字段，零值会序列化成空串）。
		reason := plan.Reason
		result.Reason = &reason
	}
	return result
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
