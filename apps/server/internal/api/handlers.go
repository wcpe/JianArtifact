// Package api 是 HTTP 边界层：实现契约（api/openapi.yaml）生成的 ServerInterface，
// 编排 domain 服务，并复用 auth 中间件解析出的主体做认证 / 鉴权判定。
//
// 分层（见 internal/doc.go）：api -> domain, auth。api 不直连 repository / persistence。
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// Deps 汇集 Handlers 的依赖。健康 / 就绪端点仅用 Version 与 Checks，
// 管理端点用各 domain 服务；Migration 供 /status 报告迁移版本。
type Deps struct {
	Version    string
	Checks     []func() error
	Migration  func() (string, error)
	Auth       *domain.AuthService
	Users      *domain.UserService
	Tokens     *domain.TokenService
	Repos      *domain.RepositoryService
	Migrations *domain.MigrationService
	Settings   *domain.SettingService
}

// Handlers 实现 ServerInterface 的全部端点。
type Handlers struct {
	version    string
	checks     []func() error
	migration  func() (string, error)
	auth       *domain.AuthService
	users      *domain.UserService
	tokens     *domain.TokenService
	repos      *domain.RepositoryService
	migrations *domain.MigrationService
	settings   *domain.SettingService
}

// NewHandlers 构造 Handlers。
func NewHandlers(d Deps) *Handlers {
	return &Handlers{
		version:    d.Version,
		checks:     d.Checks,
		migration:  d.Migration,
		auth:       d.Auth,
		users:      d.Users,
		tokens:     d.Tokens,
		repos:      d.Repos,
		migrations: d.Migrations,
		settings:   d.Settings,
	}
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
	c.JSON(http.StatusOK, HealthStatus{Status: Ok, Version: h.versionFor(c)})
}

// GetReadyz 就绪探针：全部就绪自检通过才 200，任一未过返回 503；版本号仅对已认证请求返回。
func (h *Handlers) GetReadyz(c *gin.Context) {
	if !h.ready() {
		c.JSON(http.StatusServiceUnavailable, HealthStatus{Status: Unavailable, Version: h.versionFor(c)})
		return
	}
	c.JSON(http.StatusOK, HealthStatus{Status: Ok, Version: h.versionFor(c)})
}

// GetStatus 运行时状态：就绪、初始化标志与用户数匿名可见（前端自举引导依赖），
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
	switch {
	case errors.Is(err, domain.ErrInvalidCredentials):
		auth.WriteError(c, http.StatusUnauthorized, "invalid_credentials", "用户名或口令错误")
	case errors.Is(err, domain.ErrAlreadyInitialized):
		auth.WriteError(c, http.StatusConflict, "already_initialized", "实例已初始化，自举关闭")
	case errors.Is(err, domain.ErrConflict):
		auth.WriteError(c, http.StatusConflict, "conflict", "资源已存在")
	case errors.Is(err, domain.ErrNotFound):
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, domain.ErrValidation):
		auth.WriteError(c, http.StatusBadRequest, "validation_error", err.Error())
	case errors.Is(err, domain.ErrUpstream):
		auth.WriteError(c, http.StatusBadGateway, "upstream_error", "上游不可达或返回错误")
	case errors.Is(err, domain.ErrUpstreamTimeout):
		auth.WriteError(c, http.StatusGatewayTimeout, "upstream_timeout", "上游超时")
	default:
		auth.WriteError(c, http.StatusInternalServerError, "internal_error", "内部错误")
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
		Id:        u.ID,
		Username:  u.Username,
		Role:      UserRole(u.Role),
		Status:    UserStatus(u.Status),
		CreatedAt: u.CreatedAt,
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
		CreatedAt:  r.CreatedAt,
	}
	if cfg, err := r.DecodeConfig(); err == nil {
		if cfg.RemoteURL != "" {
			out.RemoteUrl = &cfg.RemoteURL
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
