package httpserver

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// 认证入口边缘限流（规格口径）：每来源 30 次/5 分钟、实例 200 次/分钟。
// 只覆盖 /api/v1/auth/ 前缀（登录、挑战兑现、认领、恢复），来源取已裁决的
// 客户端 IP（可信代理由外层解析策略限定）；超限返回统一 429 auth_rate_limited
// 并带有界 Retry-After，不回精确配额或存在性。
const (
	authEdgeSourceLimit   = 30
	authEdgeSourceWindow  = 5 * time.Minute
	authEdgeInstanceLimit = 200
	authEdgeInstanceTick  = time.Minute
	authEdgeMaxRetry      = 30 // Retry-After 上界（秒）
)

type fixedWindow struct {
	start time.Time
	count int
}

type authEdgeLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	sources   map[string]*fixedWindow
	instance  fixedWindow
	lastSweep time.Time
}

func newAuthEdgeLimiter() *authEdgeLimiter {
	return &authEdgeLimiter{now: time.Now, sources: map[string]*fixedWindow{}}
}

// allow 计数一次请求并判定是否放行；返回剩余重试秒数（未超限时为 0）。
func (l *authEdgeLimiter) allow(source string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	if now.Sub(l.instance.start) >= authEdgeInstanceTick {
		l.instance = fixedWindow{start: now}
	}
	if l.instance.count >= authEdgeInstanceLimit {
		return false, retryAfter(l.instance.start, authEdgeInstanceTick, now)
	}
	src, ok := l.sources[source]
	if !ok || now.Sub(src.start) >= authEdgeSourceWindow {
		src = &fixedWindow{start: now}
		l.sources[source] = src
	}
	if src.count >= authEdgeSourceLimit {
		return false, retryAfter(src.start, authEdgeSourceWindow, now)
	}
	src.count++
	l.instance.count++
	l.sweepLocked(now)
	return true, 0
}

func retryAfter(start time.Time, window time.Duration, now time.Time) int {
	secs := int(window.Seconds()-now.Sub(start).Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	if secs > authEdgeMaxRetry {
		secs = authEdgeMaxRetry
	}
	return secs
}

// sweepLocked 惰性清理过期来源窗口，防map无限增长（有界计数）。
func (l *authEdgeLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < authEdgeSourceWindow {
		return
	}
	l.lastSweep = now
	for k, w := range l.sources {
		if now.Sub(w.start) >= authEdgeSourceWindow {
			delete(l.sources, k)
		}
	}
}

// authEdgeRateLimit 仅对 /api/v1/auth/ 前缀计数限流；其余路径直通。
func (l *authEdgeLimiter) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, "/api/v1/auth/") {
			c.Next()
			return
		}
		source := c.ClientIP()
		if source == "" {
			source = "unknown"
		}
		ok, retry := l.allow(source)
		if !ok {
			c.Header("Retry-After", itoa(retry))
			auth.WriteError(c, http.StatusTooManyRequests, "auth_rate_limited", "认证尝试过于频繁")
			c.Abort()
			return
		}
		c.Next()
	}
}

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
