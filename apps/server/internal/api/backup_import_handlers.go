// 备份包导入的 HTTP 边界。
//
// 本文件同时承载「触发导入」与「读取导入记录」两组端点。三条导入通道（CLI / URL 拉取 /
// 分片上传）最终都归到同一个「已落地的归档文件路径」再进入 RestoreService.Stage，
// 因此异步状态与错误语义在这里统一表达：
//
//	202 受理 → 轮询记录看进度 → pending_restart 表示已写 restore.pending、需重启生效。
package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// GetBackupRestoreStatus 返回是否存在待重启生效的备份恢复（仅管理员）。
//
// 存在的意义是让界面能解释"为什么现在导入不了"：一旦暂存成功，后续导入一律 409
// restore_pending，直到重启完成替换；而这个状态此前只体现在导入记录行的徽章上，
// 徽章被更近的事件挤出视野后就再也看不出原因。
//
// 失败模式的处理：标记读不了（属主/权限不对）时**不**让端点报错——这只是一条提示性查询，
// 而"读不了"恰恰是需要如实告知用户的情况（见 ApplyPendingRestore 的同类取舍）。
// 故仍返回 200 且 pending=false；真正的处置提示由启动告警与 CLI 导入后的属主检查给出。
func (h *Handlers) GetBackupRestoreStatus(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backupImports == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "导入服务未启用")
		return
	}
	pending, err := h.backupImports.PendingRestore()
	if err != nil {
		// 如实说明读不到，但不阻断界面：此时"没有待生效恢复"仍是可安全假设的默认。
		c.JSON(http.StatusOK, BackupRestoreStatus{Pending: false})
		return
	}
	if pending == nil {
		c.JSON(http.StatusOK, BackupRestoreStatus{Pending: false})
		return
	}
	origin := pending.Origin
	staged := pending.StagedAt
	packageID := pending.PackageID
	c.JSON(http.StatusOK, BackupRestoreStatus{
		Pending:   true,
		PackageId: &packageID,
		Origin:    &origin,
		StagedAt:  &staged,
	})
}

// RestoreFromLocalPackage 用本机已有的备份包恢复本机数据（仅管理员）。
//
// 为什么需要这个端点：另外三条恢复通道（URL 拉取 / 分片上传 / CLI）都要求包来自**外部**，
// 于是页面上明明列着可用的包，却没有任何按钮能用它恢复——用户会直接得出
// "只能备份、不能恢复"的结论（这正是实测收到的反馈）。补的是**入口**，不是新机制：
// 直接以本机包路径进入既有的 StartLocalImport（→ Stage → restore.pending），
// 与其它通道共享全部校验与错误语义。
//
// origin 记为 "local"：它与另三条在编排上完全相同，但在事件表里要能区分——
// "我用这台机器自己的包回滚"与"我从别处搬来一个包"是两件不同的事。
func (h *Handlers) RestoreFromLocalPackage(c *gin.Context, id BackupIdParam) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.backupImports == nil || h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "恢复服务未启用")
		return
	}
	// 请求体可整体省略（等同取默认），故用可选绑定；坏 JSON 仍要报 400。
	var req RestoreFromLocalPackageRequest
	if c.Request.ContentLength > 0 && !bindJSON(c, &req) {
		return
	}

	rec, err := h.backups.Get(id)
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	// 只有已完成的包才能作为恢复源：生成中的包内容不完整，失败的包不该被拿来覆盖现有数据。
	// 这条判据与演练（drill）保持一致，用户对两者"什么时候能用"的预期才一致。
	if rec.Status != repository.BackupStatusDone {
		writeBackupErr(c, fmt.Errorf("%w：%s 当前状态 %s", domain.ErrRestorePackageNotDone, id, rec.Status))
		return
	}

	imp, err := h.backupImports.StartLocalImport(c.Request.Context(), domain.LocalImportOptions{
		SourcePath: h.backups.PackagePath(string(id)),
		Origin:     "local",
		Operator:   principal.Username,
		Overwrite:  req.Overwrite != nil && *req.Overwrite,
		Deep:       req.Deep != nil && *req.Deep,
	})
	if err != nil {
		writeBackupImportErr(c, err)
		return
	}
	h.AuditLog(c, auditActionBackupRestore, "backup_package", string(id), "", "", "ok")
	c.JSON(http.StatusAccepted, toAPIBackupImport(imp))
}

// ImportBackupFromURL 从 URL 拉取备份包并启动导入（仅管理员）。//
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
