// Package auditctx 承载审计事件的请求级上下文：中间件写入、审计写入方读取，
// 以及令牌与请求体的脱敏工具。独立成包以免 api 与 httpserver 相互依赖。
package auditctx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// gin.Context 键：中间件写入，审计写入方读取。两侧只通过本包常量约定，避免包循环依赖。
const (
	// StartKey 记录请求开始时间（time.Time），用于计算处理耗时。
	StartKey = "jianartifact.audit.start"
	// BodyKey 记录脱敏后的请求体预览（string），仅管理写请求。
	BodyKey = "jianartifact.audit.body"
	// HeadersKey 记录常用白名单请求头的紧凑 JSON 文本（string），由 HeadersMiddleware 写入。
	HeadersKey = "jianartifact.audit.headers"
	// ServerStartKey 记录进入业务路由处理的起点（time.Time），用于计算单段服务端耗时。
	ServerStartKey = "jianartifact.audit.server.start"
)

// maxBodyPreview 是请求体预览的读取上限：只读取前 4KB 用于脱敏展示。
const maxBodyPreview = 4096

// maxHeaderValueRunes 是单个请求头值的截断上限（字符数）：
// 防止超长头值（如异常客户端塞入的巨型 Referer/X-Forwarded-For）把审计行撑爆。
const maxHeaderValueRunes = 256

// headerAllowlist 是允许入审计列的常用请求头白名单（canonical 名）。
// 只记录这些常用协商/溯源头；Authorization 不在白名单内（认证走 TokenPreview 机制），
// 且任何命中 sensitiveKeys 名单的头名在 CaptureHeaders 中都会被二次拦截，绝不入列。
var headerAllowlist = []string{
	"User-Agent",
	"Referer",
	"Content-Type",
	"Accept",
	"Accept-Language",
	"X-Request-ID",
	"Origin",
	"X-Forwarded-For",
}

// 需要在预览中掩码的敏感键（小写比较）。
var sensitiveKeys = map[string]bool{
	"password":      true,
	"new_password":  true,
	"old_password":  true,
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"secret":        true,
	"client_secret": true,
	"api_key":       true,
	"apikey":        true,
	"authorization": true,
	"credential":    true,
	"private_key":   true,
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
//   - 起点：请求通过全局中间件链（Recovery/请求标识/审计计时/请求体预览/管理安全审计等）
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

// BodyPreviewMiddleware 捕获管理写请求的请求体预览（脱敏、截断），并复原请求体供后续读取。
// 仅对非幂等方法生效，读取上限 4KB；读取失败时静默跳过，不影响业务。
func BodyPreviewMiddleware(isWrite func(method, path string) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isWrite == nil || !isWrite(c.Request.Method, c.Request.URL.Path) || c.Request.Body == nil {
			c.Next()
			return
		}
		head, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyPreview))
		// 无论截断与否都要把已读部分放回，保证后续 handler 能读到完整请求体。
		c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), c.Request.Body))
		if err != nil || len(head) == 0 {
			c.Next()
			return
		}
		if preview := SanitizeBody(head); preview != "" {
			c.Set(BodyKey, preview)
		}
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
		// 多值头（如多个 X-Forwarded-For 段）合并为单行文本后统一截断。
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

// BodyFrom 返回中间件捕获的脱敏请求体预览（未捕获时为空串）。
func BodyFrom(c *gin.Context) string {
	value, ok := c.Get(BodyKey)
	if !ok {
		return ""
	}
	preview, _ := value.(string)
	return preview
}

// TokenPreview 生成认证令牌的脱敏预览（形如 Bearer eyJhbG****1dnM）：
// 保留 scheme 与首尾少量字符，其余掩码；过短或空值不回显原文。
func TokenPreview(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	scheme, token := "", trimmed
	if parts := strings.SplitN(trimmed, " ", 2); len(parts) == 2 {
		scheme = parts[0]
		token = strings.TrimSpace(parts[1])
	}
	if token == "" {
		return ""
	}
	masked := "****"
	switch {
	case len(token) > 10:
		masked = token[:6] + "****" + token[len(token)-4:]
	case scheme != "" && len(token) > 4:
		masked = token[:2] + "****"
	}
	if scheme == "" {
		return masked
	}
	return scheme + " " + masked
}

// SanitizeBody 生成脱敏后的请求体文本：JSON 掩码敏感键并缩进输出；非 JSON 截断为纯文本。
func SanitizeBody(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	// 非 UTF-8（如 GBK 客户端）必须在这里拦下：Go 的 json.Unmarshal 对非法 UTF-8 是
	// 宽容的（把坏字节静默替换为 U+FFFD 而不报错），放行会在前端呈现一片乱码、且
	// 看起来像真实数据——明确标注而非伪装。
	if !utf8.Valid(trimmed) {
		return "（请求体不是合法 UTF-8，预览已省略——请检查客户端请求编码）"
	}
	var parsed any
	if err := json.Unmarshal(trimmed, &parsed); err == nil {
		masked := maskValue(parsed)
		if out, err := json.MarshalIndent(masked, "", "  "); err == nil {
			return truncatePreview(string(out))
		}
	}
	return truncatePreview(string(trimmed))
}

func maskValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if sensitiveKeys[strings.ToLower(key)] {
				result[key] = "****"
				continue
			}
			result[key] = maskValue(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = maskValue(item)
		}
		return result
	default:
		return value
	}
}

func truncatePreview(text string) string {
	if len(text) <= maxBodyPreview {
		return text
	}
	return text[:maxBodyPreview] + "…"
}

// IsManagementWrite 判断是否为需要审计的管理写请求（与方法/路由前缀无关的纯判定）。
func IsManagementWrite(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return strings.HasPrefix(path, "/api/v1/")
}
