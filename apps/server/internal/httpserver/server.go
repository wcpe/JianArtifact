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
)

// ReadinessCheck 是就绪自检钩子：返回非 nil 错误表示某依赖未就绪。
// 0.1.0 无外部依赖，检查集合为空（恒就绪）；后续版本注入 SQLite / blob 存储等自检。
type ReadinessCheck func() error

// Server 实现契约生成的 api.ServerInterface，并持有版本与就绪检查集合。
// 管理与状态端点由注入的 api.ServerInterface（api.Handlers）经嵌入提供；
// Server 自身覆盖 GetHealthz / GetReadyz，使无依赖装配（如测试）也能提供健康探针。
type Server struct {
	api.ServerInterface // 管理 + 状态 handler（WithHandlers 注入；未注入时仅健康探针可用）

	version        string
	checks         []ReadinessCheck
	middlewares    []api.MiddlewareFunc
	protocolRoutes func(gin.IRouter)
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

// GetHealthz 存活探针：进程存活即 200。
func (s *Server) GetHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, api.HealthStatus{Status: api.Ok, Version: s.version})
}

// GetReadyz 就绪探针：全部就绪自检通过才 200，任一未过返回 503。
func (s *Server) GetReadyz(c *gin.Context) {
	for _, check := range s.checks {
		if err := check(); err != nil {
			c.JSON(http.StatusServiceUnavailable, api.HealthStatus{Status: api.Unavailable, Version: s.version})
			return
		}
	}
	c.JSON(http.StatusOK, api.HealthStatus{Status: api.Ok, Version: s.version})
}

// Handler 装配并返回完整的 gin.Engine：契约路由优先，其余交给内嵌前端静态资源
// （带 SPA 回退）。assets 为 nil 时不挂载静态资源（便于测试）。
func (s *Server) Handler(assets fs.FS) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	api.RegisterHandlersWithOptions(r, s, api.GinServerOptions{Middlewares: s.middlewares})

	if s.protocolRoutes != nil {
		s.protocolRoutes(r)
	}

	if assets != nil {
		s.mountStatic(r, assets)
	}
	return r
}

// mountStatic 把未命中契约路由的 GET/HEAD 请求交给前端静态资源；
// 资源不存在时回退到 index.html，支持前端客户端路由（SPA）。
func (s *Server) mountStatic(r *gin.Engine, assets fs.FS) {
	fileServer := http.FileServer(http.FS(assets))
	serveIndex := func(c *gin.Context) {
		c.Request.URL.Path = "/"
		fileServer.ServeHTTP(c.Writer, c.Request)
	}
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Status(http.StatusNotFound)
			return
		}
		name := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/")
		if name == "" {
			serveIndex(c)
			return
		}
		if f, err := assets.Open(name); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		serveIndex(c)
	})
}
