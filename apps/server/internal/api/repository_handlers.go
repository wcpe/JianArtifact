package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// resolveRepoNameSet 把请求里的仓库名解析为「主名 ∪ 全部别名」的去重有序集合，供下载
// 计量按仓库维度聚合（FR-142）。下载明细按请求路径里的名字落库，别名功能下同一仓库可经
// 多个名字访问（重命名后旧名仍在用、或直接用别名下载），故按主名过滤会少算记在别名下的
// 部分；查询侧统一按此集合聚合，即可合并同一逻辑仓库的全部下载。
//
// 名字经 RepositoryService.Get（内部 GetByName 支持别名解析）取到主名仓库并带出别名列表；
// 仓库不存在时返回其错误（调用方沿用既有 404 语义）。仓库服务未接线（测试/降级）时退化为
// 单名集合，保证不回退既有行为。返回集合已去重、按字典序排序稳定，避免同一次查询重复计数。
func (h *Handlers) resolveRepoNameSet(name string) ([]string, error) {
	if h.repos == nil {
		return []string{name}, nil
	}
	repo, err := h.repos.Get(name)
	if err != nil {
		return nil, err
	}
	set := make([]string, 0, len(repo.Aliases)+1)
	seen := make(map[string]bool, len(repo.Aliases)+1)
	for _, candidate := range append([]string{repo.Name}, repo.Aliases...) {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		set = append(set, candidate)
	}
	sort.Strings(set)
	return set, nil
}

const repositoryListFetchSize = 100

// listAllRepositories 分批读取全部仓库，供需要先做可见性过滤的列表端点使用。
// 每批固定 100 条，避免把分页上限误当成全量上限；各批次沿用同一排序参数，保证拼接顺序稳定。
func (h *Handlers) listAllRepositories(sortBy, order string) ([]repository.Repository, map[int64]domain.RepoStats, error) {
	all := make([]repository.Repository, 0, repositoryListFetchSize)
	stats := make(map[int64]domain.RepoStats)
	for offset := 0; ; offset += repositoryListFetchSize {
		rows, batchStats, _, err := h.repos.ListWithStats(repositoryListFetchSize, offset, sortBy, order)
		if err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			break
		}
		all = append(all, rows...)
		for id, value := range batchStats {
			stats[id] = value
		}
		if len(rows) < repositoryListFetchSize {
			break
		}
	}
	return all, stats, nil
}

// ListRepositories 仓库列表（分页）。管理员可见全部；普通用户仅见可读（public 或经 ACL 授权）者。
// 可选鉴权（FR-66）：匿名请求受全局开关约束（关则 401），开则返回匿名可读集合。
// 响应中每个仓库附带 artifactCount/totalSize 统计（GROUP BY 一次查出，避免 N+1）。
func (h *Handlers) ListRepositories(c *gin.Context, params ListRepositoriesParams) {
	p, authed := auth.PrincipalFrom(c)
	if !authed && !h.anonymousAllowed(c) {
		return
	}
	limit, offset := pageOffset(params.Page, params.PageSize)
	// 排序参数（可选）。
	sortBy := ""
	order := ""
	if s := c.Query("sort"); s != "" {
		sortBy = s
	}
	if o := c.Query("order"); o != "" {
		order = o
	}
	isAdmin := authed && p.IsAdmin()
	var subjectID int64
	if authed {
		subjectID = p.UserID
	}

	var rows []repository.Repository
	var statsMap map[int64]domain.RepoStats
	var total int
	var err error
	if isAdmin {
		rows, statsMap, total, err = h.repos.ListWithStats(limit, offset, sortBy, order)
	} else {
		var allRows []repository.Repository
		allRows, statsMap, err = h.listAllRepositories(sortBy, order)
		if err == nil {
			visible := make([]repository.Repository, 0, len(allRows))
			for i := range allRows {
				allowed, accessErr := h.repos.CanAccess(allRows[i].Name, subjectID, "read")
				if accessErr == nil && allowed {
					visible = append(visible, allRows[i])
				}
			}
			total = len(visible)
			if offset < len(visible) {
				end := offset + limit
				if end > len(visible) {
					end = len(visible)
				}
				rows = visible[offset:end]
			}
		}
	}
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]Repository, 0, len(rows))
	for i := range rows {
		stats := statsMap[rows[i].ID]
		item := toAPIRepository(&rows[i], &stats)
		// 连接状态为管理面信息（FR-114），仅管理员可见。
		if isAdmin {
			item.ConnectionStatus = h.connectionStatus(&rows[i])
		}
		items = append(items, item)
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
	description := ""
	if req.Description != nil {
		description = *req.Description
	}
	cfg := repository.RepositoryConfig{}
	if req.RemoteUrl != nil {
		cfg.RemoteURL = *req.RemoteUrl
	}
	if req.CredentialRef != nil {
		cfg.CredentialRef = *req.CredentialRef
	}
	if req.Members != nil {
		cfg.Members = *req.Members
	}
	if req.ImmutableRelease != nil {
		cfg.ImmutableRelease = *req.ImmutableRelease
	}
	var aliases []string
	if req.Aliases != nil {
		aliases = *req.Aliases
	}
	repo, err := h.repos.Create(req.Name, string(req.Format), string(req.Type), visibility, description, cfg, aliases...)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// FR-109：审计附带逻辑凭据引用名称（只记名称，绝不记录环境变量名或解析结果）。
	detail := "format=" + string(req.Format) + " type=" + string(req.Type)
	if cfg.CredentialRef != "" {
		detail += " credentialRef=" + cfg.CredentialRef
	}
	if len(repo.Aliases) > 0 {
		detail += " aliases=" + strings.Join(repo.Aliases, ",")
	}
	h.AuditLog(c, "repo.create", "repository", req.Name, req.Name, detail, "ok")
	c.JSON(http.StatusCreated, h.toRepo(repo, nil))
}

// UpdateRepository 更新仓库可见性/描述/配置，仅管理员。
func (h *Handlers) UpdateRepository(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req UpdateRepositoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Visibility == nil && req.Description == nil && req.RemoteUrl == nil && req.CredentialRef == nil && req.ImmutableRelease == nil && req.Members == nil && req.Aliases == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少可更新字段")
		return
	}
	visibility := ""
	if req.Visibility != nil {
		visibility = string(*req.Visibility)
	}
	var cfg *repository.RepositoryConfig
	if req.RemoteUrl != nil || req.CredentialRef != nil || req.ImmutableRelease != nil || req.Members != nil {
		existing, err := h.repos.Get(name)
		if err != nil {
			writeDomainErr(c, err)
			return
		}
		current, err := existing.DecodeConfig()
		if err != nil {
			auth.WriteError(c, http.StatusInternalServerError, "internal_error", "仓库配置解析失败")
			return
		}
		cfg = &current
		if req.RemoteUrl != nil {
			cfg.RemoteURL = *req.RemoteUrl
		}
		if req.CredentialRef != nil {
			cfg.CredentialRef = *req.CredentialRef
		}
		if req.Members != nil {
			cfg.Members = *req.Members
		}
		if req.ImmutableRelease != nil {
			cfg.ImmutableRelease = *req.ImmutableRelease
		}
	}
	repo, err := h.repos.Update(name, visibility, req.Description, cfg, req.Aliases)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// FR-109：审计附带逻辑凭据引用变更（配置或清除），只记名称不记明文。
	detailParts := make([]string, 0, 3)
	if req.ImmutableRelease != nil {
		detailParts = append(detailParts, "immutableRelease="+strconv.FormatBool(*req.ImmutableRelease))
	}
	if req.CredentialRef != nil {
		if *req.CredentialRef != "" {
			detailParts = append(detailParts, "credentialRef="+*req.CredentialRef)
		} else {
			detailParts = append(detailParts, "credentialRef=(cleared)")
		}
	}
	if req.Aliases != nil {
		detailParts = append(detailParts, "aliases="+strings.Join(*req.Aliases, ","))
	}
	detail := strings.Join(detailParts, " ")
	h.AuditLog(c, "repo.update", "repository", name, name, detail, "ok")
	c.JSON(http.StatusOK, h.toRepo(repo, nil))
}

// RenameRepository 重命名仓库，仅管理员。
// 新名称需非空且未被任何仓库主名或别名占用；重命名后旧名自动转为别名，
// 旧链接与既有客户端仍可经 GetByName 解析到该仓库。
func (h *Handlers) RenameRepository(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req RenameRepositoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.NewName) == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "新名称不能为空")
		return
	}
	repo, err := h.repos.Rename(name, req.NewName)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	h.AuditLog(c, "repo.rename", "repository", repo.Name, repo.Name, "oldName="+name+" newName="+repo.Name, "ok")
	c.JSON(http.StatusOK, h.toRepo(repo, nil))
}

// SetRepositoryOnline 设置仓库 online/offline 状态，仅管理员（FR-113）。
// online 为节点本地运维状态：不写复制变更日志（M-2），对端复制不会覆盖。
func (h *Handlers) SetRepositoryOnline(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req SetRepositoryOnlineRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Online == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少 online 字段")
		return
	}
	online := *req.Online
	if err := h.repos.SetOnline(name, online); err != nil {
		writeDomainErr(c, err)
		return
	}
	repo, err := h.repos.Get(name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	state := "offline"
	if online {
		state = "online"
	}
	h.AuditLog(c, "repo.online", "repository", name, name, "online="+state, "ok")
	c.JSON(http.StatusOK, h.toRepo(repo, nil))
}

// RecheckRepositoryConnection 手动重测仓库上游连接，仅管理员（FR-114）。
// 仅 online 的 proxy 仓库可重测：立即 HEAD 探测并更新 auto-block 状态，返回最新状态。
func (h *Handlers) RecheckRepositoryConnection(c *gin.Context, name RepoNameParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.assets == nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal_error", "连接探测未启用")
		return
	}
	repo, err := h.repos.Get(name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	if repo.Type != "proxy" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仅 proxy 仓库可重测连接")
		return
	}
	if !repo.Online {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仓库已离线，请先上线后再重测")
		return
	}
	cfg, err := repo.DecodeConfig()
	if err != nil || cfg.RemoteURL == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "仓库未配置上游地址")
		return
	}
	h.AuditLog(c, "repo.recheck", "repository", name, name, "上游已配置", "ok")
	status := h.assets.RecheckConnection(repo.ID, cfg.RemoteURL)
	c.JSON(http.StatusOK, toAPIConnectionStatus(repo, status))
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
	h.AuditLog(c, "repo.delete", "repository", name, name, "", "ok")
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
	repo, snippets, err := h.repos.Usage(name, h.apiBaseURL(c), requestLang(c))
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]UsageSnippet, 0, len(snippets))
	for _, s := range snippets {
		items = append(items, toAPIUsageSnippet(s))
	}
	out := UsageInfo{Format: repo.Format, Type: repo.Type, Snippets: items}
	// 匿名详情页经 usage 端点取仓库基本信息，描述一并带出。
	if repo.Description != "" {
		desc := repo.Description
		out.Description = &desc
	}
	c.JSON(http.StatusOK, out)
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
	h.AuditLog(c, "acl.set", "acl", name, name, "entries="+strconv.Itoa(len(rows)), "ok")
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

// apiBaseURL 返回对外基址（scheme + host），供使用片段拼接客户端地址。
// FR-87：配置了 publicURL（对外 CDN 域名）则优先使用，隐藏源站地址；
// FR-89：改为优先读 setting（web 可运行时修改），未配置回退启动值 / 请求推断。
func (h *Handlers) apiBaseURL(c *gin.Context) string {
	if h.settings != nil {
		if u := h.settings.PublicURL(); u != "" {
			return u
		}
	}
	if h.publicURL != "" {
		return h.publicURL
	}
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
// 响应附带 pinnedNames（全局置顶且匿名可读的仓库名），使**公开页也能展示置顶**。
// 路径已在契约内（api/openapi.yaml /api/v1/public/repositories），由生成的
// ServerInterfaceWrapper 注册，此处实现接口方法。
func (h *Handlers) ListPublicRepositories(c *gin.Context) {
	if !h.anonymousAllowed(c) {
		return
	}
	rows, statsMap, err := h.listAllRepositories("", "")
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]Repository, 0, len(rows))
	for i := range rows {
		allowed, accessErr := h.repos.CanAccess(rows[i].Name, 0, "read")
		if accessErr != nil || !allowed {
			continue
		}
		stats := statsMap[rows[i].ID]
		items = append(items, toAPIRepository(&rows[i], &stats))
	}
	pinnedNames, err := h.globalPinnedNames()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, PublicRepositoryList{Items: items, Total: len(items), PinnedNames: pinnedNames})
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
		Path          string `json:"path"`
		Size          int64  `json:"size"`
		Hash          string `json:"hash"`
		Sha1          string `json:"sha1,omitempty"`
		Md5           string `json:"md5,omitempty"`
		ContentType   string `json:"contentType,omitempty"`
		CreatedAt     string `json:"createdAt,omitempty"`
		UpdatedAt     string `json:"updatedAt"`
		DownloadCount int64  `json:"downloadCount"` // FR-142：累计完整下载次数（原始口径）
	}
	// FR-142：本层文件批量取下载计数（单次 IN 查询；空层不发起查询）。
	var counts map[string]int64
	if h.assetDownloads != nil {
		paths := make([]string, 0, len(entry.Files))
		for _, f := range entry.Files {
			paths = append(paths, f.Path)
		}
		counts, err = h.assetDownloads.SumPaths(name, paths)
		if err != nil {
			writeDomainErr(c, err)
			return
		}
	}
	files := make([]fileItem, 0, len(entry.Files))
	for _, f := range entry.Files {
		files = append(files, fileItem{
			Path:          f.Path,
			Size:          f.Size,
			Hash:          f.BlobHash,
			Sha1:          f.Sha1,
			Md5:           f.Md5,
			ContentType:   f.ContentType,
			CreatedAt:     f.CreatedAt,
			UpdatedAt:     f.UpdatedAt,
			DownloadCount: counts[f.Path],
		})
	}
	dirs := entry.Dirs
	if dirs == nil {
		dirs = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"directories": dirs, "files": files})
}

// RepositoryDownloadTrendResponse 是仓库详情页「下载趋势 + 仓库总下载」的响应。
// Trend 为补零后的连续桶序列（前端折线直接可画）；TotalDownloadCount 是仓库全时段
// 累计下载（**原始口径**，与树节点 downloadCount 列同口径，不受 from/to 约束）。
type RepositoryDownloadTrendResponse struct {
	From               time.Time            `json:"from"`
	To                 time.Time            `json:"to"`
	EffectiveBucket    ObservabilityBucket  `json:"effectiveBucket"`
	TotalDownloadCount int64                `json:"totalDownloadCount"`
	Trend              []DownloadTrendPoint `json:"trend"`
}

// GetRepositoryDownloadTrend 返回仓库详情页的下载趋势与仓库总下载（原始累计口径）。
// 非契约端点，经 WithProtocolRoutes 注册，与 ListRepositoryTree 同级同风格——tree 不在契约内，
// 故本端点路径同样不入契约；响应结构已收口至 openapi.yaml components/schemas 的
// RepositoryDownloadTrendResponse（供文档与前端类型，路径仅此处手写注册）。
// 权限：与树一致走 requireRepoRead —— 仓库详情页匿名也可见制品树与 downloadCount（FR-142），
// 本端点只暴露仓库级聚合时序，不含来源 IP / UA 明文，故不高于树的读权限；
// 私有仓匿名仍 401、非授权主体 403（由 requireRepoRead 统一判定）。
func (h *Handlers) GetRepositoryDownloadTrend(c *gin.Context) {
	name := c.Param("name")
	if _, ok := h.requireRepoRead(c, name); !ok {
		return
	}
	// requireRepoRead 的管理员快捷分支不校验仓库存在性（tree 端点由 ListDirectory 兜底
	// 404）；本端点只查下载计量表，故显式补一次存在性查询，避免对不存在的仓库
	// 返回「全零趋势 200」的错觉。非管理员分支已由 requireRepoRead 查过，此处幂等。
	if _, err := h.repos.CanAccess(name, 0, "read"); err != nil {
		writeDomainErr(c, err)
		return
	}
	from, to, ok := observabilityRangeQuery(c)
	if !ok {
		return
	}
	if h.assetDownloads == nil {
		// 下载计量仓储未接线时返回 409，不伪造 0 计数的空趋势。
		authWriteUnavailable(c)
		return
	}
	bucket := operationsBucket(from, to)
	rows, err := h.assetDownloads.DownloadTrendForRepo(name, from, to, bucket)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	total, err := h.assetDownloads.SumByRepo(name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	values := make(map[time.Time]int64, len(rows))
	for _, row := range rows {
		start, parseErr := time.Parse(time.RFC3339, row.Bucket)
		if parseErr != nil {
			continue
		}
		values[truncateOperationsBucket(start, bucket)] = row.Count
	}
	response := RepositoryDownloadTrendResponse{
		From: from, To: to, EffectiveBucket: ObservabilityBucket(bucket),
		TotalDownloadCount: total,
		Trend:              zeroFilledDownloadTrendPoints(values, from, to, bucket),
	}
	c.JSON(http.StatusOK, response)
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
	if v := c.Query("page"); v != "" {
		if n, err := parseIntQuery(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := parseSearchPageSize(c.Query("page_size"))
	limit := pageSize
	offset := (page - 1) * pageSize

	// 排序参数白名单（非法值静默回落默认 path asc）
	sort := c.Query("sort")
	switch sort {
	case "name", "repo", "path", "size", "updated":
	default:
		sort = ""
	}
	order := c.Query("order")
	if order != "desc" {
		order = "asc"
	}

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

	// 仓库范围过滤（下推到服务层，保证 total 精确）
	repoFilter := c.Query("repository")
	if repoFilter != "" {
		// 单仓库内搜索：先检查读权限
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

	out, err := h.repos.SearchAssets(q, repoFilter, subjectID, isAdmin, sort, order, limit, offset)
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
	type searchFacet struct {
		Repository string `json:"repository"`
		Count      int    `json:"count"`
	}
	items := make([]searchItem, 0, len(out.Items))
	for _, r := range out.Items {
		items = append(items, searchItem{
			Repository: r.RepoName,
			Path:       r.Asset.Path,
			Size:       r.Asset.Size,
			Hash:       r.Asset.BlobHash,
			UpdatedAt:  r.Asset.UpdatedAt,
		})
	}
	facets := make([]searchFacet, 0, len(out.Facets))
	for _, f := range out.Facets {
		facets = append(facets, searchFacet{Repository: f.RepoName, Count: f.Count})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": out.Total, "facets": facets})
}

// parseIntQuery 尝试将字符串解析为 int。
// 搜索分页参数口径：非法/非正数回落默认；**超上限夹取到上限**。
// 关键回归（仓库内搜索「只搜到前几个结果」的根因）：此前实现为「n <= 100 才采纳，
// 否则整个忽略」——请求 200 条会被静默丢弃、回落默认 20 条，调用方与用户都无任何信号。
const (
	searchPageSizeDefault = 20
	searchPageSizeMax     = 100
)

func parseSearchPageSize(raw string) int {
	if raw == "" {
		return searchPageSizeDefault
	}
	n, err := parseIntQuery(raw)
	if err != nil || n <= 0 {
		return searchPageSizeDefault
	}
	if n > searchPageSizeMax {
		return searchPageSizeMax
	}
	return n
}

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
