package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// AuditLogListResponse 是审计日志列表响应（FR-38）。
type AuditLogListResponse struct {
	Items []repository.AuditLogEntry `json:"items"`
	Total int                        `json:"total"`
}

// 用户组相关审计动作与实体类型（FR-36）。
//
// 本阶段只把常量与映射备好（组管理的 HTTP handler 在阶段三落地），
// 让审计读模型在组端点上线前就认得这些动作，避免上线后组操作在审计里
// 全部落进 other / 非高危——那样「谁改了授权组」就查不出来了。
const (
	// EntityTypeUserGroup 是用户组的审计实体类型，与 domain.EntityUserGroup 同值。
	EntityTypeUserGroup = "user_group"

	AuditActionGroupCreate    = "group.create"
	AuditActionGroupUpdate    = "group.update"
	AuditActionGroupDelete    = "group.delete"
	AuditActionGroupMemberAdd = "group.member.add"
	AuditActionGroupMemberRmv = "group.member.remove"
)

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
	// repo 过滤：把仓库名（可能是别名）展开为「主名 ∪ 别名」全集，避免重命名后按当前主名
	// 检索漏掉记在旧名下的历史事件（ADR-0028 要求按仓库维度的检索处理全集）。
	var repos []string
	if name := c.Query("repo"); name != "" {
		names, nameErr := h.resolveRepoNameSet(name)
		if nameErr != nil {
			// 名称不存在：退回字面量，与既有行为一致（筛不到记录），不改判为 404。
			repos = []string{name}
		} else {
			repos = names
		}
	}
	f := repository.AuditFilter{
		Actor:      c.Query("actor"),
		UserID:     c.Query("userId"),
		AuthSource: c.Query("authSource"),
		TokenID:    c.Query("tokenId"),
		Action:     c.Query("action"),
		Repos:      repos,
		Result:     c.Query("result"),
		IP:         c.Query("ip"),
		From:       c.Query("from"),
		To:         c.Query("to"),
		Limit:      limit,
		Offset:     offset,
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
	_ = h.auditLogs.Insert(h.auditLogEntry(c, action, entityType, entityKey, repo, detail, result))
}

func (h *Handlers) auditLogEntry(c *gin.Context, action, entityType, entityKey, repo, detail, result string) repository.AuditLogEntry {
	actor := ""
	var userID, tokenID *int64
	authSource, tokenName := "", ""
	if p, ok := auth.PrincipalFrom(c); ok {
		actor = p.Username
		userID = &p.UserID
		authSource = p.AuthSource
		if p.TokenID > 0 {
			tokenID = &p.TokenID
			tokenName = p.TokenName
		}
	}
	ip := c.ClientIP()
	email := ""
	if p, ok := auth.PrincipalFrom(c); ok {
		email = p.Email
	}
	// 路由模板而非原始路径：不含查询串与具体 ID，符合契约的脱敏要求。
	httpPath := c.FullPath()
	if httpPath == "" {
		httpPath = c.Request.URL.Path
	}
	status := c.Writer.Status()
	if status == 0 {
		status = http.StatusOK
	}
	return repository.AuditLogEntry{
		TS:         time.Now().UTC().Format(time.RFC3339Nano),
		Actor:      actor,
		Action:     action,
		EntityType: entityType,
		EntityKey:  entityKey,
		Repo:       repo,
		Detail:     detail,
		Result:     result,
		IP:         ip,
		UserID:     userID,
		AuthSource: authSource,
		TokenID:    tokenID,
		TokenName:  tokenName,
		UserAgent:  c.GetHeader("User-Agent"),
		RequestID:  requestID(c),
		SourceNode: h.auditSourceNode,

		HTTPMethod:       c.Request.Method,
		HTTPPath:         httpPath,
		StatusCode:       status,
		DurationMs:       auditctx.DurationMs(c),
		TokenPreview:     auditctx.TokenPreview(c.GetHeader("Authorization")),
		BodyPreview:      auditctx.BodyFrom(c),
		HTTPHeaders:      auditctx.HeadersFrom(c),
		DurationServerMs: auditctx.ServerDurationMs(c),
		ActorEmail:       email,
	}
}

func requestID(c *gin.Context) string {
	if value := c.GetHeader("X-Request-ID"); value != "" {
		return value
	}
	if value, ok := c.Get("request_id"); ok {
		if id, ok := value.(string); ok {
			return id
		}
	}
	return ""
}
