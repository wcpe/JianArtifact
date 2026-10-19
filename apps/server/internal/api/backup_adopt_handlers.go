// 未管理备份包的 HTTP 边界（FR-40 追加八）。
//
// 两个端点：列出"磁盘有包体、登记无记录"的包，以及把选中的包导入管理列表。
// 权限沿用备份域的统一口径：仅管理员。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// ListUnmanagedBackups 列出未管理的备份包（仅管理员）。
//
// 为什么要单独一个端点而不是复用列表接口的过滤参数：列表接口读的是登记表，
// 而这里要回答的恰恰是"登记表里没有的那些"。语义完全不同，混在一个端点上会让
// 分页、排序、权限校验全部需要分叉。
func (h *Handlers) ListUnmanagedBackups(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupAdopt == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	items, err := h.backupAdopt.Scan()
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]UnmanagedBackup, 0, len(items))
	for _, it := range items {
		out = append(out, UnmanagedBackup{
			FileId:     it.FileID,
			PackageId:  it.PackageID,
			SizeBytes:  it.SizeBytes,
			ModifiedAt: it.ModifiedAt,
			HasSidecar: it.HasSidecar,
			Valid:      it.Valid,
			Reason:     optionalString(it.Reason),
			Renamed:    it.Renamed,
		})
	}
	c.JSON(http.StatusOK, UnmanagedBackupList{Items: out, Total: len(out)})
}

// AdoptBackups 把选中的未管理包导入管理列表（仅管理员）。
//
// 返回 200 而不是 202：整个导入是同步的（只读 manifest + 写一行登记），
// 没有需要轮询的异步阶段；逐项结果就在响应体里。
func (h *Handlers) AdoptBackups(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupAdopt == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	var req AdoptBackupsRequest
	if !bindJSON(c, &req) {
		return
	}
	if len(req.FileIds) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "validation_error", "请至少选择一个备份包")
		return
	}

	result, err := h.backupAdopt.Adopt(req.FileIds)
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	items := make([]AdoptedBackup, 0, len(result.Items))
	for _, it := range result.Items {
		items = append(items, AdoptedBackup{
			FileId:    it.FileID,
			PackageId: it.PackageID,
			Adopted:   it.Adopted,
			Skipped:   it.Skipped,
			Reason:    optionalString(it.Reason),
		})
	}
	c.JSON(http.StatusOK, AdoptBackupsResult{
		Items:   items,
		Adopted: result.Adopted,
		Skipped: result.Skipped,
		Failed:  result.Failed,
	})
}

// 编译期断言：确保 domain 的服务类型在此文件被引用（防止未来重构时静默丢字段）。
var _ = (*domain.BackupAdoptService)(nil)
