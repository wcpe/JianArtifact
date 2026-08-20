package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AuditLogListResponse 是审计日志列表响应（FR-38）。
type AuditLogListResponse struct {
	Items []repository.AuditLogEntry `json:"items"`
	Total int                        `json:"total"`
}

// GetAuditLogs 返回审计日志（分页 + 筛选），仅管理员（FR-38）。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetAuditLogs(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.auditLogs == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "审计日志存储未就绪")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	f := repository.AuditFilter{
		Actor:  c.Query("actor"),
		Action: c.Query("action"),
		Repo:   c.Query("repo"),
		From:   c.Query("from"),
		To:     c.Query("to"),
		Limit:  limit,
		Offset: offset,
	}
	items, err := h.auditLogs.List(f)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	total, err := h.auditLogs.Count(f)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, AuditLogListResponse{Items: items, Total: total})
}

// AuditLog 记录一条审计日志（尽力而为，失败不阻断）；供各写 handler 在成功后调用（FR-38）。
// actor 取当前主体用户名（匿名/无主体为空）；ip 取 X-Forwarded-For 或 RemoteAddr。
func (h *Handlers) AuditLog(c *gin.Context, action, entityType, entityKey, repo, detail, result string) {
	if h.auditLogs == nil {
		return
	}
	actor := ""
	if p, ok := auth.PrincipalFrom(c); ok {
		actor = p.Username
	}
	ip := c.ClientIP()
	_ = h.auditLogs.Insert(repository.AuditLogEntry{
		TS:         time.Now().UTC().Format(time.RFC3339Nano),
		Actor:      actor,
		Action:     action,
		EntityType: entityType,
		EntityKey:  entityKey,
		Repo:       repo,
		Detail:     detail,
		Result:     result,
		IP:         ip,
	})
}
