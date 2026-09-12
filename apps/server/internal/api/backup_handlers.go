package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 备份下载链接有效期约束。
const (
	backupLinkDefaultTTL = 30 * time.Minute
	backupLinkMinTTL     = time.Minute
	backupLinkMaxTTL     = 24 * time.Hour
)

// ListBackups 返回备份包列表（仅管理员）。
func (h *Handlers) ListBackups(c *gin.Context, params ListBackupsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	items, err := h.backups.List(limit, offset)
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	total, err := h.backups.Count()
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	out := BackupPackageList{Items: make([]BackupPackage, 0, len(items)), Total: total}
	for _, item := range items {
		out.Items = append(out.Items, toAPIBackupPackage(item))
	}
	c.JSON(http.StatusOK, out)
}

// CreateBackup 登记并启动一次备份生成（仅管理员）。
// 生成耗时与包体积同阶，接口立即返回 queued 记录，由前端轮询进度。
func (h *Handlers) CreateBackup(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	var req CreateBackupRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.backups.Create(c.Request.Context(), domain.CreateBackupOptions{
		Mode:  archive.Mode(req.Mode),
		Label: derefString(req.Label),
	})
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAPIBackupPackage(rec))
}

// GetBackup 返回单个备份包详情（仅管理员）。
func (h *Handlers) GetBackup(c *gin.Context, id BackupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	rec, err := h.backups.Get(id)
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIBackupPackage(*rec))
}

// DeleteBackup 删除备份包（仅管理员）。被增量包引用的基线会被拒绝。
func (h *Handlers) DeleteBackup(c *gin.Context, id BackupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	if err := h.backups.Delete(id); err != nil {
		writeBackupErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// CreateBackupLink 生成带时效的下载链接（仅管理员）。
func (h *Handlers) CreateBackupLink(c *gin.Context, id BackupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	if _, err := h.backups.Get(id); err != nil {
		writeBackupErr(c, err)
		return
	}

	ttl := backupLinkDefaultTTL
	var req CreateBackupLinkRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		if req.TtlSeconds != nil {
			ttl = time.Duration(*req.TtlSeconds) * time.Second
		}
	}
	if ttl < backupLinkMinTTL {
		ttl = backupLinkMinTTL
	}
	if ttl > backupLinkMaxTTL {
		ttl = backupLinkMaxTTL
	}

	exp := time.Now().Add(ttl)
	c.JSON(http.StatusOK, BackupLink{
		Url:       h.backupDownloadURL(c, id, exp),
		ExpiresAt: exp.UTC().Format(time.RFC3339),
	})
}

// VerifyBackup 校验备份包完整性（仅管理员）。
// deep=true 会逐 blob 比对摘要，耗时与包体积同阶。
//
// 分成两类失败：包本身不可用（不存在 / 未完成 / 文件缺失）走通用错误映射；
// 包在但内容校验未通过返回 422 + BackupVerification（契约约定的形状），
// 让调用方能拿到"校验不通过"这一业务结论而不是一个泛化错误。
func (h *Handlers) VerifyBackup(c *gin.Context, id BackupIdParam, params VerifyBackupParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	deep := params.Deep != nil && *params.Deep
	manifest, err := h.backups.Verify(id, deep)
	if err != nil {
		if isBackupUnavailable(err) {
			writeBackupErr(c, err)
			return
		}
		reason := err.Error()
		c.JSON(http.StatusUnprocessableEntity, BackupVerification{
			Ok:        false,
			PackageId: &id,
			Deep:      &deep,
			Error:     &reason,
		})
		return
	}
	blobs := manifest.Blobs.Count
	assets := manifest.Counts.Assets
	c.JSON(http.StatusOK, BackupVerification{
		Ok:        true,
		PackageId: &id,
		Deep:      &deep,
		Blobs:     &blobs,
		Assets:    &assets,
	})
}

// isBackupUnavailable 判断错误是否属于"包不可用"而非"包内容校验未通过"。
func isBackupUnavailable(err error) bool {
	return errors.Is(err, domain.ErrBackupNotFound) ||
		errors.Is(err, domain.ErrBackupFileMissing) ||
		errors.Is(err, domain.ErrBackupIncomplete)
}

// DownloadBackup 流式下载备份包归档。
//
// 鉴权两条路径：带 token/exp 的签名链接（供新机器直接拉取，无需会话），
// 或已认证的管理员会话。前者才是搬迁主路径——普通 <a href> 带不上 Bearer 头。
func (h *Handlers) DownloadBackup(c *gin.Context, id BackupIdParam, params DownloadBackupParams) {
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	if !h.authorizeBackupDownload(c, id, params) {
		return
	}

	// Get 确保记录存在且已完成，避免对生成中的包发起下载。
	if _, err := h.backups.Get(id); err != nil {
		writeBackupErr(c, err)
		return
	}
	path := h.backups.PackagePath(id)
	// c.File 走 http.ServeFile：支持 Range 断点续传与 If-Modified-Since，
	// 对 GB 级包是必要能力。
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".tar.gz"))
	c.Header("Content-Type", "application/gzip")
	c.Header("Cache-Control", "no-store")
	c.File(path)
}

func (h *Handlers) authorizeBackupDownload(c *gin.Context, id BackupIdParam, params DownloadBackupParams) bool {
	if params.Token != nil && *params.Token != "" {
		exp, err := strconv.ParseInt(derefString(params.Exp), 10, 64)
		if err != nil {
			auth.WriteError(c, http.StatusUnauthorized, "invalid_link", domain.ErrBackupLinkInvalid.Error())
			return false
		}
		if err := h.backupLinks.Verify(id, *params.Token, time.Unix(exp, 0), time.Now()); err != nil {
			auth.WriteError(c, http.StatusUnauthorized, "invalid_link", err.Error())
			return false
		}
		return true
	}
	if _, ok := requireAdmin(c); !ok {
		return false
	}
	return true
}

// backupDownloadURL 拼出可被外部机器直接使用的绝对地址。
// 优先用对外基础 URL（CDN 域名），缺失时回退到当前请求的 scheme+host。
func (h *Handlers) backupDownloadURL(c *gin.Context, packageID string, exp time.Time) string {
	base := strings.TrimRight(h.publicURL, "/")
	if base == "" {
		scheme := "http"
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + c.Request.Host
	}
	return fmt.Sprintf("%s/api/v1/backups/%s/download?exp=%d&token=%s",
		base, url.PathEscape(packageID), exp.Unix(), h.backupLinks.Sign(packageID, exp))
}

// toAPIBackupPackage 把行模型转为契约类型。
func toAPIBackupPackage(p repository.BackupPackage) BackupPackage {
	out := BackupPackage{
		PackageId: p.PackageID,
		Mode:      BackupPackageMode(p.Mode),
		Status:    BackupPackageStatus(p.Status),
		SizeBytes: p.SizeBytes,
		CreatedAt: p.CreatedAt,
	}
	if p.BasePackageID != "" {
		out.BasePackageId = &p.BasePackageID
	}
	if p.Label != "" {
		out.Label = &p.Label
	}
	if p.NodeID != "" {
		out.NodeId = &p.NodeID
	}
	if p.AppVersion != "" {
		out.AppVersion = &p.AppVersion
	}
	if p.DBSchemaVersion > 0 {
		out.DbSchemaVersion = &p.DBSchemaVersion
	}
	if p.FinishedAt != nil {
		out.FinishedAt = p.FinishedAt
	}
	if p.ErrorSummary != "" {
		out.ErrorSummary = &p.ErrorSummary
	}
	out.Counts = parseBackupCounts(p.CountsJSON)
	return out
}

// parseBackupCounts 把库内的计数 JSON 转为契约类型；空对象返回 nil 以便省略字段。
func parseBackupCounts(countsJSON string) *BackupCounts {
	trimmed := strings.TrimSpace(countsJSON)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	var raw archive.Counts
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil
	}
	return &BackupCounts{
		Users:          &raw.Users,
		Tokens:         &raw.Tokens,
		Repositories:   &raw.Repositories,
		Acls:           &raw.ACLs,
		Assets:         &raw.Assets,
		FormatMetadata: &raw.FormatMetadata,
	}
}

// writeBackupErr 把备份领域错误映射为明确的状态码。
func writeBackupErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrBackupNotFound):
		auth.WriteError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrBackupFileMissing):
		auth.WriteError(c, http.StatusNotFound, "backup_file_missing", err.Error())
	case errors.Is(err, domain.ErrBackupInProgress):
		auth.WriteError(c, http.StatusConflict, "backup_in_progress", err.Error())
	case errors.Is(err, domain.ErrBackupHasDerived):
		auth.WriteError(c, http.StatusConflict, "backup_has_derived", err.Error())
	case errors.Is(err, domain.ErrBackupIncomplete):
		auth.WriteError(c, http.StatusConflict, "backup_incomplete", err.Error())
	default:
		writeDomainErr(c, err)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
