// 备份包导入的 HTTP 边界（FR-137）。
//
// 本文件同时承载「触发导入」与「读取导入记录」两组端点。三条导入通道（CLI / URL 拉取 /
// 分片上传）最终都归到同一个「已落地的归档文件路径」再进入 RestoreService.Stage，
// 因此异步状态与错误语义在这里统一表达：
//
//	202 受理 → 轮询记录看进度 → pending_restart 表示已写 restore.pending、需重启生效。
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// ImportBackupFromURL 从 URL 拉取备份包并启动导入（仅管理员）。
//
// 立即返回 202 与一条 queued 记录；拉取、校验、合并 blob 与暂存都在后台推进，
// 调用方轮询 /backups/imports/{id} 观察进度。即使一切顺利，也需重启服务才会
// 真正替换数据库（见 ADR-0027 决策 5）。
func (h *Handlers) ImportBackupFromURL(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.backupImports == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "导入服务未启用")
		return
	}
	var req CreateBackupImportRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.backupImports.StartURLImport(c.Request.Context(), domain.CreateImportOptions{
		SourceURL:      req.SourceUrl,
		Operator:       principal.Username,
		Overwrite:      req.Overwrite != nil && *req.Overwrite,
		Deep:           req.Deep != nil && *req.Deep,
		ExpectedSHA256: derefString(req.ExpectedSha256),
	})
	if err != nil {
		writeBackupImportErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, toAPIBackupImport(rec))
}

// ListBackupImports 分页列出导入记录（最近优先，仅管理员）。
func (h *Handlers) ListBackupImports(c *gin.Context, params ListBackupImportsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupImports == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "导入服务未启用")
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	items, err := h.backupImports.List(limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	total, err := h.backupImports.Count()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	out := BackupImportList{Items: make([]BackupImport, 0, len(items)), Total: total}
	for _, item := range items {
		out.Items = append(out.Items, toAPIBackupImport(item))
	}
	c.JSON(http.StatusOK, out)
}

// GetBackupImport 读取单条导入记录（仅管理员）。
func (h *Handlers) GetBackupImport(c *gin.Context, id BackupImportIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupImports == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "导入服务未启用")
		return
	}
	rec, err := h.backupImports.Get(id)
	if err != nil {
		writeBackupImportErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIBackupImport(*rec))
}

// toAPIBackupImport 把行模型映射为契约类型；零值字段一律省略，
// 避免前端把「未知」渲染成 0 或空串。
func toAPIBackupImport(rec repository.BackupImport) BackupImport {
	out := BackupImport{
		ImportId:  rec.ImportID,
		Origin:    BackupImportOrigin(rec.Origin),
		Status:    BackupImportStatus(rec.Status),
		Operator:  rec.Operator,
		Overwrite: rec.Overwrite,
		Deep:      rec.Deep,
		CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt,
	}
	if rec.SourceURL != "" {
		out.SourceUrl = &rec.SourceURL
	}
	if rec.TotalBytes > 0 {
		out.TotalBytes = &rec.TotalBytes
	}
	if rec.FetchedBytes > 0 {
		out.FetchedBytes = &rec.FetchedBytes
	}
	if rec.BlobCount > 0 {
		out.BlobCount = &rec.BlobCount
	}
	if rec.PackageID != "" {
		out.PackageId = &rec.PackageID
	}
	if rec.ErrorCode != "" {
		out.ErrorCode = &rec.ErrorCode
	}
	if rec.ErrorSummary != "" {
		out.Error = &rec.ErrorSummary
	}
	if rec.FinishedAt != nil {
		out.FinishedAt = rec.FinishedAt
	}
	if rec.RestorePendingAt != nil {
		out.RestorePendingAt = rec.RestorePendingAt
	}
	return out
}

// writeBackupImportErr 把导入相关错误映射为明确状态码。
//
// target_not_empty 与 restore_pending 都是 409，但运维处置完全不同
// （前者加 overwrite 重试，后者必须先重启服务），因此用不同 error_code 区分。
func writeBackupImportErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrBackupImportInProgress):
		auth.WriteError(c, http.StatusConflict, "import_in_progress", err.Error())
	case errors.Is(err, domain.ErrRestorePending):
		auth.WriteError(c, http.StatusConflict, "restore_pending", err.Error())
	case errors.Is(err, domain.ErrRestoreTargetNotEmpty):
		auth.WriteError(c, http.StatusConflict, "target_not_empty", err.Error())
	case errors.Is(err, repository.ErrNotFound):
		auth.WriteError(c, http.StatusNotFound, "not_found", "导入记录不存在")
	default:
		writeDomainErr(c, err)
	}
}
