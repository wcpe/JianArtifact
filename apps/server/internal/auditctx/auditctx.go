// Package auditctx 承载可信请求来源与审计辅助信息，独立于业务层和仓储。
package auditctx

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// gin.Context 键：中间件写入，审计写入方读取。两侧只通过本包常量约定，避免包循环依赖。
const (
	// StartKey 记录请求开始时间（time.Time），用于计算处理耗时。
	StartKey = "jianartifact.audit.start"
	// HeadersKey 记录常用白名单请求头的紧凑 JSON 文本（string），由 HeadersMiddleware 写入。
	HeadersKey = "jianartifact.audit.headers"
	// ServerStartKey 记录进入业务路由处理的起点（time.Time），用于计算单段服务端耗时。
	ServerStartKey = "jianartifact.audit.server.start"
)

// maxHeaderValueRunes 限制单个旧审计请求头的字符数。
const maxHeaderValueRunes = 256

// headerAllowlist 是可以写入旧审计快照的标准头；不采集凭据或原始转发声明。
var headerAllowlist = []string{
	"User-Agent",
	"Referer",
	"Content-Type",
	"Accept",
	"Accept-Language",
	"X-Request-ID",
	"Origin",
}

// 旧请求头快照禁止记录的敏感键（小写比较）。
var sensitiveKeys = map[string]bool{
	"password":            true,
	"new_password":        true,
	"old_password":        true,
	"token":               true,
	"access_token":        true,
	"refresh_token":       true,
	"secret":              true,
	"client_secret":       true,
	"api_key":             true,
	"apikey":              true,
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"credential":          true,
	"private_key":         true,
}

// TimingMiddleware 记录请求开始时间；必须在业务处理前注册。
func TimingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(StartKey, time.Now())
		c.Next()
	}
}

// HeadersMiddleware 捕获常用白名单请求头（脱敏过滤、长度截断）为紧凑 JSON 文本，
// 供审计写入方经 HeadersFrom 读取；未命中任何白名单头时不写入键（读取方回退空串）。
func HeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request != nil {
			if captured := CaptureHeaders(c.Request.Header); captured != "" {
				c.Set(HeadersKey, captured)
			}
		}
		c.Next()
	}
}

// ServerTimingMiddleware 记录「进入业务路由处理」的起点，必须挂在契约路由级中间件链
// （RegisterHandlersWithOptions 的 GinServerOptions.Middlewares）的链首。
//
// 口径定义（duration_server_ms 的分段耗时）：
//   - 起点：请求通过全局中间件链（异常恢复、请求标识、审计计时、管理安全审计等）
//     并进入契约路由处理的时刻；
//   - 终点：审计落笔时刻（handler 内调用 AuditLog，约等于业务处理结束）。
//
// 如实说明可区分边界：非契约路由（协议端点/静态回退）不经过本中间件、以及在进入
// 契约路由前被全局中间件直接拒绝的请求，没有起点，分段耗时记 0。
func ServerTimingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ServerStartKey, time.Now())
		c.Next()
	}
}

// DurationMs 返回请求处理耗时（毫秒）；未记录开始时间时返回 0。
func DurationMs(c *gin.Context) int64 {
	return elapsedMs(c, StartKey)
}

// ServerDurationMs 返回单段服务端耗时（毫秒，口径见 ServerTimingMiddleware）；
// 请求未进入业务路由处理（无起点）时返回 0。
func ServerDurationMs(c *gin.Context) int64 {
	return elapsedMs(c, ServerStartKey)
}

// elapsedMs 从指定键读取起点并计算经过的毫秒数；键缺失或非法时返回 0。
func elapsedMs(c *gin.Context, key string) int64 {
	value, ok := c.Get(key)
	if !ok {
		return 0
	}
	start, ok := value.(time.Time)
	if !ok || start.IsZero() {
		return 0
	}
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

// HeadersFrom 返回中间件捕获的常用请求头紧凑 JSON（未捕获时为空串）。
func HeadersFrom(c *gin.Context) string {
	value, ok := c.Get(HeadersKey)
	if !ok {
		return ""
	}
	captured, _ := value.(string)
	return captured
}

// CaptureHeaders 按白名单从请求头中提取常用头，生成紧凑 JSON 文本（空命中返回空串）。
// 安全约束：
//   - 只遍历 headerAllowlist，白名单外的头（尤其 Authorization）永不读取；
//   - 任何命中 sensitiveKeys 的头名（小写比较）一律跳过，作为纵深防御；
//   - 单头值按字符截断到 maxHeaderValueRunes（尾部加省略号），防爆行。
func CaptureHeaders(header http.Header) string {
	captured := make(map[string]string, len(headerAllowlist))
	for _, name := range headerAllowlist {
		// 纵深防御：白名单本身不含敏感头，但名单可能演进，命中即跳过。
		if sensitiveKeys[strings.ToLower(name)] {
			continue
		}
		values := header.Values(name)
		if len(values) == 0 {
			continue
		}
		// 多值协商头合并为单行文本后统一截断。
		joined := strings.Join(values, ", ")
		if strings.EqualFold(name, "Referer") {
			// Referer 的 query/fragment 可能携带签名、一次性码或搜索词；userinfo 也不应进入审计。
			joined = sanitizeReferer(joined)
		}
		if strings.TrimSpace(joined) == "" {
			continue
		}
		captured[name] = truncateHeaderValue(joined)
	}
	if len(captured) == 0 {
		return ""
	}
	// json.Marshal 对 map 按键排序，输出稳定紧凑，便于前后端对照。
	// 编码失败（不可能：键值均为合法 UTF-8 字符串）时返回空串，不阻断请求。
	out, err := json.Marshal(captured)
	if err != nil {
		return ""
	}
	return string(out)
}

// truncateHeaderValue 按字符（rune）截断头值到上限：中文等多字节字符不会被
// 按字节硬切而产生半截 UTF-8。
// sanitizeReferer 只保留 Referer 的 scheme/host/path，移除 userinfo、query 与 fragment，
// 防止 URL 中的签名参数、一次性码或搜索词进入审计头快照；非绝对 URL 直接丢弃。
func sanitizeReferer(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

func truncateHeaderValue(value string) string {
	runes := []rune(value)
	if len(runes) <= maxHeaderValueRunes {
		return value
	}
	return string(runes[:maxHeaderValueRunes]) + "…"
}

// BodyFrom 保留旧调用方式；新请求不读取或保存正文预览。
func BodyFrom(_ *gin.Context) string { return "" }

// TokenPreview 保留旧调用方式；新请求不记录凭据的任何部分。
func TokenPreview(_ string) string { return "" }

// SanitizeBody 保留旧调用方式；新请求不记录任意正文。
func SanitizeBody(_ []byte) string { return "" }

// IsManagementWrite 判断是否为需要审计的管理写请求（与方法/路由前缀无关的纯判定）。
func IsManagementWrite(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return strings.HasPrefix(path, "/api/v1/")
}
