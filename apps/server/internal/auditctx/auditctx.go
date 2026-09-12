// Package auditctx 承载审计事件的请求级上下文：中间件写入、审计写入方读取，
// 以及令牌与请求体的脱敏工具。独立成包以免 api 与 httpserver 相互依赖。
package auditctx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// gin.Context 键：中间件写入，审计写入方读取。两侧只通过本包常量约定，避免包循环依赖。
const (
	// StartKey 记录请求开始时间（time.Time），用于计算处理耗时。
	StartKey = "jianartifact.audit.start"
	// BodyKey 记录脱敏后的请求体预览（string），仅管理写请求。
	BodyKey = "jianartifact.audit.body"
)

// maxBodyPreview 是请求体预览的读取上限：只读取前 4KB 用于脱敏展示。
const maxBodyPreview = 4096

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
	value, ok := c.Get(StartKey)
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
