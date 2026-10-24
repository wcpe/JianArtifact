package api

// 定向清理与清理任务队列的运维端点，均**仅管理员**、均为**非契约端点**。
//
// 为什么不进 api/openapi.yaml：它们服务运维手动排障与清理，形状会随清理能力演进，
// 不属于协议客户端依赖的稳定面（与 /api/v1/search/index 同一先例）。登记处是 docs/API.md。
//
// 为什么执行端点返回 202 + taskId 而不是同步返回结果：清理是破坏性动作，调用方需要
// 一个可查询的凭据来核对"到底动了哪些东西"。任务记录同时是**审计载面**——
// 只回一个 200 而不留凭据，事后无法回答"这次清理删了什么"。

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 定向清理执行的审计动作。取值写进审计库后不可随意更名：
// 前端 auditAction 文案与按 action 的筛选都依赖这个字面量。
//
// 用维护类动作名（maintenance.*）而不是仓库维度的 repo.cleanup：清理的四类候选都是
// 数据目录下的运维残留，与单个仓库不是同一维度（口径对齐 maintenance.job_run /
// maintenance.negative_cache_purge）。
const auditActionMaintenanceCleanup = "maintenance.cleanup"

// CleanupController 是接口层对清理能力的依赖（便于测试注入桩）。
type CleanupController interface {
	Preview(kind string, keepLast int) (domain.CleanupPreview, error)
	Execute(opts domain.ExecuteOptions) (*repository.CleanTask, error)
}

// CleanTaskController 提供清理任务队列的只读查询。
type CleanTaskController interface {
	Get(id int64) (*repository.CleanTask, error)
	List(limit, offset int) ([]repository.CleanTask, int, error)
}

// cleanupRequest 是预览与执行共用的请求体。
type cleanupRequest struct {
	Kind string `json:"kind"`
	// Items 省略 = 整类全收；给出 = 只收这些（相对数据目录路径）。
	Items    []string `json:"items"`
	KeepLast int      `json:"keepLast"`
}

// PreviewCleanup 计算候选集并返回清单（**纯只读**）。
//
// 不写审计：本端点不删除、不移动、不写库，只把"此刻有哪些东西可回收"算出来给管理员看。
// 只读操作留痕会把审计变成访问日志——真正需要追责的执行动作反而被埋进噪音里；而且预览
// 会被前端反复点击（切换类别、调整保留数各算一次），记录它们无助于回答"东西是谁删的"。
// 口径与负缓存查看端点、作业清单查询一致：写动作才记审计。
func (h *Handlers) PreviewCleanup(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.cleanup == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "清理服务未启用")
		return
	}
	var req cleanupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	preview, err := h.cleanup.Preview(req.Kind, req.KeepLast)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, preview)
}

// ExecuteCleanup 受理一次清理（移入回收站）并返回任务标识。
//
// 响应 202：任务已建立并**同步执行完毕**（四类候选都是毫秒级），但语义上仍按"已受理"
// 返回 taskId，让调用方用统一方式核对结果——不为"快"而在接口形状上分叉。
func (h *Handlers) ExecuteCleanup(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.cleanup == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "清理服务未启用")
		return
	}
	var req cleanupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	task, err := h.cleanup.Execute(domain.ExecuteOptions{
		Kind:     req.Kind,
		Items:    req.Items,
		KeepLast: req.KeepLast,
	})
	if err != nil {
		// 单并发冲突走 409，与既有作业手动触发的口径一致。
		if errors.Is(err, repository.ErrCleanTaskRunning) {
			auth.WriteError(c, http.StatusConflict, "cleanup_running", "已有清理任务正在执行")
			return
		}
		writeDomainErr(c, err)
		return
	}
	// 受理即记一条审计：清理会把文件移出原位置（虽可恢复），"谁在何时用哪个 kind 动了多少项"
	// 必须可追溯。只在受理成功后记——参数非法、类别未知、单并发冲突都已提前返回，
	// 一次被拒的请求不是"已执行的动作"，记下来会让审计与事实不符。
	//
	// 不另记 maintenance.cleanup_done：执行结果（状态 / 进度 / 释放字节 / 失败原因）已经在
	// clean_task 表里，审计只回答"谁发起了什么"，避免一次清理产生两条噪音记录
	// （口径对齐 maintenance.job_run，见规格 0.13.0 审计小节）。
	h.AuditLog(c, auditActionMaintenanceCleanup, "maintenance", "cleanup", "",
		cleanupAuditDetail(task, len(req.Items)), "ok")
	c.JSON(http.StatusAccepted, gin.H{
		"taskId":         task.ID,
		"status":         task.Status,
		"kind":           task.Kind,
		"processedItems": task.ProcessedItems,
		"totalItems":     task.TotalItems,
	})
}

// cleanupAuditDetail 生成清理审计详情：kind、请求删除项数、是否整类（items 为空即整类），
// 外加受理项数与任务号。
//
// 为什么补这两个字段：整类清理时"请求项数"是 0，只靠它读不出这次到底动了多大的范围，
// 而受理项数是服务端按白名单重算后的真实规模；任务号则把这条审计与 clean_task 记录对上，
// 事后才能从"记了一笔"走到"逐项看删了什么"。
//
// 不列 items 的具体路径：任务表与回收站条目已逐项留痕，审计详情保持单行可读，也避免
// 一次整类清理把详情撑爆。kind 在此处必然属于四个已校验的类别（未知 kind 在执行前就报错
// 返回，走不到这里），因此无需额外转义即不含控制字符。
func cleanupAuditDetail(task *repository.CleanTask, requestedItems int) string {
	scope := "定向"
	if requestedItems == 0 {
		scope = "整类"
	}
	return "类型=" + task.Kind +
		" 请求项数=" + strconv.Itoa(requestedItems) +
		" 范围=" + scope +
		" 受理项数=" + strconv.Itoa(task.TotalItems) +
		" 任务=" + strconv.FormatInt(task.ID, 10)
}

// GetCleanTask 返回单个清理任务的进度与结果。
func (h *Handlers) GetCleanTask(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.cleanTasks == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "清理任务查询未启用")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "任务 ID 非法")
		return
	}
	task, err := h.cleanTasks.Get(id)
	if err != nil {
		auth.WriteError(c, http.StatusNotFound, "not_found", "任务不存在")
		return
	}
	c.JSON(http.StatusOK, task)
}

// ListCleanTasks 分页返回清理任务历史（供"任务面板"展示）。
func (h *Handlers) ListCleanTasks(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.cleanTasks == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "清理任务查询未启用")
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	tasks, total, err := h.cleanTasks.List(size, (page-1)*size)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	if tasks == nil {
		tasks = []repository.CleanTask{}
	}
	c.JSON(http.StatusOK, gin.H{
		"items":    tasks,
		"total":    total,
		"page":     page,
		"pageSize": size,
	})
}
