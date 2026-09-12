package httpserver

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

// originTokenGuard 读取回源 Token 校验配置：开关、请求头名、Token 值（运行时读取最新值）。
type originTokenGuard func() (enabled bool, header, value string)

// originTokenMiddleware 校验请求是否携带 CDN 回源注入的 Token（FR-130）。
// 未启用时完全放行；启用后，非回环请求必须携带「请求头名: Token值」且完全匹配，否则 404。
// 本机回环（健康检查、本机 CLI）恒放行。与 Host 白名单并存（本中间件在 Host 过滤之后）。
func originTokenMiddleware(guard originTokenGuard) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !originTokenAllowed(c.Request, guard) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// originTokenAllowed 判断请求是否通过回源 Token 校验。
func originTokenAllowed(r *http.Request, guard originTokenGuard) bool {
	enabled, header, value := guard()
	if !enabled {
		return true
	}
	if remoteAddrIsLoopback(r.RemoteAddr) {
		return true
	}
	if header == "" || value == "" {
		return false
	}
	got := r.Header.Get(header)
	return subtle.ConstantTimeCompare([]byte(got), []byte(value)) == 1
}
