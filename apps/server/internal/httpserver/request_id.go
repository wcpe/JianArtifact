package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
)

const (
	requestIDContextKey = "request_id"
	requestIDHeader     = "X-Request-ID"
	requestIDBytes      = 16
)

// requestSourceHandler 在路由匹配前建立可信来源，并为路由器直接生成的响应设置请求标识。
func requestSourceHandler(policy auditctx.Policy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := newRequestID()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		req := r.WithContext(auditctx.WithSource(r.Context(), policy.Resolve(r, id)))
		// Gin 的自动跳转会改写 URL；复制它和头映射，保持调用方的请求和正文不变。
		u := *r.URL
		req.URL = &u
		req.Header = r.Header.Clone()
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		// 当前来源策略不支持转发前缀；Gin 会在路由中间件之前读取它。
		req.Header.Del("X-Forwarded-Prefix")
		// 旧审计仍读同名请求头，只向应用提供生成的值。
		req.Header.Set(requestIDHeader, id)
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, req)
	})
}

// requestIDMiddleware 仅从内部上下文向仍依赖 Gin 上下文的调用方提供请求标识。
func requestIDMiddleware(c *gin.Context) {
	source, ok := auditctx.SourceFrom(c.Request.Context())
	if !ok || source.RequestID == "" {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Set(requestIDContextKey, source.RequestID)
	c.Next()
}

func newRequestID() (string, error) {
	var value [requestIDBytes]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// hostFilterMiddleware 只接受本机回环或命中允许访问域名白名单的请求，其余一律 404。
// 白名单读取器返回空列表表示不限制；nil 由调用方决定是否挂载本中间件。
func hostFilterMiddleware(allowedHosts func() []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !hostAllowed(c.Request, allowedHosts) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// hostAllowed 判断请求是否允许：本机回环来源恒放行；
// 否则请求 Host（去端口、小写）须命中允许访问的域名白名单。
func hostAllowed(r *http.Request, allowedHosts func() []string) bool {
	if remoteAddrIsLoopback(r.RemoteAddr) {
		return true
	}
	host := requestHostname(r.Host)
	lower := strings.ToLower(strings.Trim(host, "[]"))
	list := allowedHosts()
	if len(list) == 0 {
		return true
	}
	for _, allowed := range list {
		if lower == strings.ToLower(strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

// remoteAddrIsLoopback 只按服务端看到的 TCP 对端地址判定回环，不信任可伪造的 Host。
func remoteAddrIsLoopback(remoteAddr string) bool {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().Unmap().IsLoopback()
	}
	addr, err := netip.ParseAddr(strings.Trim(remoteAddr, "[]"))
	return err == nil && addr.Unmap().IsLoopback()
}

func requestHostname(hostPort string) string {
	hostPort = strings.TrimSpace(hostPort)
	if host, _, err := net.SplitHostPort(hostPort); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(hostPort, "[]")
}
