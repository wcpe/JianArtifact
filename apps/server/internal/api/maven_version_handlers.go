package api

// 本文件实现 Maven 版本级清理与资产回收站的 HTTP 入口。
//
// 三条必须守住的口径：
//
//  1. **预览与删除是两个独立请求**，删除请求必须带上用户在预览中确认过的坐标。
//     服务端不缓存预览结果（缓存会在并发删改后变成"删的不是刚才看见的"），
//     而是让删除端重新计算计划并做子集校验（见 domain.DeleteMavenVersion）。
//
//  2. **回收站恢复与彻底清理都逐条报告**，一条冲突不影响其余条目：
//     批量操作里把"3 条冲突"折叠成"整批失败"会让用户无法判断到底恢复了哪些。
//
//  3. **审计动作名按动作区分**，不统一记成一个笼统名字：
//     恢复是可逆的正向操作、彻底清理不可逆，两者混在一个动作名下，
//     事后读审计只能靠 detail 猜语义。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 审计动作名。
const (
	auditActionMavenVersionTrash = "asset.maven_version.trash"
	auditActionAssetTrashRestore = "asset.trash.restore"
	auditActionAssetTrashPurge   = "asset.trash.purge"
)

// PlanMavenVersionCleanup 预览 Maven 版本的清理计划（只读）。
func (h *Handlers) PlanMavenVersionCleanup(c *gin.Context, name RepoNameParam, params PlanMavenVersionCleanupParams) {
	if !requireAssetOperationAdmin(c, "") {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "制品存储未就绪")
		return
	}
	coord := domain.MavenVersionCoordinate{
		GroupID:    strings.TrimSpace(params.GroupId),
		ArtifactID: strings.TrimSpace(params.ArtifactId),
		Version:    strings.TrimSpace(params.Version),
	}
	if params.BuildId != nil {
		coord.BuildID = strings.TrimSpace(*params.BuildId)
	}
	plan, err := h.assets.PlanMavenVersion(string(name), coord)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, plan)
}

// DiagnoseMavenCoordinate 诊断同一 Maven 坐标的跨仓重复（只读）。
func (h *Handlers) DiagnoseMavenCoordinate(c *gin.Context, name RepoNameParam, params DiagnoseMavenCoordinateParams) {
	if !requireAssetOperationAdmin(c, "") {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "制品存储未就绪")
		return
	}
	_ = name // 诊断范围是全部 Maven hosted 仓库，不局限于路径里的这个仓库
	report, err := h.assets.DiagnoseMavenCoordinate(domain.MavenVersionCoordinate{
		GroupID:    strings.TrimSpace(params.GroupId),
		ArtifactID: strings.TrimSpace(params.ArtifactId),
		Version:    strings.TrimSpace(params.Version),
	})
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, report)
}

// TrashMavenVersion 把 Maven 版本移入资产回收站（软删除，可恢复）。
func (h *Handlers) TrashMavenVersion(c *gin.Context, name RepoNameParam) {
	operationID := domain.NewOperationID()
	if !requireAssetOperationAdmin(c, operationID) {
		return
	}
	if h.assets == nil {
		writeAssetOperationError(c, http.StatusConflict, "conflict", "制品存储未就绪", operationID)
		return
	}
	var req MavenVersionTrashRequest
	if !bindAssetOperationJSON(c, &req, operationID) {
		return
	}
	reason := strings.TrimSpace(req.OverrideReason)
	if reason == "" || len([]rune(reason)) > 512 {
		writeAssetOperationError(c, http.StatusBadRequest, "bad_request",
			"overrideReason 必填且长度不得超过 512", operationID)
		return
	}
	coord := domain.MavenVersionCoordinate{
		GroupID:    strings.TrimSpace(req.GroupId),
		ArtifactID: strings.TrimSpace(req.ArtifactId),
		Version:    strings.TrimSpace(req.Version),
	}
	if req.BuildId != nil {
		coord.BuildID = strings.TrimSpace(*req.BuildId)
	}
	var paths []string
	if req.Paths != nil {
		paths = *req.Paths
	}
	actor := ""
	if p, ok := auth.PrincipalFrom(c); ok {
		actor = p.Username
	}

	result, err := h.assets.DeleteMavenVersion(string(name), coord, paths, actor)
	if err != nil {
		// 已完成的批次仍会返回结果：审计必须如实记录"实际删了多少"，
		// 而不是因为整体报错就当成什么都没发生。
		if result != nil && result.Trashed > 0 {
			h.AuditLog(c, auditActionMavenVersionTrash, "asset",
				string(name)+"/"+coord.Dir(), string(name),
				mavenTrashAuditDetail(reason, result), "partial")
		}
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, auditActionMavenVersionTrash, "asset",
		string(name)+"/"+coord.Dir(), string(name),
		mavenTrashAuditDetail(reason, result), "ok")
	c.JSON(http.StatusOK, result)
}

// mavenTrashAuditDetail 生成删除审计详情：原因 + 坐标 + 实际影响量。
func mavenTrashAuditDetail(reason string, result *domain.MavenVersionDeleteResult) string {
	if result == nil {
		return reason
	}
	return reason + "；文件 " + strconv.Itoa(result.Trashed) +
		"，批次 " + strconv.Itoa(result.Batches) +
		"，字节 " + strconv.FormatInt(result.Bytes, 10)
}

// ListAssetTrash 分页列出资产回收站条目。
func (h *Handlers) ListAssetTrash(c *gin.Context, params ListAssetTrashParams) {
	if !requireAssetOperationAdmin(c, "") {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "制品存储未就绪")
		return
	}
	page := 1
	if params.Page != nil && *params.Page > 0 {
		page = *params.Page
	}
	size := 20
	if params.PageSize != nil && *params.PageSize > 0 && *params.PageSize <= 100 {
		size = *params.PageSize
	}
	entries, total, stats, err := h.assets.ListAssetTrash(size, (page-1)*size)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items": entries, "total": total, "page": page, "pageSize": size,
		"stats": gin.H{"entries": stats.Entries, "bytes": stats.Bytes},
	})
}

// RestoreAssetTrash 从资产回收站恢复指定条目。
func (h *Handlers) RestoreAssetTrash(c *gin.Context) {
	if !requireAssetOperationAdmin(c, "") {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "制品存储未就绪")
		return
	}
	var req AssetTrashIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Ids) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "ids 必填且不能为空")
		return
	}
	result, err := h.assets.RestoreAssetTrash(req.Ids)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 冲突与缺失都如实记入审计：它们是"用户以为恢复了、其实没有"的唯一线索。
	h.AuditLog(c, auditActionAssetTrashRestore, "maintenance", "asset-trash", "",
		trashResolveAuditDetail("恢复", len(req.Ids), result.Restored,
			len(result.Conflicts), len(result.Missing)), "ok")
	c.JSON(http.StatusOK, gin.H{
		"restored":  result.Restored,
		"conflicts": result.Conflicts,
		"missing":   result.Missing,
	})
}

// PurgeAssetTrash 彻底清理资产回收站条目（不可逆）。
func (h *Handlers) PurgeAssetTrash(c *gin.Context) {
	if !requireAssetOperationAdmin(c, "") {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "制品存储未就绪")
		return
	}
	var req AssetTrashIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Ids) == 0 {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "ids 必填且不能为空")
		return
	}
	result, err := h.assets.PurgeAssetTrash(req.Ids)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, auditActionAssetTrashPurge, "maintenance", "asset-trash", "",
		trashResolveAuditDetail("清理", len(req.Ids), result.Purged, 0, 0), "ok")
	c.JSON(http.StatusOK, gin.H{
		"purged":     result.Purged,
		"freedBytes": result.FreedBytes,
	})
}

// trashResolveAuditDetail 生成回收站解析动作的审计详情。
func trashResolveAuditDetail(verb string, requested, done, conflicts, missing int) string {
	detail := verb + "：请求 " + strconv.Itoa(requested) + " 条，完成 " + strconv.Itoa(done) + " 条"
	if conflicts > 0 {
		detail += "，冲突跳过 " + strconv.Itoa(conflicts) + " 条"
	}
	if missing > 0 {
		detail += "，无法恢复 " + strconv.Itoa(missing) + " 条"
	}
	return detail
}
