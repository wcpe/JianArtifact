// Package httpserver 装配 JianArtifact 的 HTTP 服务：挂载据契约（api/openapi.yaml）
// 生成的路由、健康 / 就绪端点，以及内嵌前端静态资源（SPA 回退）。
// 见 docs/adr/0004-design-first-openapi.md 与 docs/adr/0005-single-binary-embed.md。
package httpserver

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// ReadinessCheck 是就绪自检钩子：返回非 nil 错误表示某依赖未就绪。
// 0.1.0 无外部依赖，检查集合为空（恒就绪）；后续版本注入 SQLite / blob 存储等自检。
type ReadinessCheck func() error

// Server 实现契约生成的 api.ServerInterface，并持有版本与就绪检查集合。
// 管理与状态端点由注入的 api.ServerInterface（api.Handlers）经嵌入提供；
// Server 自身覆盖 GetHealthz / GetReadyz，使无依赖装配（如测试）也能提供健康探针。
type Server struct {
	api.ServerInterface // 管理 + 状态 handler（WithHandlers 注入；未注入时仅健康探针可用）

	version                 string
	checks                  []ReadinessCheck
	middlewares             []api.MiddlewareFunc
	protocolRoutes          func(gin.IRouter)
	protocolPrefixes        []string
	writeFreeze             func() domain.FreezeState // FR-135：运行时写入冻结窗口状态读取器（nil 表示不启用）
	managementSecurityAudit SecurityAuditFunc
	protocolMetric          func(*gin.Context)
	allowedHosts            func() []string  // 允许访问的域名白名单（nil 表示不启用限制）
	originToken             originTokenGuard // 回源 Token 校验配置读取器（nil 表示不启用）
}

// Option 配置 Server。
type Option func(*Server)

// WithReadinessCheck 追加一个就绪自检钩子。
func WithReadinessCheck(c ReadinessCheck) Option {
	return func(s *Server) { s.checks = append(s.checks, c) }
}

// WithHandlers 注入实现管理与状态端点的 api.ServerInterface。
func WithHandlers(h api.ServerInterface) Option {
	return func(s *Server) { s.ServerInterface = h }
}

// WithMiddleware 追加应用到全部契约路由的 Gin 中间件（如鉴权主体解析）。
func WithMiddleware(m ...api.MiddlewareFunc) Option {
	return func(s *Server) { s.middlewares = append(s.middlewares, m...) }
}

// WithProtocolRoutes 注入协议层路由注册闭包（如 Raw/Maven/npm）。
// 这些端点不在 OpenAPI 契约内，注册在契约路由之后、静态 SPA 回退之前，
// 因此不与 /api/v1、/healthz 冲突，且优先于前端回退。
func WithProtocolRoutes(register func(gin.IRouter)) Option {
	return func(s *Server) { s.protocolRoutes = register }
}

// WithProtocolPrefixes 声明协议保留前缀，未装配的前缀不得回退到 SPA 首页。
func WithProtocolPrefixes(prefixes ...string) Option {
	return func(s *Server) { s.protocolPrefixes = append(s.protocolPrefixes, prefixes...) }
}

// WithProtocolMetric 在制品协议请求完成后接收最终状态，管理与静态请求不会调用它。
func WithProtocolMetric(metric func(*gin.Context)) Option {
	return func(s *Server) { s.protocolMetric = metric }
}

// WithAllowedHosts 注入允许访问的域名白名单读取器（运行时读取最新值，如后台设置）。
// 非 nil 时，所有非回环请求的 Host 必须命中白名单，否则 404；空列表表示不限制。
// nil 表示完全关闭该检查（保持旧行为）。
func WithAllowedHosts(provider func() []string) Option {
	return func(s *Server) { s.allowedHosts = provider }
}

// WithOriginTokenGuard 注入回源 Token 校验配置读取器（FR-130，运行时读取最新值）。
// 非 nil 时启用校验中间件：开启状态下非回环请求必须携带匹配的 Token 头，否则 404。
func WithOriginTokenGuard(guard originTokenGuard) Option {
	return func(s *Server) { s.originToken = guard }
}

// New 构造 Server。
func New(version string, opts ...Option) *Server {
	s := &Server{version: version}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// 编译期断言：Server 满足契约生成的接口。
var _ api.ServerInterface = (*Server)(nil)

// versionFor 按认证状态返回版本号：匿名请求脱敏为空串（与 api.Handlers 同策略），
// 避免向公网暴露精确版本信息。
func (s *Server) versionFor(c *gin.Context) string {
	if _, ok := auth.PrincipalFrom(c); ok {
		return s.version
	}
	return ""
}

// GetHealthz 存活探针：进程存活即 200；版本号仅对已认证请求返回。
func (s *Server) GetHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, api.HealthStatus{Status: api.HealthStatusStatusOk, Version: s.versionFor(c)})
}

// GetReadyz 就绪探针：全部就绪自检通过才 200，任一未过返回 503；版本号仅对已认证请求返回。
func (s *Server) GetReadyz(c *gin.Context) {
	for _, check := range s.checks {
		if err := check(); err != nil {
			c.JSON(http.StatusServiceUnavailable, api.HealthStatus{Status: api.HealthStatusStatusUnavailable, Version: s.versionFor(c)})
			return
		}
	}
	c.JSON(http.StatusOK, api.HealthStatus{Status: api.HealthStatusStatusOk, Version: s.versionFor(c)})
}

// Handler 装配并返回完整的 gin.Engine：契约路由优先，其余交给内嵌前端静态资源
// （带 SPA 回退）。assets 为 nil 时不挂载静态资源（便于测试）。
func (s *Server) Handler(assets fs.FS) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestIDMiddleware())
	if s.allowedHosts != nil {
		r.Use(hostFilterMiddleware(s.allowedHosts))
	}
	if s.originToken != nil {
		r.Use(originTokenMiddleware(s.originToken))
	}
	if s.protocolMetric != nil {
		r.Use(func(c *gin.Context) {
			c.Next()
			if s.isProtocolPath(c.Request.URL.Path) {
				s.protocolMetric(c)
			}
		})
	}
	// 审计上下文中间件：记录请求耗时并捕获脱敏请求体（仅管理写请求），
	// 供审计写入方填充 http_method/http_path/status_code/duration_ms/body_preview。
	r.Use(auditctx.TimingMiddleware())
	r.Use(auditctx.BodyPreviewMiddleware(auditctx.IsManagementWrite))
	if s.managementSecurityAudit != nil {
		r.Use(managementSecurityAuditMiddleware(s.managementSecurityAudit))
	}
	if s.writeFreeze != nil {
		r.Use(writeFreezeMiddleware(s.writeFreeze))
	}

	api.RegisterHandlersWithOptions(r, s, api.GinServerOptions{Middlewares: s.middlewares})

	if s.protocolRoutes != nil {
		s.protocolRoutes(r)
	}

	if assets != nil {
		s.mountStatic(r, assets)
	}
	return r
}

func (s *Server) isProtocolPath(requestPath string) bool {
	for _, prefix := range s.protocolPrefixes {
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return true
		}
	}
	return false
}

// mountStatic 把未命中契约路由的 GET/HEAD 请求交给前端静态资源；
// 资源不存在时回退到 index.html，支持前端客户端路由（SPA）。
//
// 缓存策略（根治"前端发版后浏览器仍显示旧版"）：
//   - index.html（含 SPA 回退）→ Cache-Control: no-cache：每次请求回源验证，
//     发版后刷新立即拿到引用最新 content-hash 资源的入口页；
//   - /assets/*（构建产物带 content-hash，内容变则文件名变）→ 长缓存 + immutable；
//   - 其他静态文件（favicon 等无 hash）→ no-cache，避免误缓存导致更新不生效。
func (s *Server) mountStatic(r *gin.Engine, assets fs.FS) {
	fileServer := http.FileServer(http.FS(assets))
	serveIndex := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Request.URL.Path = "/"
		fileServer.ServeHTTP(c.Writer, c.Request)
	}
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Status(http.StatusNotFound)
			return
		}
		for _, prefix := range s.protocolPrefixes {
			if c.Request.URL.Path == prefix || strings.HasPrefix(c.Request.URL.Path, prefix+"/") {
				c.Status(http.StatusNotFound)
				return
			}
		}
		name := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/")
		if name == "" {
			serveIndex(c)
			return
		}
		if f, err := assets.Open(name); err == nil {
			_ = f.Close()
			if strings.HasPrefix(name, "assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				c.Header("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		serveIndex(c)
	})
}
