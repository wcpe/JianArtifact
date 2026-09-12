package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	requestIDContextKey = "request_id"
	requestIDHeader     = "X-Request-ID"
	requestIDBytes      = 16
)

// requestIDMiddleware 为所有 HTTP 请求统一保留或生成请求标识，供审计与客户端关联同一次请求。
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			generated, err := newRequestID()
			if err != nil {
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
			id = generated
		}
		c.Set(requestIDContextKey, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
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
