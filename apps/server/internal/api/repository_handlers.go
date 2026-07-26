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

// ListRepositories 仓库列表（分页）。管理员可见全部；普通用户仅见可读（public 或经 ACL 授权）者。
// 可选鉴权（FR-66）：匿名请求受全局开关约束（关则 401），开则返回匿名可读集合。
// 响应中每个仓库附带 artifactCount/totalSize 统计（GROUP BY 一次查出，避免 N+1）。
func (h *Handlers) ListRepositories(c *gin.Context, params ListRepositoriesParams) {
	p, authed := auth.PrincipalFrom(c)
	if !authed && !h.anonymousAllowed(c) {
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	// 排序参数（可选）
	sortBy := ""
	order := ""
	if s := c.Query("sort"); s != "" {
		sortBy = s
	}
	if o := c.Query("order"); o != "" {
		order = o
	}
	rows, statsMap, total, err := h.repos.ListWithStats(limit, offset, sortBy, order)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	isAdmin := authed && p.IsAdmin()
	var subjectID int64
	if authed {
		subjectID = p.UserID
	}
	items := make([]Repository, 0, len(rows))
	for i := range rows {
		if !isAdmin {
			allowed, err := h.repos.CanAccess(rows[i].Name, subjectID, "read")
			if err != nil || !allowed {
				continue
			}
		}
		stats := statsMap[rows[i].ID]
		items = append(items, toAPIRepository(&rows[i], &stats))
	}
	if !isAdmin {
		total = len(items)
	}
	c.JSON(http.StatusOK, RepositoryList{Items: items, Total: total})
}

// CreateRepository 创建仓库，仅管理员。
func (h *Handlers) CreateRepository(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req CreateRepositoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Name == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仓库名称不能为空")
		return
	}
	visibility := ""
	if req.Visibility != nil {
		visibility = string(*req.Visibility)
	}
	cfg := repository.RepositoryConfig{}
	if req.RemoteUrl != nil {
		cfg.RemoteURL = *req.RemoteUrl
	}
	if req.Members != nil {
		cfg.Members = *req.Members
	}
	repo, err := h.repos.Create(req.Name, string(req.Format), string(req.Type), visibility, cfg)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAPIRepository(repo, nil))
}

// UpdateRepository 更新仓库可见性，仅管理员。
func (h *Handlers) UpdateRepository(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req UpdateRepositoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Visibility == nil && req.RemoteUrl == nil && req.Members == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少可更新字段")
		return
	}
	visibility := ""
	if req.Visibility != nil {
		visibility = string(*req.Visibility)
	}
	var cfg *repository.RepositoryConfig
	if req.RemoteUrl != nil || req.Members != nil {
		cfg = &repository.RepositoryConfig{}
		if req.RemoteUrl != nil {
			cfg.RemoteURL = *req.RemoteUrl
		}
		if req.Members != nil {
			cfg.Members = *req.Members
		}
	}
	repo, err := h.repos.Update(name, visibility, cfg)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIRepository(repo, nil))
}

// DeleteRepository 删除仓库，仅管理员。
func (h *Handlers) DeleteRepository(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if err := h.repos.Delete(name); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ListRepositoryAssets 列出仓库制品（分页，可按路径前缀过滤）：需对该仓库有 read 权限。
func (h *Handlers) ListRepositoryAssets(c *gin.Context, name RepoNameParam, params ListRepositoryAssetsParams) {
	if _, ok := h.requireRepoRead(c, name); !ok {
		return
	}
	prefix := ""
	if params.Prefix != nil {
		prefix = *params.Prefix
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	rows, total, err := h.repos.ListAssets(name, prefix, limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]AssetSummary, 0, len(rows))
	for i := range rows {
		items = append(items, toAPIAsset(&rows[i]))
	}
	c.JSON(http.StatusOK, AssetList{Items: items, Total: total})
}

// GetRepositoryUsage 返回仓库客户端接入片段（据 format/type 与对外基址组装）：需 read 权限。
func (h *Handlers) GetRepositoryUsage(c *gin.Context, name RepoNameParam) {
	if _, ok := h.requireRepoRead(c, name); !ok {
		return
	}
	repo, snippets, err := h.repos.Usage(name, apiBaseURL(c))
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]UsageSnippet, 0, len(snippets))
	for _, s := range snippets {
		items = append(items, toAPIUsageSnippet(s))
	}
	c.JSON(http.StatusOK, UsageInfo{Format: repo.Format, Type: repo.Type, Snippets: items})
}

// GetRepositoryAcl 读取仓库 ACL：需全局管理员或对该仓库有 admin 授权。
func (h *Handlers) GetRepositoryAcl(c *gin.Context, name RepoNameParam) {
	if _, ok := h.requireRepoAdmin(c, name); !ok {
		return
	}
	rows, err := h.repos.GetAcl(name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, AclList{Items: aclEntries(rows)})
}

// SetRepositoryAcl 覆盖写入仓库 ACL：需全局管理员或对该仓库有 admin 授权。
func (h *Handlers) SetRepositoryAcl(c *gin.Context, name RepoNameParam) {
	if _, ok := h.requireRepoAdmin(c, name); !ok {
		return
	}
	var req PutAclRequest
	if !bindJSON(c, &req) {
		return
	}
	entries := make([]repository.Acl, 0, len(req.Items))
	for _, e := range req.Items {
		entries = append(entries, repository.Acl{SubjectID: e.SubjectId, Action: string(e.Action)})
	}
	rows, err := h.repos.SetAcl(name, entries)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, AclList{Items: aclEntries(rows)})
}

// requireRepoAdmin 要求主体对仓库有管理权：全局管理员或该仓库 admin ACL。
func (h *Handlers) requireRepoAdmin(c *gin.Context, name RepoNameParam) (*auth.Principal, bool) {
	p, ok := requirePrincipal(c)
	if !ok {
		return nil, false
	}
	if p.IsAdmin() {
		return p, true
	}
	allowed, err := h.repos.CanAccess(name, p.UserID, "admin")
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
			return nil, false
		}
		writeDomainErr(c, err)
		return nil, false
	}
	if !allowed {
		auth.WriteError(c, http.StatusForbidden, "forbidden", "无权管理该仓库 ACL")
		return nil, false
	}
	return p, true
}

// requireRepoRead 要求主体对仓库有读权限：全局管理员、public 仓库（含匿名）或该仓库 read/write/admin ACL。
// 采用可选鉴权：public 仓库允许匿名读；私有仓库匿名 401、已认证但越权 403，与协议层放行策略一致。
func (h *Handlers) requireRepoRead(c *gin.Context, name RepoNameParam) (*auth.Principal, bool) {
	p, authed := auth.PrincipalFrom(c)
	if authed && p.IsAdmin() {
		return p, true
	}
	var subjectID int64
	if authed {
		subjectID = p.UserID
	}
	allowed, err := h.repos.CanAccess(name, subjectID, "read")
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
			return nil, false
		}
		writeDomainErr(c, err)
		return nil, false
	}
	if !allowed {
		if !authed {
			auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
		} else {
			auth.WriteError(c, http.StatusForbidden, "forbidden", "无权访问该仓库")
		}
		return nil, false
	}
	return p, true
}

// apiBaseURL 依请求推断对外基址（scheme + host），供使用片段拼接客户端地址。
// 反代部署经 X-Forwarded-Proto 修正 scheme；Host 直接取请求头。
func apiBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + c.Request.Host
}

// CleanupEmptyMavenArtifacts 清理 Maven 仓库中无 jar 的 GAV 目录。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) CleanupEmptyMavenArtifacts(c *gin.Context) {
	name := c.Param("name")
	if _, ok := h.requireRepoAdmin(c, name); !ok {
		return
	}
	deleted, err := h.repos.CleanupEmptyMavenArtifacts(name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// ListPublicRepositories 匿名可读仓库列表（无需认证）：public ∪ anonymous 主体
// 被授 read 的仓库（FR-66）。全局开关关闭时 401。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) ListPublicRepositories(c *gin.Context) {
	if !h.anonymousAllowed(c) {
		return
	}
	limit, offset := pageOffset(nil, nil)
	rows, statsMap, _, err := h.repos.ListWithStats(limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]Repository, 0, len(rows))
	for i := range rows {
		allowed, err := h.repos.CanAccess(rows[i].Name, 0, "read")
		if err != nil || !allowed {
			continue
		}
		stats := statsMap[rows[i].ID]
		items = append(items, toAPIRepository(&rows[i], &stats))
	}
	c.JSON(http.StatusOK, RepositoryList{Items: items, Total: len(items)})
}

// anonymousAllowed 校验匿名访问全局开关；关闭则写 401 并返回 false（FR-66）。
func (h *Handlers) anonymousAllowed(c *gin.Context) bool {
	enabled, err := h.settings.AnonymousAccessEnabled()
	if err != nil {
		writeDomainErr(c, err)
		return false
	}
	if !enabled {
		auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "匿名访问已关闭")
		return false
	}
	return true
}

// ListRepositoryTree 按目录懒加载：返回仓库指定前缀下当前层的目录和文件。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) ListRepositoryTree(c *gin.Context) {
	name := c.Param("name")
	if _, ok := h.requireRepoRead(c, name); !ok {
		return
	}
	prefix := c.Query("prefix")
	entry, err := h.repos.ListDirectory(name, prefix)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	type fileItem struct {
		Path        string `json:"path"`
		Size        int64  `json:"size"`
		Hash        string `json:"hash"`
		ContentType string `json:"contentType,omitempty"`
		UpdatedAt   string `json:"updatedAt"`
	}
	files := make([]fileItem, 0, len(entry.Files))
	for _, f := range entry.Files {
		files = append(files, fileItem{
			Path:        f.Path,
			Size:        f.Size,
			Hash:        f.BlobHash,
			ContentType: f.ContentType,
			UpdatedAt:   f.UpdatedAt,
		})
	}
	dirs := entry.Dirs
	if dirs == nil {
		dirs = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"directories": dirs, "files": files})
}

// SearchAssets 全局跨仓库制品搜索。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) SearchAssets(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "搜索关键词不能为空")
		return
	}
	page := 1
	pageSize := 20
	if v := c.Query("page"); v != "" {
		if n, err := parseIntQuery(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := c.Query("page_size"); v != "" {
		if n, err := parseIntQuery(v); err == nil && n > 0 && n <= 100 {
			pageSize = n
		}
	}
	limit := pageSize
	offset := (page - 1) * pageSize

	// 解析主体（可能匿名）；匿名受全局开关约束（FR-66）
	p, authed := auth.PrincipalFrom(c)
	if !authed && !h.anonymousAllowed(c) {
		return
	}
	var subjectID int64
	var isAdmin bool
	if authed {
		subjectID = p.UserID
		isAdmin = p.IsAdmin()
	}

	// 仓库范围过滤
	repoFilter := c.Query("repository")
	if repoFilter != "" {
		// 单仓库内搜索：检查读权限后复用 ListAssets
		allowed, err := h.repos.CanAccess(repoFilter, subjectID, "read")
		if err != nil {
			writeDomainErr(c, err)
			return
		}
		if !allowed && !isAdmin {
			if !authed {
				auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证")
			} else {
				auth.WriteError(c, http.StatusForbidden, "forbidden", "无权访问")
			}
			return
		}
	}

	results, total, err := h.repos.SearchAssets(q, subjectID, isAdmin, limit, offset)
	if err != nil {
		writeDomainErr(c, err)
		return
	}

	type searchItem struct {
		Repository string `json:"repository"`
		Path       string `json:"path"`
		Size       int64  `json:"size"`
		Hash       string `json:"hash"`
		UpdatedAt  string `json:"updatedAt"`
	}
	items := make([]searchItem, 0, len(results))
	for _, r := range results {
		// 如果指定了 repository 过滤，只返回匹配仓库的
		if repoFilter != "" && r.RepoName != repoFilter {
			continue
		}
		items = append(items, searchItem{
			Repository: r.RepoName,
			Path:       r.Asset.Path,
			Size:       r.Asset.Size,
			Hash:       r.Asset.BlobHash,
			UpdatedAt:  r.Asset.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// parseIntQuery 尝试将字符串解析为 int。
func parseIntQuery(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// aclEntries 把行模型批量转为契约 AclEntry。
func aclEntries(rows []repository.Acl) []AclEntry {
	items := make([]AclEntry, 0, len(rows))
	for _, a := range rows {
		items = append(items, toAPIAcl(a))
	}
	return items
}
