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

// spaRoutePrefixes 是允许回退到前端 index.html 的路由**首段**白名单。
// 来源为前端真实路由表 apps/web/src/app/router.tsx，新增前端页面时必须同步登记，
// 否则该页面刷新后会 404。空串代表根路径 "/"（其首段为空）。
//
// 为什么用白名单而不是黑名单：此前 NoRoute 只拒绝协议前缀，其余路径一律回退 index.html，
// 于是 /api/v1/nonexistent、/org/x/maven-metadata.xml、/llms.txt 这类"不像前端页面"的
// 请求也拿到 200 + HTML。AI agent 与工具链会把这段 HTML 当成 XML / JSON 数据解析，
// 进而给出基于幻觉的结论。收窄为白名单后只有前端真实路由才回退，其余一律 404 + JSON。
//
// 为什么静态产物（/assets/*、favicon 等）不登记在白名单里：它们在 NoRoute 中先按真实文件
// 命中即返回（见 mountStatic），根本不走到白名单判定；未命中说明文件确实不存在，此时
// 返回 404 比返回首页 HTML 更正确——浏览器把 HTML 当 JS / CSS 解析只会得到语法错误。
var spaRoutePrefixes = map[string]struct{}{
	"":                {}, // 根路径 "/"
	"setup":           {},
	"login":           {}, // 兼容旧链接，前端重定向到 /repositories
	"p":               {}, // /p/:name 公开链接别名
	"repositories":    {},
	"search":          {},
	"licenses":        {},
	"settings":        {},
	"audit-logs":      {},
	"host-monitoring": {},
	"dashboard":       {},
	"users":           {},
	"user-groups":     {},
	"tokens":          {},
	"migrations":      {},
	"maintenance":     {},
	"security":        {},
}

// spaFirstSegment 取请求路径的首段，用于 SPA 回退白名单判定。
// 先补前导 "/" 再 path.Clean，使 "repositories/x"、"//repositories/x"、"repositories/"
// 等写法都归一到同一首段；Clean 保证结果以 "/" 开头，故切分后至少有 2 段。
func spaFirstSegment(requestPath string) string {
	return strings.SplitN(path.Clean("/"+requestPath), "/", 3)[1]
}

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
	writeFreeze             func() domain.FreezeState // 运行时写入冻结窗口状态读取器（nil 表示不启用）
	managementSecurityAudit SecurityAuditFunc
	incidentWriteLockGate   gin.HandlerFunc
	securityAuditPrincipal  gin.HandlerFunc
	securityAuditProbe      func() api.HealthStatusAuditCollection
	sourcePolicy            auditctx.Policy
	ingressGate             *IngressGate
	protocolMetric          func(*gin.Context)
	allowedHosts            func() []string  // 允许访问的域名白名单（nil 表示不启用限制）
	originToken             originTokenGuard // 回源 Token 校验配置读取器（nil 表示不启用）
}

// Option 配置 Server。
type Option func(*Server)

// WithSourcePolicy 注入显式验证的代理来源策略；不注入则只使用直连事实。
func WithSourcePolicy(policy auditctx.Policy) Option {
	return func(s *Server) { s.sourcePolicy = policy }
}

// WithIngressGate 在路由前登记在途请求，可由多个监听器共享。
func WithIngressGate(gate *IngressGate) Option {
	return func(s *Server) { s.ingressGate = gate }
}

// WithSecurityAuditRead 注入四条审计读取契约的主体解析和公开内存状态。
func WithSecurityAuditRead(principal gin.HandlerFunc, probe func() api.HealthStatusAuditCollection) Option {
	return func(s *Server) {
		s.securityAuditPrincipal, s.securityAuditProbe = principal, probe
	}
}

// WithReadinessCheck 追加一个就绪自检钩子。
func WithReadinessCheck(c ReadinessCheck) Option {
	return func(s *Server) { s.checks = append(s.checks, c) }
}

// WithHandlers 注入实现管理与状态端点的 api.ServerInterface。
func WithHandlers(h api.ServerInterface) Option {
	return func(s *Server) { s.ServerInterface = h }
}

// WithIncidentWriteLockGate 挂载事故写锁入口裁决（覆盖全部写路由，含协议入口）。
func WithIncidentWriteLockGate(g gin.HandlerFunc) Option {
	return func(s *Server) { s.incidentWriteLockGate = g }
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

// WithOriginTokenGuard 注入回源 Token 校验配置读取器（运行时读取最新值）。
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
	c.JSON(http.StatusOK, s.healthStatus(c, api.HealthStatusStatusOk))
}

// GetReadyz 就绪探针：全部就绪自检通过才 200，任一未过返回 503；版本号仅对已认证请求返回。
func (s *Server) GetReadyz(c *gin.Context) {
	for _, check := range s.checks {
		if err := check(); err != nil {
			c.JSON(http.StatusServiceUnavailable, s.healthStatus(c, api.HealthStatusStatusUnavailable))
			return
		}
	}
	c.JSON(http.StatusOK, s.healthStatus(c, api.HealthStatusStatusOk))
}

func (s *Server) healthStatus(c *gin.Context, status api.HealthStatusStatus) api.HealthStatus {
	result := api.HealthStatus{Status: status, Version: s.versionFor(c)}
	if s.securityAuditProbe != nil {
		state := s.securityAuditProbe()
		if !state.Valid() {
			state = api.HealthStatusAuditCollectionUnknown
		}
		result.AuditCollection = &state
	}
	return result
}

// Handler 装配 HTTP 入口和 Gin 路由：契约路由优先，其余交给内嵌前端静态资源
// （带 SPA 回退）。assets 为 nil 时不挂载静态资源（便于测试）。
func (s *Server) Handler(assets fs.FS) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// 可信转发只由来源策略解析，禁用 Gin 的默认代理信任及头回退。
	_ = r.SetTrustedProxies(nil) // 空列表不含需要解析的 CIDR。
	r.ForwardedByClientIP = false
	r.RemoteIPHeaders = nil
	r.Use(gin.Recovery())
	r.Use(requestIDMiddleware)
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
	// 旧审计继续记录请求耗时与白名单头，不读取正文。
	r.Use(newAuthEdgeLimiter().middleware())
	r.Use(auditctx.TimingMiddleware())
	r.Use(auditctx.HeadersMiddleware())
	if s.managementSecurityAudit != nil {
		r.Use(managementSecurityAuditMiddleware(s.managementSecurityAudit))
	}
	if s.writeFreeze != nil {
		r.Use(writeFreezeMiddleware(s.writeFreeze))
	}
	if s.incidentWriteLockGate != nil {
		r.Use(s.incidentWriteLockGate)
	}

	// 路由级中间件链：链首插入「进入业务路由处理」计时（duration_server_ms 口径起点），
	// 必须先于鉴权等业务中间件执行；协议路由与静态回退不经过此链，分段耗时记 0。
	routeMiddlewares := make([]api.MiddlewareFunc, 0, len(s.middlewares)+1)
	routeMiddlewares = append(routeMiddlewares, api.MiddlewareFunc(auditctx.ServerTimingMiddleware()))
	routeMiddlewares = append(routeMiddlewares, s.middlewares...)
	// 只有注入了 handler 才注册契约路由。
	//
	// 为什么必须判断：契约路由的 wrapper 形如 `siw.Handler.Xxx(c)`，若内嵌接口为 nil，
	// **任何**命中契约路径的请求都会 nil pointer panic（被 Recovery 兜成 500 并打一整段堆栈）。
	// 此前无条件注册，装配用例（只测健康探针与静态回退）之所以没暴露，是因为它们只访问
	// /healthz、/readyz —— 这两个由 Server 自己实现，不经过内嵌接口。
	//
	// 这也与 Server 字段注释「未注入时仅健康探针可用」一致：不注册即真正不可用（404），
	// 而不是命中后崩溃。健康探针与就绪探针单独注册，保持无依赖装配可用。
	if s.ServerInterface != nil {
		router := securityAuditReadRouter{IRouter: r, middleware: routeMiddlewares, principal: s.securityAuditPrincipal}
		api.RegisterHandlersWithOptions(router, s, router.options())
	} else {
		r.GET("/healthz", s.GetHealthz)
		r.GET("/readyz", s.GetReadyz)
	}

	if s.protocolRoutes != nil {
		s.protocolRoutes(r)
	}

	if assets != nil {
		s.mountStatic(r, assets)
	}
	var next http.Handler = r
	if s.ingressGate != nil {
		next = s.ingressGate.wrap(next)
	}
	return requestSourceHandler(s.sourcePolicy, next)
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
	// 未命中契约路由且不属于前端页面时的统一出口：JSON 错误体，与 api / auth 的
	// 错误格式保持一致（{"error":{"code":...,"message":...}}），
	// 避免调用方拿到 HTML 后误当数据解析。
	notFound := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		auth.WriteError(c, http.StatusNotFound, "not_found", "资源不存在")
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
		// 先归一化路径：静态查找与白名单判定共用同一份清理结果，
		// 避免「/repositories/../api/v1/x」这类写法让两者得出不同结论。
		cleanPath := path.Clean(c.Request.URL.Path)
		name := strings.TrimPrefix(cleanPath, "/")
		if name == "" {
			serveIndex(c)
			return
		}
		// 真实静态资源查找排在白名单判定**之前**：/assets/*、favicon 等产物的首段
		// 本就不在前端路由表里，若先判白名单它们会全部 404。命中文件即按原有缓存
		// 策略返回，与「首段是否像前端路由」无关。
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
		// 文件不存在：只有首段命中前端路由白名单才回退 index.html。
		if _, ok := spaRoutePrefixes[spaFirstSegment(cleanPath)]; !ok {
			notFound(c)
			return
		}
		serveIndex(c)
	})
}
