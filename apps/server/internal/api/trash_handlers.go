package api

// 回收站端点：列表 / 恢复 / 彻底删除 / 清空，均**仅管理员**、**非契约端点**。
//
// 破坏性分级的差异在这里体现得很直接：
//   - `restore` 是**反向**操作（把东西放回去），接口层不做二次防护——否则误删之后想取回
//     还得再过三道关，反而助长误操作；
//   - `purge` / `purge-all` 是不可逆的，防护在界面上做（预览 + 二次确认 + 确认词），
//     接口层只负责如实执行。

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 回收站动作的审计动作。取值写进审计库后不可随意更名：
// 前端 auditAction 文案与按 action 的筛选都依赖这些字面量。
//
// 三个动作分开记而不是统一记 maintenance.trash：恢复是**可逆的反向**操作，彻底删除与清空
// 不可逆，事后排查时必须能一眼区分"东西是放回去了还是真没了"——合并成一个动作会让审计
// 读者只能靠 detail 猜语义。清空单独成 maintenance.trash.purge_all 也便于把"一次误点清空"
// 这类高危事件单独筛出来。
const (
	auditActionMaintenanceTrashRestore  = "maintenance.trash.restore"
	auditActionMaintenanceTrashPurge    = "maintenance.trash.purge"
	auditActionMaintenanceTrashPurgeAll = "maintenance.trash.purge_all"
)

// TrashController 是接口层对回收站的依赖。
type TrashController interface {
	List(limit, offset int) ([]repository.TrashEntry, int, error)
	Stats() (repository.TrashStats, error)
	Restore(ids []int64) (domain.TrashRestoreResult, error)
	Purge(ids []int64) (domain.TrashPurgeResult, error)
	PurgeAll() (domain.TrashPurgeResult, error)
}

type trashIDsRequest struct {
	IDs []int64 `json:"ids"`
}

// ListTrash 分页列出回收站条目。
func (h *Handlers) ListTrash(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.trash == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "回收站未启用")
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
	entries, total, err := h.trash.List(size, (page-1)*size)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	stats, err := h.trash.Stats()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	if entries == nil {
		entries = []repository.TrashEntry{}
	}
	// 同时回占用：界面要显示"回收站占了多少"，否则用户无法判断该不该清空。
	c.JSON(http.StatusOK, gin.H{
		"items":    entries,
		"total":    total,
		"page":     page,
		"pageSize": size,
		"stats":    stats,
	})
}

// RestoreTrash 恢复指定条目（反向操作，不做二次防护）。
func (h *Handlers) RestoreTrash(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.trash == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "回收站未启用")
		return
	}
	var req trashIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	if len(req.IDs) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "ids 不能为空")
		return
	}
	result, err := h.trash.Restore(req.IDs)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 恢复也要留痕：它是反向操作，但与删除**成对可对账**——只看删除审计无法判断
	// "东西回来了没有"。只在恢复调用成功后记，空 ids / 格式错误已提前 400 返回。
	h.AuditLog(c, auditActionMaintenanceTrashRestore, "maintenance", "trash", "",
		trashRestoreAuditDetail(len(req.IDs), result), "ok")
	c.JSON(http.StatusOK, result)
}

// PurgeTrash 彻底删除指定条目（不可逆）。
func (h *Handlers) PurgeTrash(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.trash == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "回收站未启用")
		return
	}
	var req trashIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	if len(req.IDs) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "ids 不能为空")
		return
	}
	result, err := h.trash.Purge(req.IDs)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 不可逆动作必须留痕：一旦记不上"谁在何时彻底删了 N 条"，事后没有任何其它载面能还原。
	// 空 ids / 格式错误已提前 400 返回，不会记成一次已执行的删除。
	h.AuditLog(c, auditActionMaintenanceTrashPurge, "maintenance", "trash", "",
		trashPurgeAuditDetail(len(req.IDs), result), "ok")
	c.JSON(http.StatusOK, result)
}

// PurgeAllTrash 清空回收站（不可逆）。
func (h *Handlers) PurgeAllTrash(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.trash == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "回收站未启用")
		return
	}
	result, err := h.trash.PurgeAll()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 清空是全库范围、一次点击毁掉整座回收站，单独记一条 purge_all：既有审计里最难复原的
	// 事件，也是"是不是某次误点清掉了一切"的唯一可查凭据。
	h.AuditLog(c, auditActionMaintenanceTrashPurgeAll, "maintenance", "trash", "",
		trashPurgeAllAuditDetail(result), "ok")
	c.JSON(http.StatusOK, result)
}

// trashRestoreAuditDetail 生成恢复审计详情：请求条目数、实际恢复数、保护性跳过数。
//
// 三者分开是因为它们语义不同：请求数说明调用方点了多少条，恢复数是真正回到原位置的条数，
// 冲突跳过是"目标已存在所以没动"的条数——只记请求数会让一次全部冲突的恢复看起来像成功。
// 只放计数不放条目 ID 与路径：回收站条目本身逐条留痕，审计只需回答"谁动了多少条"。
func trashRestoreAuditDetail(requested int, result domain.TrashRestoreResult) string {
	return "条目数=" + strconv.Itoa(requested) +
		" 已恢复=" + strconv.Itoa(result.Restored) +
		" 冲突跳过=" + strconv.Itoa(len(result.Conflicts))
}

// trashPurgeAuditDetail 生成彻底删除审计详情：请求条目数、实际删除数、释放字节。
// 请求数与删除数可能不等（部分条目已不存在），两者都记才能发现"点了 10 条只删掉 3 条"。
func trashPurgeAuditDetail(requested int, result domain.TrashPurgeResult) string {
	return "条目数=" + strconv.Itoa(requested) +
		" 已删除=" + strconv.Itoa(result.Purged) +
		" 释放字节=" + strconv.FormatInt(result.FreedBytes, 10)
}

// trashPurgeAllAuditDetail 生成清空审计详情：条目数与释放字节。
//
// 清空没有"请求条目数"——范围就是当时回收站里的全部，因此直接用 PurgeAll 回报的已删除
// 条数作为条目数，不再补一个恒等于它的字段。
func trashPurgeAllAuditDetail(result domain.TrashPurgeResult) string {
	return "条目数=" + strconv.Itoa(result.Purged) +
		" 释放字节=" + strconv.FormatInt(result.FreedBytes, 10)
}
