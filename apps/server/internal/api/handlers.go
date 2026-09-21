// Package api 是 HTTP 边界层：实现契约（api/openapi.yaml）生成的 ServerInterface，
// 编排 domain 服务，并复用 auth 中间件解析出的主体做认证 / 鉴权判定。
//
// 分层（见 internal/doc.go）：api -> domain, auth。api 不直连 repository / persistence。
package api

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// Deps 汇集 Handlers 的依赖。健康 / 就绪端点仅用 Version 与 Checks，
// 管理端点用各 domain 服务；Migration 供 /status 报告迁移版本。
type Deps struct {
	Version                 string
	Checks                  []func() error
	Migration               func() (string, error)
	Auth                    *domain.AuthService
	Users                   *domain.UserService
	Tokens                  *domain.TokenService
	Repos                   *domain.RepositoryService
	Assets                  *domain.AssetService // FR-103：制品批量删除
	Migrations              *domain.MigrationService
	Settings                *domain.SettingService
	AuditLogs               *repository.AuditLogRepo                // FR-38：审计日志
	AuditObservability      *repository.AuditObservabilityRepo      // FR-118：统一审计读模型
	OperationsObservability *repository.OperationsObservabilityRepo // FR-53/120：当前节点业务与主机读模型
	OperationsAlerts        *repository.OperationsAlertRepo         // v0.8.0：运维告警去重持久化
	AssetDownloads          *repository.AssetDownloadRepo           // FR-142：制品下载计量明细（tree 计数列）
	AuditAttentionKey       []byte                                  // FR-118：关注标识签名密钥（由启动密钥派生）
	AuditSourceNode         string                                  // FR-109：本节点审计标识
	ClusterTokenSet         bool                                    // FR-86：同步令牌是否已配置（不暴露明文）
	PublicURL               string                                  // FR-87：对外基础 URL（CDN 域名，隐藏源站 IP）
	EnabledFormats          []string                                // FR-32：启动时启用的格式清单
	PublishPolicies         *domain.PublishPolicyService            // FR-109：发布账号策略管理
	OnUpstreamTimeoutChange func(time.Duration)                     // FR-89：回源超时设置变更回调（wiring 注入 upstream.Client.SetTimeout；nil 不触发）
	Backups                 *domain.BackupService                   // FR-132：节点备份包生成与登记
	BackupLinkKey           []byte                                  // FR-132：备份下载令牌签名密钥（由启动密钥派生）
	Freeze                  *domain.FreezeController                // FR-135：运行时写入冻结窗口
	BackupImports           *domain.BackupImportService             // FR-137：导入记录与 URL 拉取
	BackupUploads           *domain.BackupUploadService             // FR-137：分片上传（Web 第三通道）
}

// Handlers 实现 ServerInterface 的全部端点。
type Handlers struct {
	version                 string
	checks                  []func() error
	migration               func() (string, error)
	auth                    *domain.AuthService
	users                   *domain.UserService
	tokens                  *domain.TokenService
	repos                   *domain.RepositoryService
	assets                  *domain.AssetService
	migrations              *domain.MigrationService
	settings                *domain.SettingService
	auditLogs               *repository.AuditLogRepo // FR-38：审计日志
	auditObservability      *repository.AuditObservabilityRepo
	operationsObservability *repository.OperationsObservabilityRepo
	operationsAlertStore    *repository.OperationsAlertRepo
	assetDownloads          *repository.AssetDownloadRepo // FR-142：制品下载计量明细（tree 计数列）
	auditAttentionKey       []byte
	auditSourceNode         string // FR-109：本节点审计标识
	clusterTokenSet         bool
	publicURL               string                       // FR-87：对外基础 URL（CDN 域名）
	enabledFormats          []string                     // FR-32：启动时启用的格式清单
	publishPolicies         *domain.PublishPolicyService // FR-109：发布账号策略管理
	onUpstreamTimeoutChange func(time.Duration)          // FR-89：回源超时设置变更回调
	backups                 *domain.BackupService        // FR-132：节点备份包
	backupLinks             *domain.BackupLinkSigner     // FR-132：下载令牌签名
	freeze                  *domain.FreezeController     // FR-135：运行时写入冻结
	backupImports           *domain.BackupImportService  // FR-137：导入记录与 URL 拉取
	backupUploads           *domain.BackupUploadService  // FR-137：分片上传（Web 第三通道）
}

// NewHandlers 构造 Handlers。
func NewHandlers(d Deps) *Handlers {
	return &Handlers{
		version:                 d.Version,
		checks:                  d.Checks,
		migration:               d.Migration,
		auth:                    d.Auth,
		users:                   d.Users,
		tokens:                  d.Tokens,
		repos:                   d.Repos,
		assets:                  d.Assets,
		migrations:              d.Migrations,
		settings:                d.Settings,
		auditLogs:               d.AuditLogs,
		auditObservability:      d.AuditObservability,
		operationsObservability: d.OperationsObservability,
		operationsAlertStore:    d.OperationsAlerts,
		assetDownloads:          d.AssetDownloads,
		auditAttentionKey:       auditAttentionKey(d.AuditAttentionKey, d.AuditSourceNode),
		auditSourceNode:         d.AuditSourceNode,
		clusterTokenSet:         d.ClusterTokenSet,
		publicURL:               d.PublicURL,
		enabledFormats:          append([]string(nil), d.EnabledFormats...),
		publishPolicies:         d.PublishPolicies,
		onUpstreamTimeoutChange: d.OnUpstreamTimeoutChange,
		backups:                 d.Backups,
		backupLinks:             domain.NewBackupLinkSigner(d.BackupLinkKey),
		freeze:                  d.Freeze,
		backupImports:           d.BackupImports,
		backupUploads:           d.BackupUploads,
	}
}

func auditAttentionKey(key []byte, sourceNode string) []byte {
	if len(key) > 0 {
		sum := sha256.Sum256(append(append([]byte(nil), key...), []byte("jianartifact/audit-attention/v1")...))
		return sum[:]
	}
	sum := sha256.Sum256([]byte("test-only/audit-attention/" + sourceNode))
	return sum[:]
}

// 编译期断言：Handlers 满足契约生成的接口。
var _ ServerInterface = (*Handlers)(nil)

// versionFor 按认证状态返回版本号：匿名请求脱敏为空串，
// 避免向公网暴露精确版本信息（降低已知漏洞被针对性利用的侦察价值）。
func (h *Handlers) versionFor(c *gin.Context) string {
	if _, ok := auth.PrincipalFrom(c); ok {
		return h.version
	}
	return ""
}

// GetHealthz 存活探针：进程存活即 200；版本号仅对已认证请求返回。
func (h *Handlers) GetHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, HealthStatus{Status: HealthStatusStatusOk, Version: h.versionFor(c)})
}

// GetReadyz 就绪探针：全部就绪自检通过才 200，任一未过返回 503；版本号仅对已认证请求返回。
func (h *Handlers) GetReadyz(c *gin.Context) {
	if !h.ready() {
		c.JSON(http.StatusServiceUnavailable, HealthStatus{Status: HealthStatusStatusUnavailable, Version: h.versionFor(c)})
		return
	}
	c.JSON(http.StatusOK, HealthStatus{Status: HealthStatusStatusOk, Version: h.versionFor(c)})
}

// GetStatus 运行时状态：就绪、初始化标志、用户数与自举许可匿名可见（前端自举引导依赖），
// 版本与迁移版本仅对已认证请求返回（匿名脱敏为空串）。
func (h *Handlers) GetStatus(c *gin.Context) {
	count := 0
	if h.users != nil {
		if n, err := h.users.Count(); err == nil {
			count = n
		}
	}
	info := StatusInfo{
		Ready:       h.ready(),
		Initialized: count > 0,
		UserCount:   count,
		// 复制退役：不再有 standby，自举只取决于是否已有用户。
		BootstrapAllowed: count == 0,
	}
	if _, ok := auth.PrincipalFrom(c); ok {
		info.Version = h.version
		if h.migration != nil {
			if v, err := h.migration(); err == nil {
				info.MigrationVersion = v
			}
		}
	}
	c.JSON(http.StatusOK, info)
}

// GetEnabledFormats 返回本进程启动时启用的协议格式，仅管理员可见。
func (h *Handlers) GetEnabledFormats(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	items := make([]EnabledFormatsFormats, len(h.enabledFormats))
	for i, name := range h.enabledFormats {
		items[i] = EnabledFormatsFormats(name)
	}
	c.JSON(http.StatusOK, EnabledFormats{Formats: items})
}

// ready 判断全部就绪自检是否通过。
func (h *Handlers) ready() bool {
	for _, check := range h.checks {
		if check == nil {
			continue
		}
		if err := check(); err != nil {
			return false
		}
	}
	return true
}

// requirePrincipal 取出已认证主体；缺失则写 401 并返回 false。
func requirePrincipal(c *gin.Context) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(c)
	if !ok {
		auth.WriteError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
		return nil, false
	}
	if p.WebLoginDisabled {
		auth.WriteError(c, http.StatusForbidden, "web_login_disabled", "该账号已禁止登录管理端")
		return nil, false
	}
	return p, true
}

// requireAdmin 取出主体并要求管理员角色；未认证 401、非管理员 403。
func requireAdmin(c *gin.Context) (*auth.Principal, bool) {
	p, ok := requirePrincipal(c)
	if !ok {
		return nil, false
	}
	if !p.IsAdmin() {
		auth.WriteError(c, http.StatusForbidden, "forbidden", "需要管理员权限")
		return nil, false
	}
	return p, true
}

// writeDomainErr 把领域错误映射为契约约定的 HTTP 状态与错误码。
func writeDomainErr(c *gin.Context, err error) {
	writeDomainErrWithOperationID(c, err, "")
}

// writeDomainErrWithOperationID 将领域错误映射为契约约定的 HTTP 状态与错误码。
// operationID 仅用于已进入统一资产操作执行阶段的失败，其他端点保持既有错误响应形状。
func writeDomainErrWithOperationID(c *gin.Context, err error, operationID string) {
	write := func(status int, code, message string) {
		if operationID == "" {
			auth.WriteError(c, status, code, message)
			return
		}
		c.JSON(status, gin.H{
			"error":       gin.H{"code": code, "message": message},
			"operationId": operationID,
		})
	}
	switch {
	case errors.Is(err, domain.ErrInvalidCredentials):
		write(http.StatusUnauthorized, "invalid_credentials", "用户名或口令错误")
	case errors.Is(err, domain.ErrAlreadyInitialized):
		write(http.StatusConflict, "already_initialized", "实例已初始化，自举关闭")
	case errors.Is(err, domain.ErrConflict):
		write(http.StatusConflict, "conflict", "资源已存在")
	case errors.Is(err, domain.ErrFormatDisabled):
		write(http.StatusConflict, "format_disabled", "请求的格式未启用")
	case errors.Is(err, domain.ErrLogicalDeleteRequired):
		write(http.StatusConflict, "logical_delete_required", "必须使用格式感知的逻辑删除目标")
	case errors.Is(err, domain.ErrOperationUnsupported):
		write(http.StatusConflict, "operation_unsupported", "该格式不支持此制品操作")
	case errors.Is(err, domain.ErrOperationLimit):
		write(http.StatusBadRequest, "operation_limit", "单次实际制品数不得超过 500")
	case errors.Is(err, domain.ErrNotFound):
		write(http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, domain.ErrValidation):
		write(http.StatusBadRequest, "validation_error", err.Error())
	case errors.Is(err, domain.ErrUpstream):
		write(http.StatusBadGateway, "upstream_error", "上游不可达或返回错误")
	case errors.Is(err, domain.ErrUpstreamTimeout):
		write(http.StatusGatewayTimeout, "upstream_timeout", "上游超时")
	default:
		write(http.StatusInternalServerError, "internal_error", "内部错误")
	}
}

// bindJSON 解析请求体；失败写 400 并返回 false。
func bindJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return false
	}
	return true
}

// pageOffset 解析分页参数，返回 (limit, offset)。默认第 1 页、每页 20，上限 100。
func pageOffset(page, pageSize *int) (int, int) {
	p, ps := 1, 20
	if page != nil && *page > 0 {
		p = *page
	}
	if pageSize != nil && *pageSize > 0 {
		ps = *pageSize
	}
	if ps > 100 {
		ps = 100
	}
	return ps, (p - 1) * ps
}

// toAPIUser 把行模型转为契约 User。
func toAPIUser(u *repository.User) User {
	return User{
		Id:               u.ID,
		Username:         u.Username,
		Role:             UserRole(u.Role),
		Status:           UserStatus(u.Status),
		WebLoginDisabled: u.WebLoginDisabled,
		CreatedAt:        u.CreatedAt,
	}
}

// toAPIRepository 把行模型转为契约 Repository。stats 非 nil 时填入统计字段。
func toAPIRepository(r *repository.Repository, stats *domain.RepoStats) Repository {
	out := Repository{
		Id:         r.ID,
		Name:       r.Name,
		Format:     RepositoryFormat(r.Format),
		Type:       RepositoryType(r.Type),
		Visibility: RepositoryVisibility(r.Visibility),
		Online:     &r.Online,
		CreatedAt:  r.CreatedAt,
	}
	if r.Description != "" {
		desc := r.Description
		out.Description = &desc
	}
	if cfg, err := r.DecodeConfig(); err == nil {
		immutable := cfg.ImmutableRelease
		out.ImmutableRelease = &immutable
		if safeRemoteURLForResponse(cfg.RemoteURL) {
			out.RemoteUrl = &cfg.RemoteURL
		}
		if cfg.CredentialRef != "" {
			out.CredentialRef = &cfg.CredentialRef
		}
		if len(cfg.Members) > 0 {
			members := cfg.Members
			out.Members = &members
		}
	}
	if stats != nil {
		count := int(stats.Count)
		totalSize := stats.TotalSize
		out.ArtifactCount = &count
		out.TotalSize = &totalSize
	}
	return out
}

// safeRemoteURLForResponse 仅回显与当前 proxy 配置规则一致的上游地址。
// 遗留配置可能绕过现有校验写入，管理端不得因此暴露用户信息或查询中的秘密。
func safeRemoteURLForResponse(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.IsAbs() && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// toRepo 把行模型转为契约 Repository，并按 MD-2 规则填充连接状态（FR-114）。
func (h *Handlers) toRepo(r *repository.Repository, stats *domain.RepoStats) Repository {
	out := toAPIRepository(r, stats)
	out.ConnectionStatus = h.connectionStatus(r)
	return out
}

// connectionStatus 返回仓库连接状态（FR-114）：assets 未注入或 hosted 仓库返回 nil。
func (h *Handlers) connectionStatus(r *repository.Repository) *ConnectionStatus {
	if h.assets == nil {
		return nil
	}
	return toAPIConnectionStatus(r, h.assets.Status(r.ID))
}

// toAPIConnectionStatus 按 MD-2 状态合并规则生成契约 ConnectionStatus：
//   - hosted 不展示连接状态（返回 nil）；
//   - online=false（离线）优先覆盖其它状态：无论内存态为何一律 OFFLINE；
//   - 仅 online=true 显示内存态连接状态（READY/AVAILABLE/AUTO_BLOCKED/UNAVAILABLE）；
//   - group 无独立上游，online 时显示 READY（未连接）。
func toAPIConnectionStatus(r *repository.Repository, h domain.RemoteHealth) *ConnectionStatus {
	if r.Type == "hosted" {
		return nil
	}
	if !r.Online {
		return &ConnectionStatus{Status: ConnectionStatusStatus(domain.StatusOffline), Description: optionalString("已手动离线")}
	}
	switch h.Status {
	case domain.StatusAutoBlocked:
		out := &ConnectionStatus{Status: ConnectionStatusStatus(domain.StatusAutoBlocked), Description: optionalString("上游不可用，自动阻止中")}
		if !h.BlockedUntil.IsZero() {
			until := h.BlockedUntil
			out.BlockedUntil = &until
		}
		return out
	case domain.StatusUnavailable:
		return &ConnectionStatus{Status: ConnectionStatusStatus(domain.StatusUnavailable), Description: optionalString("上游不可用")}
	case domain.StatusAvailable:
		return &ConnectionStatus{Status: ConnectionStatusStatus(domain.StatusAvailable), Description: optionalString("上游可用")}
	default:
		// READY 及未知状态：尚未探测（未连接）。
		if r.Type == "group" {
			return &ConnectionStatus{Status: ConnectionStatusStatus(domain.StatusReady), Description: optionalString("尚未探测（group 无独立上游）")}
		}
		return &ConnectionStatus{Status: ConnectionStatusStatus(domain.StatusReady), Description: optionalString("尚未探测")}
	}
}

// toAPIToken 把行模型转为契约 Token。
func toAPIToken(t repository.Token) Token {
	return Token{Id: t.ID, Name: t.Name, CreatedAt: t.CreatedAt}
}

// toAPIAsset 把 asset 行模型转为契约 AssetSummary。
func toAPIAsset(a *repository.Asset) AssetSummary {
	out := AssetSummary{
		Path:      a.Path,
		Size:      a.Size,
		Hash:      a.BlobHash,
		Sha1:      &a.Sha1,
		Md5:       &a.Md5,
		UpdatedAt: a.UpdatedAt,
	}
	if a.ContentType != "" {
		ct := a.ContentType
		out.ContentType = &ct
	}
	if a.CreatedAt != "" {
		created := a.CreatedAt
		out.CreatedAt = &created
	}
	return out
}

// toAPIUsageSnippet 把领域使用片段转为契约 UsageSnippet。
func toAPIUsageSnippet(s domain.UsageSnippet) UsageSnippet {
	out := UsageSnippet{Title: s.Title, Code: s.Code}
	if s.Description != "" {
		d := s.Description
		out.Description = &d
	}
	return out
}

// toAPIAcl 把行模型转为契约 AclEntry。
func toAPIAcl(a repository.Acl) AclEntry {
	return AclEntry{SubjectId: a.SubjectID, Action: AclEntryAction(a.Action)}
}

// stringValue 解引用可能为 nil 的字符串指针，返回空字符串或取值。
// 由已退役的复制应用日志处理器原定义，现保留为包级工具函数供审计/连接状态处理复用。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// optionalString 把空字符串映射为 nil 指针，非空则为指向该值的指针。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
