package httpserver

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// SecurityAuditFunc 记录统一安全审计。参数只允许使用已脱敏的稳定分类与路由模板。
type SecurityAuditFunc func(c *gin.Context, action, entityType, entityKey, repo, detail, result string)

// WithManagementSecurityAudit 为管理写入口的 401、403、503 拒绝注册统一审计。
func WithManagementSecurityAudit(audit SecurityAuditFunc) Option {
	return func(s *Server) { s.managementSecurityAudit = audit }
}

func managementSecurityAuditMiddleware(audit SecurityAuditFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if !isManagementWrite(c.Request.Method, c.Request.URL.Path) || !isSecurityRejection(c.Writer.Status()) {
			return
		}
		entityKey := c.FullPath()
		if entityKey == "" {
			entityKey = "management"
		}
		audit(c, "management.write_rejected", "management", entityKey, "", "status="+strconv.Itoa(c.Writer.Status()), "rejected")
	}
}

func isManagementWrite(method, requestPath string) bool {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return false
	}
	if !strings.HasPrefix(requestPath, "/api/v1/") {
		return false
	}
	// 登录失败不属于本功能的安全审计；普通读取已在上方排除。
	return requestPath != "/api/v1/auth/login"
}

func isSecurityRejection(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusServiceUnavailable
}
