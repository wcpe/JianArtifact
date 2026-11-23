package httpserver

import (
	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
)

const securityAuditReadBase = "/api/v1/observability/audit/"

func isSecurityAuditReadRoute(path string) bool {
	switch path {
	case securityAuditReadBase + "collection-health", securityAuditReadBase + "security-events", securityAuditReadBase + "security-events/:eventId", securityAuditReadBase + "security-events/:eventId/objects":
		return true
	default:
		return false
	}
}

// 只为生成器登记的四个读取入口前置授权；路由和参数绑定仍由生成器负责。
type securityAuditReadRouter struct {
	gin.IRouter
	middleware []api.MiddlewareFunc
	principal  gin.HandlerFunc
}

func (r securityAuditReadRouter) GET(path string, handlers ...gin.HandlerFunc) gin.IRoutes {
	if !isSecurityAuditReadRoute(path) {
		return r.IRouter.GET(path, handlers...)
	}
	chain := make([]gin.HandlerFunc, 0, len(r.middleware)+len(handlers)+2)
	if r.principal != nil {
		chain = append(chain, auditctx.ServerTimingMiddleware(), r.principal)
	} else {
		for _, middleware := range r.middleware {
			chain = append(chain, gin.HandlerFunc(middleware))
		}
	}
	chain = append(chain, api.AuthorizeSecurityAuditRead)
	return r.IRouter.GET(path, append(chain, handlers...)...)
}

func (r securityAuditReadRouter) options() api.GinServerOptions {
	return api.GinServerOptions{
		Middlewares: []api.MiddlewareFunc{func(c *gin.Context) {
			if c.Request.Method == "GET" && isSecurityAuditReadRoute(c.FullPath()) {
				return
			}
			for _, middleware := range r.middleware {
				middleware(c)
				if c.IsAborted() {
					return
				}
			}
		}},
		ErrorHandler: func(c *gin.Context, err error, status int) {
			if c.Request.Method == "GET" && isSecurityAuditReadRoute(c.FullPath()) {
				api.WriteSecurityAuditBindingError(c)
				return
			}
			c.JSON(status, gin.H{"msg": err.Error()})
		},
	}
}
