package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// FR-168：事故处置动作——预览/确认执行；执行须近期强认证（step_up 水位）。

// PreviewSecurityAction 预览动作并持久化（幂等键回放，摘要冲突 409）。
func (h *Handlers) PreviewSecurityAction(c *gin.Context) {
	p, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req SecurityActionPreviewRequest
	if !bindJSON(c, &req) {
		return
	}
	targets := make([]domain.SecurityTarget, 0, len(req.Targets))
	for _, tg := range req.Targets {
		var tgt, eff string
		if tg.Target != nil {
			tgt = *tg.Target
		}
		if tg.Effect != nil {
			eff = *tg.Effect
		}
		targets = append(targets, domain.SecurityTarget{Kind: string(tg.Kind), Target: tgt, Effect: eff})
	}
	prev, err := h.incident.PreviewSecurityAction(c.Request.Context(), p.UserID,
		string(req.ActionType), req.Reason, targets, req.IdempotencyKey)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, SecurityActionPreview{
		ActionId: prev.ActionID, ActionType: prev.ActionType,
		ActionDigest: prev.ActionDigest, ExpiresAt: prev.ExpiresAt, Targets: prev.Targets,
	})
}

// ExecuteSecurityAction 确认并执行：先复核动作摘要（防预览漂移），再复核近期强认证水位。
func (h *Handlers) ExecuteSecurityAction(c *gin.Context, actionId string) {
	p, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req SecurityActionExecuteRequest
	if !bindJSON(c, &req) {
		return
	}
	if !h.requireRecentMfa(c, p) {
		return
	}
	actionType, err := h.incident.GetActionType(c.Request.Context(), actionId)
	if err != nil {
		writeDomainErr(c, domain.ErrSecurityConfirmationStale)
		return
	}
	var apply func() (*domain.SecurityActionResult, error)
	switch actionType {
	case "restrict":
		apply = func() (*domain.SecurityActionResult, error) {
			return h.incident.ExecuteSecurityAction(c.Request.Context(), p.UserID, actionId, req.ActionDigest,
				h.incident.ApplyRestriction(actionId))
		}
	case "lift_restriction":
		apply = func() (*domain.SecurityActionResult, error) {
			return h.incident.ExecuteSecurityAction(c.Request.Context(), p.UserID, actionId, req.ActionDigest,
				h.incident.ApplyLiftInstanceLock(actionId))
		}
	default:
		writeDomainErr(c, domain.ErrSecurityConfirmationStale)
		return
	}
	out, err := apply()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]SecurityActionItem, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, SecurityActionItem{Seq: it.Seq, Target: it.Target, Outcome: it.Outcome, Detail: it.Detail})
	}
	c.JSON(http.StatusOK, SecurityActionResult{ActionId: out.ActionID, State: out.State, Items: items})
}

// IncidentWriteLockGate 业务写锁入口裁决：写锁激活时业务/管理写一律 503；
// 只放开具名安全出口（事故处置、强认证、只读查询）。
func (h *Handlers) IncidentWriteLockGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if h.incident == nil {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		// 安全出口：事故处置与强认证链路不放行业务写之外的检查会导致死锁。
		if hasPrefixAny(path, "/api/v1/security/", "/api/v1/auth/mfa", "/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/bootstrap") {
			c.Next()
			return
		}
		repo, prefix := incidentTargetOf(c.Request.Method, path)
		v, err := h.incident.BusinessWriteVerdict(c.Request.Context(), repo, prefix)
		if err != nil {
			c.Next()
			return
		}
		if v.Locked {
			auth.WriteError(c, http.StatusServiceUnavailable, "incident_write_locked", "实例业务写锁生效，仅安全出口可用")
			c.Abort()
			return
		}
		if v.Restricted {
			auth.WriteError(c, http.StatusForbidden, "incident_restricted", "目标受事故限制约束")
			c.Abort()
			return
		}
		c.Next()
	}
}

// incidentTargetOf 从请求目标粗解出仓库名与路径前缀（入口级裁决；
// 权威裁决在最终写事务内，本层只做已知名称的快速拦截）。
func incidentTargetOf(method, path string) (repo, prefix string) {
	if method != http.MethodPut && method != http.MethodPost && method != http.MethodDelete && method != http.MethodPatch {
		return "", ""
	}
	p := strings.TrimPrefix(path, "/")
	if strings.HasPrefix(p, "api/v1/repositories/") {
		rest := strings.TrimPrefix(p, "api/v1/repositories/")
		if i := strings.Index(rest, "/"); i > 0 {
			return rest[:i], rest[i+1:]
		}
		return rest, ""
	}
	if strings.HasPrefix(p, "api/v1/") {
		return "", ""
	}
	// 协议路由：/repository/{repo}/... 形态取第二段为仓库名。
	if strings.HasPrefix(p, "repository/") {
		rest := strings.TrimPrefix(p, "repository/")
		if i := strings.Index(rest, "/"); i > 0 {
			return rest[:i], rest[i+1:]
		}
		return rest, ""
	}
	// 其余协议入口（npm/cargo/oci 等自有挂载）：首段按仓库名试探。
	if i := strings.Index(p, "/"); i > 0 {
		return p[:i], p[i+1:]
	}
	return "", ""
}

func hasPrefixAny(path string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(path) >= len(p) && path[:len(p)] == p {
			return true
		}
	}
	return false
}
