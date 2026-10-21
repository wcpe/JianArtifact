// 未管理备份包的 HTTP 边界。
//
// 两个端点：列出"磁盘有包体、登记无记录"的包，以及把选中的包导入管理列表。
// 权限沿用备份域的统一口径：仅管理员。
package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 本文件同时承载「未管理包导入」与「包内容预览」两组端点：前者解决"文件在但看不见"，
// 后者解决"看得见但看不清里面有什么"，都是恢复前决策所必需的信息。

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
			Duplicate:  it.Duplicate,
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

// PreviewBackup 查看备份包内容概要（仅管理员，）。
//
// 为什么需要：恢复是不可撤销的操作，而此前列表只给出抽象计数（"多少制品、多少仓库"），
// 用户无法判断这个包是不是自己要的那个。本端点让"看清里面有什么"成为可能。
//
// 只读承诺由 domain.BackupService.Preview 保证：解到临时目录、以 mode=ro 查询、
// 查完即删；不改包体、不写备份目录。因此这里不需要额外的写冻结保护。
func (h *Handlers) PreviewBackup(c *gin.Context, id BackupIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}
	pv, err := h.backups.Preview(string(id))
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIBackupPreview(pv))
}

// toAPIBackupPreview 把领域层的预览结果映射为契约类型。
func toAPIBackupPreview(pv domain.BackupPreview) BackupPreview {
	repos := make([]struct {
		Format string `json:"format"`
		Name   string `json:"name"`
		Type   string `json:"type"`
	}, 0, len(pv.Repositories))
	for _, r := range pv.Repositories {
		repos = append(repos, struct {
			Format string `json:"format"`
			Name   string `json:"name"`
			Type   string `json:"type"`
		}{Format: r.Format, Name: r.Name, Type: r.Type})
	}
	users := make([]struct {
		Role     string `json:"role"`
		Status   string `json:"status"`
		Username string `json:"username"`
	}, 0, len(pv.Users))
	for _, u := range pv.Users {
		users = append(users, struct {
			Role     string `json:"role"`
			Status   string `json:"status"`
			Username string `json:"username"`
		}{Role: u.Role, Status: u.Status, Username: u.Username})
	}
	samples := make([]string, 0, len(pv.SampleAssets))
	samples = append(samples, pv.SampleAssets...)

	out := BackupPreview{
		Repositories: repos,
		Users:        users,
		SampleAssets: samples,
		TotalAssets:  pv.TotalAssets,
		BlobBytes:    pv.BlobBytes,
		Manifest: struct {
			AppVersion      string  `json:"appVersion"`
			BasePackageId   *string `json:"basePackageId,omitempty"`
			CreatedAt       *string `json:"createdAt,omitempty"`
			DbSchemaVersion int     `json:"dbSchemaVersion"`
			Mode            string  `json:"mode"`
			PackageId       string  `json:"packageId"`
		}{
			AppVersion:      pv.Manifest.AppVersion,
			DbSchemaVersion: pv.Manifest.DBSchemaVersion,
			Mode:            string(pv.Manifest.Mode),
			PackageId:       pv.Manifest.PackageID,
		},
	}
	if pv.Manifest.BasePackageID != "" {
		base := pv.Manifest.BasePackageID
		out.Manifest.BasePackageId = &base
	}
	if !pv.Manifest.CreatedAt.IsZero() {
		created := pv.Manifest.CreatedAt.UTC().Format(time.RFC3339)
		out.Manifest.CreatedAt = &created
	}
	return out
}

// GetBackupDiff 对比包内容与当前系统（仅管理员，）。
//
// 两种模式共用一个端点，用 scope 区分：
//   - summary（默认）：只返回四类计数（聚合查询，代价小）；
//   - 某实体：返回该分类的分页明细（单页上限 200）。
//
// 这样界面可以先拿到"有多少差异"，用户点开某一类才拉明细——
// 几十万制品时，一次拉全量既慢又占内存。
func (h *Handlers) GetBackupDiff(c *gin.Context, id BackupIdParam, params GetBackupDiffParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.backups == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "unavailable", "备份服务未启用")
		return
	}

	scope := "summary"
	if params.Scope != nil {
		scope = string(*params.Scope)
	}
	if scope == "summary" || scope == "" {
		sum, err := h.backups.DiffSummary(string(id))
		if err != nil {
			writeBackupErr(c, err)
			return
		}
		repos := toAPIDiffCounts(sum.Repositories)
		assets := toAPIDiffCounts(sum.Assets)
		users := toAPIDiffCounts(sum.Users)
		c.JSON(http.StatusOK, BackupDiff{
			PackageId:    sum.PackageID,
			MetadataOnly: sum.MetadataOnly,
			Repositories: &repos,
			Assets:       &assets,
			Users:        &users,
		})
		return
	}

	// 明细模式：kind 必填。
	if params.Kind == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "查看明细时必须指定 kind")
		return
	}
	page := 1
	if params.Page != nil {
		page = *params.Page
	}
	pageSize := 0
	if params.PageSize != nil {
		pageSize = *params.PageSize
	}
	detail, err := h.backups.DiffDetail(string(id),
		domain.DiffEntity(scope), domain.DiffKind(string(*params.Kind)), page, pageSize)
	if err != nil {
		writeBackupErr(c, err)
		return
	}
	items := make([]BackupDiffDetailItem, 0, len(detail.Items))
	for _, it := range detail.Items {
		items = append(items, BackupDiffDetailItem{Key: it.Key, Detail: it.Detail})
	}
	detailPage := BackupDiffDetailPage{
		Items:     items,
		Total:     detail.Total,
		Truncated: detail.Truncated,
	}
	c.JSON(http.StatusOK, BackupDiff{
		PackageId:    string(id),
		MetadataOnly: true,
		Detail:       &detailPage,
	})
}

// toAPIDiffCounts 把领域层计数映射为契约类型。
func toAPIDiffCounts(c domain.DiffCounts) DiffCounts {
	return DiffCounts{
		OnlyInPackage: c.OnlyInPackage,
		OnlyLocal:     c.OnlyLocal,
		Changed:       c.Changed,
		Same:          c.Same,
	}
}
