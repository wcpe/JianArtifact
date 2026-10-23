package api

// 回收站端点（FR-161）：列表 / 恢复 / 彻底删除 / 清空，均**仅管理员**、**非契约端点**。
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
	c.JSON(http.StatusOK, result)
}
