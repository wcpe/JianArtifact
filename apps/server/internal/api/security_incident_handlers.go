package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
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
	case "hold_evidence":
		apply = func() (*domain.SecurityActionResult, error) {
			return h.incident.ExecuteSecurityAction(c.Request.Context(), p.UserID, actionId, req.ActionDigest,
				h.incident.ApplyEvidenceHold(actionId))
		}
	case "release_hold":
		apply = func() (*domain.SecurityActionResult, error) {
			return h.incident.ExecuteSecurityAction(c.Request.Context(), p.UserID, actionId, req.ActionDigest,
				h.incident.ApplyReleaseHold(actionId))
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

// ListEvidenceHolds 保全清单（管理员只读）。
func (h *Handlers) ListEvidenceHolds(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	holds, err := h.incident.ListEvidenceHolds(c.Request.Context())
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	out := make([]gin.H, 0, len(holds))
	for _, hd := range holds {
		out = append(out, gin.H{"holdId": hd.HoldID, "digest": hd.Digest, "byteState": hd.ByteState,
			"state": hd.State, "sizeBytes": hd.SizeBytes, "createdAt": hd.CreatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// IncidentWriteLockGate 业务写锁入口裁决：写锁激活时业务/管理写一律 503；
// 只放开具名安全出口（事故处置、强认证、只读查询）。
func (h *Handlers) IncidentWriteLockGate() gin.HandlerFunc {
	return func(c *gin.Context) {
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
		// 恢复隔离中：公网管理/协议/下载一律 503 recovery_isolated（读也拦）。
		if isolated, ierr := h.incident.RecoveryIsolated(c.Request.Context()); ierr == nil && isolated {
			auth.WriteError(c, http.StatusServiceUnavailable, "recovery_isolated", "恢复隔离中，仅受控安全出口可用")
			c.Abort()
			return
		}
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
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

// GetRecoveryGate 恢复闸状态（管理员可读，隔离中仍可查）。
func (h *Handlers) GetRecoveryGate(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	g, err := h.incident.RecoveryGateStatus(c.Request.Context())
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"state": g.State, "restoreId": g.RestoreID, "securityEpoch": g.SecurityEpoch,
		"checkpointStatus": g.CheckpointStatus, "verifiedWatermark": g.VerifiedWatermark,
		"gapStatus": g.GapStatus, "reason": g.Reason})
}

// BeginRecoveryIsolation 恢复启动：normal → isolated。
func (h *Handlers) BeginRecoveryIsolation(c *gin.Context) {
	p, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req BeginRecoveryIsolationJSONRequestBody
	if !bindJSON(c, &req) {
		return
	}
	if !h.requireRecentMfa(c, p) {
		return
	}
	var reason string
	if req.Reason != nil {
		reason = *req.Reason
	}
	if err := h.incident.BeginRestoreIsolation(c.Request.Context(), req.RestoreId, reason); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ReportRecoveryCheckpoint 检查点核对（隔离中放行——核对本身是安全出口）。
func (h *Handlers) ReportRecoveryCheckpoint(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req ReportRecoveryCheckpointJSONRequestBody
	if !bindJSON(c, &req) {
		return
	}
	if err := h.incident.ReportRestoreCheckpoint(c.Request.Context(), req.RestoreId,
		string(req.CheckpointStatus), int64(req.VerifiedWatermark)); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ConfirmRecovery 近期强认证管理员解闸。
func (h *Handlers) ConfirmRecovery(c *gin.Context) {
	p, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.incident == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req ConfirmRecoveryJSONRequestBody
	if !bindJSON(c, &req) {
		return
	}
	if !h.requireRecentMfa(c, p) {
		return
	}
	if err := h.incident.ConfirmRestore(c.Request.Context(), req.RestoreId); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── FR-171 风险调查案件 ────────────────────────────────────────────────────

func caseRowToModel(r *repository.SecurityCaseRow) SecurityCase {
	out := SecurityCase{CaseId: r.CaseID, Title: r.Title, State: SecurityCaseState(r.State),
		Severity:      SecurityCaseSeverity(r.Severity),
		TargetVersion: r.TargetVersion, CreatedUnix: &r.CreatedUnix, UpdatedUnix: &r.UpdatedUnix}
	if r.Resolution != "" {
		out.Resolution = &r.Resolution
	}
	if r.Reason != "" {
		out.Reason = &r.Reason
	}
	if r.OwnerUserID != nil {
		out.OwnerUserId = r.OwnerUserID
	}
	return out
}

func (h *Handlers) ListSecurityCases(c *gin.Context, params ListSecurityCasesParams) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	state := ""
	if params.State != nil {
		state = string(*params.State)
	}
	rows, err := h.incident.ListSecurityCases(c.Request.Context(), state, 200)
	if err != nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "security_unavailable", "案件查询暂不可用")
		return
	}
	out := make([]SecurityCase, 0, len(rows))
	for i := range rows {
		out = append(out, caseRowToModel(&rows[i]))
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handlers) OpenSecurityCase(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	var req SecurityCaseOpenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体无效")
		return
	}
	var links []repository.SecurityCaseLinkRow
	if req.EventLinks != nil {
		for _, l := range *req.EventLinks {
			links = append(links, repository.SecurityCaseLinkRow{EventID: l.EventId, LinkKind: string(l.LinkKind)})
		}
	}
	row, err := h.incident.OpenSecurityCase(c.Request.Context(), p, req.Title, string(req.Severity), req.Reason, links)
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "建案参数无效")
			return
		}
		auth.WriteError(c, http.StatusServiceUnavailable, "security_unavailable", "建案暂不可用")
		return
	}
	c.JSON(http.StatusOK, caseRowToModel(row))
}

func (h *Handlers) TransitionSecurityCase(c *gin.Context, caseID string) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	var req SecurityCaseTransitionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "请求体无效")
		return
	}
	var reason, resCode string
	if req.Reason != nil {
		reason = *req.Reason
	}
	if req.ResolutionCode != nil {
		resCode = *req.ResolutionCode
	}
	row, err := h.incident.TransitionSecurityCase(c.Request.Context(), p, caseID, string(req.Action), reason, resCode, req.TargetVersion)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrNotFound):
			auth.WriteError(c, http.StatusNotFound, "security_case_not_found", "案件不存在")
		case errors.Is(err, domain.ErrValidation):
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "转态参数无效")
		case errors.Is(err, domain.ErrSecurityConfirmationStale):
			auth.WriteError(c, http.StatusConflict, "security_confirmation_stale", "目标版本漂移")
		default:
			auth.WriteError(c, http.StatusServiceUnavailable, "security_unavailable", "转态暂不可用")
		}
		return
	}
	c.JSON(http.StatusOK, caseRowToModel(row))
}

func (h *Handlers) ListSecurityCaseLinks(c *gin.Context, caseID string) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	links, err := h.incident.SecurityCaseLinks(c.Request.Context(), caseID)
	if err != nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "security_unavailable", "关联查询暂不可用")
		return
	}
	out := make([]SecurityCaseLink, 0, len(links))
	for _, l := range links {
		out = append(out, SecurityCaseLink{CaseId: l.CaseID, EventId: l.EventID, LinkKind: SecurityCaseLinkLinkKind(l.LinkKind), LinkedBy: &l.LinkedBy, LinkedUnix: &l.LinkedUnix})
	}
	c.JSON(http.StatusOK, out)
}

// ── FR-172 站内安全通知 ────────────────────────────────────────────────────

func (h *Handlers) ListSecurityNotifications(c *gin.Context, params ListSecurityNotificationsParams) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	var after, limit int64 = 0, 50
	if params.After != nil {
		after = *params.After
	}
	if params.Limit != nil {
		limit = int64(*params.Limit)
	}
	page, err := h.incident.ListSecurityNotifications(c.Request.Context(), p, after, int(limit))
	if err != nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "notification_projection_unavailable", "通知投影暂不可用")
		return
	}
	type noticeItem = struct {
		Acked       *bool   `json:"acked,omitempty"`
		CreatedUnix *int64  `json:"createdUnix,omitempty"`
		Kind        *string `json:"kind,omitempty"`
		NoticeId    *string `json:"noticeId,omitempty"`
		Payload     *string `json:"payload,omitempty"`
		Seq         *int64  `json:"seq,omitempty"`
		Title       *string `json:"title,omitempty"`
	}
	items := make([]noticeItem, 0, len(page.Items))
	for _, it := range page.Items {
		acked := page.Acked[it.Seq]
		items = append(items, noticeItem{Seq: &it.Seq, NoticeId: &it.NoticeID, Kind: &it.Kind,
			Title: &it.Title, Payload: &it.Payload, CreatedUnix: &it.CreatedUnix, Acked: &acked})
	}
	out := SecurityNotificationPage{Items: items, HasMore: page.HasMore, Watermark: page.Watermark}
	if page.NextSeq > 0 {
		out.NextCursor = &page.NextSeq
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handlers) AckSecurityNotification(c *gin.Context, noticeID string) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	ackAt, err := h.incident.AckSecurityNotification(c.Request.Context(), p, noticeID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			auth.WriteError(c, http.StatusNotFound, "notification_not_found", "通知不存在或不可见")
			return
		}
		auth.WriteError(c, http.StatusServiceUnavailable, "notification_projection_unavailable", "确认暂不可用")
		return
	}
	c.JSON(http.StatusOK, map[string]int64{"ackAt": ackAt.Unix()})
}

func (h *Handlers) ListNotificationDeliveries(c *gin.Context, params ListNotificationDeliveriesParams) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	limit := 100
	if params.Limit != nil {
		limit = int(*params.Limit)
	}
	rows, err := h.incident.ListNotificationDeliveries(c.Request.Context(), p, limit)
	if err != nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "notification_projection_unavailable", "投递查询暂不可用")
		return
	}
	out := make([]SecurityNotificationDelivery, 0, len(rows))
	for _, r := range rows {
		d := SecurityNotificationDelivery{DeliveryId: r.DeliveryID, Channel: SecurityNotificationDeliveryChannel(r.Channel),
			TargetMasked: r.TargetMasked, State: SecurityNotificationDeliveryState(r.State), Attempts: &r.Attempts}
		if r.LastError != "" {
			d.LastError = &r.LastError
		}
		out = append(out, d)
	}
	c.JSON(http.StatusOK, out)
}

// ListSecurityFindings は規則検出命中を新しい順で返す（FR-171）。
func (h *Handlers) ListSecurityFindings(c *gin.Context, params ListSecurityFindingsParams) {
	c.Header("Cache-Control", "private, no-store")
	p, ok := requireAdmin(c)
	if !ok || !h.requireRecentMfa(c, p) {
		return
	}
	if h.finding == nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "security_projection_unavailable", "検出投影未装配")
		return
	}
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	items, err := h.finding.ListFindings(c.Request.Context(), p, limit)
	if err != nil {
		auth.WriteError(c, http.StatusServiceUnavailable, "security_projection_unavailable", "検出投影暂不可用")
		return
	}
	out := make([]SecurityFinding, 0, len(items))
	for _, f := range items {
		item := SecurityFinding{
			FindingId: f.FindingID, RuleId: f.RuleID, RuleVersion: f.RuleVersion,
			Severity:   SecurityFindingSeverity(f.Severity),
			SubjectKey: f.SubjectKey, WindowFrom: &f.WindowFrom, WindowTo: &f.WindowTo,
			Summary: f.Summary, CreatedUnix: &f.CreatedUnix,
		}
		if f.Confidence != "" {
			c := SecurityFindingConfidence(f.Confidence)
			item.Confidence = &c
		}
		if f.CaseID != "" {
			item.CaseId = &f.CaseID
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, out)
}
