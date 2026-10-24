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
	c.JSON(http.StatusAccepted, gin.H{
		"taskId":         task.ID,
		"status":         task.Status,
		"kind":           task.Kind,
		"processedItems": task.ProcessedItems,
		"totalItems":     task.TotalItems,
	})
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
