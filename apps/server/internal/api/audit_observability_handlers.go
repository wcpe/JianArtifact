package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const (
	auditSource       = "audit"
	replicationSource = "replication"
	defaultAuditLimit = 50
	maxAuditLimit     = 100
	maxAuditRange     = 30 * 24 * time.Hour
	// maxAuditAggregationEvents 是**聚合读**（概览 KPI/趋势、关注批次列表、通知）的事件上限。
	// 聚合走窄投影（repository.observabilityAggregateSelect，不含 detail/body_preview/user_agent
	// 这些大字段），同内存预算下能覆盖的事件量比全量投影高一个数量级，因此这里设 5 万：
	// 线上一个活跃节点 24h 的审计事件在万级（实测：上线第 3 天 24h 已达 5900 条），
	// 原来按全量投影设 5000 会让**默认的 24h 视图**直接 409「审计聚合范围过大」。
	maxAuditAggregationEvents = 50000
	// maxAuditAttentionEvents 是**关注批次详情**的上限：那条路径会把事件原样序列化给前端
	// （含 detail/body_preview/user_agent），用全量投影，故维持较低上限；且其查询范围
	// 已被限定在单个关注批次内，正常远达不到。
	maxAuditAttentionEvents = 5000
	auditAttentionAction    = "audit.attention_acknowledge"
)

type auditReadSnapshot struct {
	AuditMaxID       int64           `json:"a"`
	ReplicationMaxID int64           `json:"r"`
	From             string          `json:"f"`
	To               string          `json:"t"`
	SourceNode       string          `json:"n"`
	CapturedAt       string          `json:"c"`
	Categories       []AuditCategory `json:"k,omitempty"`
	Results          []AuditResult   `json:"u,omitempty"`
	Actor            string          `json:"x,omitempty"`
	Repository       string          `json:"p,omitempty"`
	// 新增检索维度同样纳入指纹：概览签发的 snapshot 只能与相同筛选共同使用。
	Query      string `json:"q,omitempty"`
	Method     string `json:"hm,omitempty"`
	Action     string `json:"an,omitempty"`
	Email      string `json:"ae,omitempty"`
	ClientIP   string `json:"ci,omitempty"`
	AuthSource string `json:"as,omitempty"`
	Attention  string `json:"at,omitempty"`
}

type auditAttentionPayload struct {
	Snapshot auditReadSnapshot `json:"s"`
	GroupKey string            `json:"g"`
	Bucket   string            `json:"b"`
}

type auditViewFilter struct {
	from       time.Time
	to         time.Time
	categories map[AuditCategory]bool
	results    map[AuditResult]bool
	actor      string
	repository string
	query      string
	method     string
	action     string
	email      string
	clientIP   string
	authSource string
	attention  string
}

type auditAttentionGroup struct {
	payload auditAttentionPayload
	events  []repository.ObservabilityEvent
}

func (s auditReadSnapshot) capturedAt() time.Time {
	at, err := time.Parse(time.RFC3339Nano, s.CapturedAt)
	if err != nil {
		return time.Time{}
	}
	return at
}

func (s auditReadSnapshot) rangeDuration() time.Duration {
	from, fromErr := time.Parse(time.RFC3339Nano, s.From)
	to, toErr := time.Parse(time.RFC3339Nano, s.To)
	if fromErr != nil || toErr != nil || !to.After(from) {
		return 24 * time.Hour
	}
	return to.Sub(from)
}

// GetAuditObservabilitySummary 返回当前节点的安全审计概览和稳定快照。
func (h *Handlers) GetAuditObservabilitySummary(c *gin.Context, params GetAuditObservabilitySummaryParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	filter, ok := auditSummaryFilter(c, params)
	if !ok {
		return
	}
	snapshot, events, ok := h.loadAuditSnapshot(c, filter, "")
	if !ok {
		return
	}
	summary := auditSummary(events, filter)
	summary.Snapshot = h.signSnapshot(snapshot)
	summary.SnapshotAt = snapshot.capturedAt()
	c.JSON(http.StatusOK, summary)
}

// ListAuditObservabilityEvents 返回快照内逐条、脱敏的统一审计事件。
func (h *Handlers) ListAuditObservabilityEvents(c *gin.Context, params ListAuditObservabilityEventsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	filter, ok := auditEventsFilter(c, params)
	if !ok {
		return
	}
	snapshotValue := ""
	if params.Snapshot != nil {
		snapshotValue = *params.Snapshot
	}
	snapshot, ok := h.loadAuditReadSnapshot(c, filter, snapshotValue)
	if !ok {
		return
	}
	limit, cursor, ok := auditPageParams(c, params.Limit, params.Cursor)
	if !ok {
		return
	}
	offset := cursorOffset(cursor)
	// 显式 offset 优先于 cursor：分页器可直接跳到任意页。
	if params.Offset != nil {
		if *params.Offset < 0 {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "offset 必须为非负整数")
			return
		}
		offset = int(*params.Offset)
	}
	events, total, nextOffset, err := h.auditObservability.ListEventPage(repositoryObservabilityFilter(snapshot, filter), offset, limit)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	acknowledgements, err := h.auditObservability.Acknowledgements(events)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]AuditEvent, 0, len(events))
	for _, event := range events {
		items = append(items, h.toAPIAuditEvent(snapshot, event, acknowledgements))
	}
	response := AuditEventPage{Items: items, Snapshot: h.signSnapshot(snapshot), SnapshotAt: snapshot.capturedAt(), TotalCount: total}
	if nextOffset != nil {
		next := strconv.Itoa(*nextOffset)
		response.NextCursor = &next
	}
	c.JSON(http.StatusOK, response)
}

// ListAuditAttentions 返回同一审计快照内的风险关注批次分页。
func (h *Handlers) ListAuditAttentions(c *gin.Context, params ListAuditAttentionsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	filter, ok := auditAttentionsFilter(c, params)
	if !ok {
		return
	}
	snapshotValue := ""
	if params.Snapshot != nil {
		snapshotValue = *params.Snapshot
	}
	snapshot, events, ok := h.loadAuditSnapshot(c, filter, snapshotValue)
	if !ok {
		return
	}
	limit, cursor, ok := auditPageParams(c, params.Limit, params.Cursor)
	if !ok {
		return
	}
	groups, acknowledgements, err := h.auditGroups(snapshot, events, filter)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]AuditAttention, 0, len(groups))
	for _, group := range groups {
		items = append(items, h.toAPIAttention(group.payload, group, acknowledgements))
	}
	sort.Slice(items, func(i, j int) bool {
		return notificationLess(notificationPreview(items[i]), notificationPreview(items[j]))
	})
	start := cursorOffset(cursor)
	if start > len(items) {
		start = len(items)
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	response := AuditAttentionPage{
		Items:      items[start:end],
		Snapshot:   h.signSnapshot(snapshot),
		SnapshotAt: snapshot.capturedAt(),
		TotalCount: len(items),
	}
	if end < len(items) {
		next := strconv.Itoa(end)
		response.NextCursor = &next
	}
	c.JSON(http.StatusOK, response)
}

// GetAuditObservabilityEvent 返回单条脱敏事件详情；完整诊断字段永不回显。
func (h *Handlers) GetAuditObservabilityEvent(c *gin.Context, eventID AuditEventIdParam) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	source, id, err := h.parseEventID(string(eventID))
	if err != nil {
		auth.WriteError(c, http.StatusNotFound, "not_found", "审计事件不存在")
		return
	}
	if h.auditObservability == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "统一审计存储未就绪")
		return
	}
	event, err := h.auditObservability.EventByID(source, id, h.auditSourceNode)
	if errors.Is(err, repository.ErrNotFound) {
		auth.WriteError(c, http.StatusNotFound, "not_found", "审计事件不存在")
		return
	}
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	from := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	snapshot := auditReadSnapshot{AuditMaxID: maxID(source, id, auditSource), ReplicationMaxID: maxID(source, id, replicationSource), From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), SourceNode: h.auditSourceNode}
	acknowledgements, err := h.auditObservability.Acknowledgements([]repository.ObservabilityEvent{*event})
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	apiEvent := h.toAPIAuditEvent(snapshot, *event, acknowledgements)
	details := AuditEventSafeDetails{AffectedCount: 1, ResultSummary: apiEvent.Summary}
	if event.ErrorClass != "" {
		errClass := event.ErrorClass
		details.ErrorClass = &errClass
	}
	c.JSON(http.StatusOK, AuditEventDetail{Event: apiEvent, Details: details})
}

// GetAuditAttention 返回服务端关注标识绑定的完整批次安全摘要和成员分页。
func (h *Handlers) GetAuditAttention(c *gin.Context, attentionID AuditAttentionIdParam, params GetAuditAttentionParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	payload, group, acknowledgements, ok := h.loadAttention(c, string(attentionID))
	if !ok {
		return
	}
	limit, cursor, ok := auditPageParams(c, params.Limit, params.Cursor)
	if !ok {
		return
	}
	start := cursorOffset(cursor)
	if start > len(group.events) {
		start = len(group.events)
	}
	end := start + limit
	if end > len(group.events) {
		end = len(group.events)
	}
	items := make([]AuditEvent, 0, end-start)
	for _, event := range group.events[start:end] {
		items = append(items, h.toAPIAuditEvent(payload.Snapshot, event, acknowledgements))
	}
	attention := h.toAPIAttention(payload, group, acknowledgements)
	response := AuditAttentionDetail{Attention: attention, Items: items, TotalCount: len(group.events)}
	if end < len(group.events) {
		next := strconv.Itoa(end)
		response.NextCursor = &next
	}
	c.JSON(http.StatusOK, response)
}

// AcknowledgeAuditAttention 以 attentionId 绑定的完整风险成员为单位执行原子、幂等确认。
func (h *Handlers) AcknowledgeAuditAttention(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	var request AcknowledgeAuditAttentionRequest
	if !bindJSON(c, &request) || strings.TrimSpace(request.AttentionId) == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "attentionId 必填")
		return
	}
	payload, group, _, ok := h.loadAttention(c, request.AttentionId)
	if !ok {
		return
	}
	riskEvents := riskEvents(group.events)
	if len(riskEvents) == 0 {
		auth.WriteError(c, http.StatusConflict, "attention_stale", "风险关注批次不可重建")
		return
	}
	ack, newly, err := h.auditObservability.AcknowledgeAndAudit(
		riskEvents,
		&principal.UserID,
		principal.Username,
		principal.AuthSource,
		time.Now().UTC().Format(time.RFC3339Nano),
		h.auditLogEntry(c, auditAttentionAction, "audit", payload.GroupKey, "", "attentionId=recorded", "ok"),
	)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	first := toAPIAcknowledgement(ack)
	response := AcknowledgeAuditAttentionResponse{AttentionId: request.AttentionId, FirstAcknowledgement: first, NewlyAcknowledgedCount: newly, TotalRiskEventCount: len(riskEvents), UnacknowledgedRiskEventCount: 0}
	// 审计确认本身不是风险事件，且在旧快照之外，不会递归进入当前确认批次。
	c.JSON(http.StatusOK, response)
}

// ListAuditAttentionNotifications 返回当前节点风险通知批次分页（FR-117）。
// 缺省请求保持页眉口径：最近 24 小时未确认风险、失败优先排序、最多 20 条预览并返回
// totalUnacknowledged；显式携带 from/to/status/limit/cursor 时按 status 口径分页并按最新时间排序。
func (h *Handlers) ListAuditAttentionNotifications(c *gin.Context, params ListAuditAttentionNotificationsParams) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	explicit := params.From != nil || params.To != nil || params.Status != nil || params.Limit != nil || params.Cursor != nil
	from, to, ok := notificationRange(c, params)
	if !ok {
		return
	}
	status := AuditNotificationStatusUnacknowledged
	if params.Status != nil {
		status = *params.Status
		if !status.Valid() {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "status 无效")
			return
		}
	}
	limit := 20
	if params.Limit != nil {
		limit = *params.Limit
		if limit < 1 || limit > maxAuditLimit {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "limit 须在 1 到 100 之间")
			return
		}
	}
	offset := 0
	if params.Cursor != nil {
		offset = cursorOffset(string(*params.Cursor))
		if offset < 0 {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "cursor 无效")
			return
		}
	}
	filter := auditViewFilter{from: from, to: to}
	snapshot, events, ok := h.loadAuditSnapshot(c, filter, "")
	if !ok {
		return
	}
	groups, acknowledgements, err := h.auditGroups(snapshot, events, filter)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]AuditAttentionPreview, 0, len(groups))
	for _, group := range groups {
		attention := h.toAPIAttention(group.payload, group, acknowledgements)
		switch status {
		case AuditNotificationStatusUnacknowledged:
			if attention.UnacknowledgedRiskEventCount == 0 {
				continue
			}
		case AuditNotificationStatusAcknowledged:
			if attention.UnacknowledgedRiskEventCount > 0 {
				continue
			}
		case AuditNotificationStatusAll:
		}
		items = append(items, notificationPreview(attention))
	}
	if explicit {
		sort.Slice(items, func(i, j int) bool { return notificationTimeLess(items[i], items[j]) })
	} else {
		sort.Slice(items, func(i, j int) bool { return notificationLess(items[i], items[j]) })
	}
	response := AuditAttentionNotificationList{Items: items, Total: len(items)}
	if explicit {
		start := offset
		if start > len(items) {
			start = len(items)
		}
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		response.Items = items[start:end]
		if end < len(items) {
			next := strconv.Itoa(end)
			response.NextCursor = &next
			response.HasMore = true
		}
	} else {
		total := response.Total
		response.TotalUnacknowledged = &total
		if len(response.Items) > 20 {
			response.Items = response.Items[:20]
			response.HasMore = true
		}
	}
	c.JSON(http.StatusOK, response)
}

// notificationRange 解析通知查询的时间窗口：缺省最近 24 小时，显式提供时上限 30 天。
func notificationRange(c *gin.Context, params ListAuditAttentionNotificationsParams) (time.Time, time.Time, bool) {
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now
	if (params.From == nil) != (params.To == nil) {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "from 与 to 必须同时提供")
		return from, to, false
	}
	if params.From != nil {
		from, to = (*time.Time)(params.From).UTC(), (*time.Time)(params.To).UTC()
	}
	if !to.After(from) || to.Sub(from) > maxAuditRange {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "时间范围无效或超过 30 天")
		return from, to, false
	}
	return from, to, true
}

func (h *Handlers) loadAuditSnapshot(c *gin.Context, filter auditViewFilter, raw string) (auditReadSnapshot, []repository.ObservabilityEvent, bool) {
	snapshot, ok := h.loadAuditReadSnapshot(c, filter, raw)
	if !ok {
		return auditReadSnapshot{}, nil, false
	}
	// 聚合读取窄投影：这些事件只用于统计与分组，不进前端（见 ListAggregateEventsLimited）。
	events, tooLarge, err := h.auditObservability.ListAggregateEventsLimited(repositoryObservabilityFilter(snapshot, filter), maxAuditAggregationEvents)
	if err != nil {
		writeDomainErr(c, err)
		return auditReadSnapshot{}, nil, false
	}
	if tooLarge {
		auth.WriteError(c, http.StatusConflict, "audit_query_too_large", "审计聚合范围过大，请缩小时间范围或筛选条件")
		return auditReadSnapshot{}, nil, false
	}
	return snapshot, events, true
}

func (h *Handlers) loadAuditReadSnapshot(c *gin.Context, filter auditViewFilter, raw string) (auditReadSnapshot, bool) {
	if h.auditObservability == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "统一审计存储未就绪")
		return auditReadSnapshot{}, false
	}
	snapshot := auditReadSnapshot{}
	if raw != "" {
		var err error
		snapshot, err = h.parseSnapshot(raw)
		if err != nil || snapshot.SourceNode != h.auditSourceNode || !snapshot.matchesFilter(filter) {
			auth.WriteError(c, http.StatusConflict, "attention_stale", "审计快照已失效，请刷新")
			return auditReadSnapshot{}, false
		}
	} else {
		auditMax, replicationMax, err := h.auditObservability.SnapshotBoundary(h.auditSourceNode)
		if err != nil {
			writeDomainErr(c, err)
			return auditReadSnapshot{}, false
		}
		snapshot = newAuditReadSnapshot(auditMax, replicationMax, h.auditSourceNode, filter)
	}
	return snapshot, true
}

func (h *Handlers) loadAttention(c *gin.Context, attentionID string) (auditAttentionPayload, auditAttentionGroup, map[string]repository.AuditAttentionAck, bool) {
	payload, err := h.parseAttention(attentionID)
	if err != nil || payload.Snapshot.SourceNode != h.auditSourceNode {
		auth.WriteError(c, http.StatusConflict, "attention_stale", "风险关注批次已失效，请刷新")
		return auditAttentionPayload{}, auditAttentionGroup{}, nil, false
	}
	filter, valid := auditFilterFromSnapshot(payload.Snapshot)
	if !valid {
		auth.WriteError(c, http.StatusConflict, "attention_stale", "风险关注批次已失效，请刷新")
		return auditAttentionPayload{}, auditAttentionGroup{}, nil, false
	}
	// 关注批次详情：事件会原样进前端（含 detail / body_preview），用全量投影 + 较低上限。
	events, tooLarge, err := h.auditObservability.ListEventsLimited(repositoryObservabilityFilter(payload.Snapshot, filter), maxAuditAttentionEvents)
	if err != nil {
		writeDomainErr(c, err)
		return auditAttentionPayload{}, auditAttentionGroup{}, nil, false
	}
	if tooLarge {
		auth.WriteError(c, http.StatusConflict, "audit_query_too_large", "风险批次范围过大，请缩小时间范围或筛选条件")
		return auditAttentionPayload{}, auditAttentionGroup{}, nil, false
	}
	groups, acknowledgements, err := h.auditGroups(payload.Snapshot, events, filter)
	if err != nil {
		writeDomainErr(c, err)
		return auditAttentionPayload{}, auditAttentionGroup{}, nil, false
	}
	group, found := groups[payload.GroupKey]
	if !found || group.payload.Bucket != payload.Bucket || len(riskEvents(group.events)) == 0 {
		auth.WriteError(c, http.StatusConflict, "attention_stale", "风险关注批次不可重建，请刷新")
		return auditAttentionPayload{}, auditAttentionGroup{}, nil, false
	}
	return payload, group, acknowledgements, true
}

func auditFilterFromSnapshot(snapshot auditReadSnapshot) (auditViewFilter, bool) {
	from, fromErr := time.Parse(time.RFC3339Nano, snapshot.From)
	to, toErr := time.Parse(time.RFC3339Nano, snapshot.To)
	if fromErr != nil || toErr != nil || !to.After(from) || to.Sub(from) > maxAuditRange {
		return auditViewFilter{}, false
	}
	filter := auditViewFilter{
		from:       from,
		to:         to,
		actor:      snapshot.Actor,
		repository: snapshot.Repository,
	}
	for _, category := range snapshot.Categories {
		if !category.Valid() {
			return auditViewFilter{}, false
		}
		if filter.categories == nil {
			filter.categories = make(map[AuditCategory]bool, len(snapshot.Categories))
		}
		filter.categories[category] = true
	}
	for _, result := range snapshot.Results {
		if !result.Valid() {
			return auditViewFilter{}, false
		}
		if filter.results == nil {
			filter.results = make(map[AuditResult]bool, len(snapshot.Results))
		}
		filter.results[result] = true
	}
	return filter, true
}

func newAuditReadSnapshot(auditMax, replicationMax int64, sourceNode string, filter auditViewFilter) auditReadSnapshot {
	return auditReadSnapshot{
		AuditMaxID:       auditMax,
		ReplicationMaxID: replicationMax,
		From:             filter.from.Format(time.RFC3339Nano),
		To:               filter.to.Format(time.RFC3339Nano),
		SourceNode:       sourceNode,
		CapturedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		Categories:       sortedAuditCategories(filter.categories),
		Results:          sortedAuditResults(filter.results),
		Actor:            filter.actor,
		Repository:       filter.repository,
		Query:            filter.query,
		Method:           filter.method,
		Action:           filter.action,
		Email:            filter.email,
		ClientIP:         filter.clientIP,
		AuthSource:       filter.authSource,
		Attention:        filter.attention,
	}
}

func (s auditReadSnapshot) matchesFilter(filter auditViewFilter) bool {
	return s.From == filter.from.Format(time.RFC3339Nano) &&
		s.To == filter.to.Format(time.RFC3339Nano) &&
		s.Actor == filter.actor &&
		s.Repository == filter.repository &&
		s.Query == filter.query &&
		s.Method == filter.method &&
		s.Action == filter.action &&
		s.Email == filter.email &&
		s.ClientIP == filter.clientIP &&
		s.AuthSource == filter.authSource &&
		s.Attention == filter.attention &&
		slices.Equal(s.Categories, sortedAuditCategories(filter.categories)) &&
		slices.Equal(s.Results, sortedAuditResults(filter.results))
}

func sortedAuditCategories(values map[AuditCategory]bool) []AuditCategory {
	items := make([]AuditCategory, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Slice(items, func(i, j int) bool { return items[i] < items[j] })
	return items
}

func sortedAuditResults(values map[AuditResult]bool) []AuditResult {
	items := make([]AuditResult, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Slice(items, func(i, j int) bool { return items[i] < items[j] })
	return items
}

func repositoryObservabilityFilter(snapshot auditReadSnapshot, filter auditViewFilter) repository.ObservabilityFilter {
	categories := make([]string, 0, len(filter.categories))
	for category := range filter.categories {
		categories = append(categories, string(category))
	}
	results := make([]string, 0, len(filter.results))
	for result := range filter.results {
		results = append(results, string(result))
	}
	sort.Strings(categories)
	sort.Strings(results)
	return repository.ObservabilityFilter{
		From:             snapshot.From,
		To:               snapshot.To,
		AuditMaxID:       snapshot.AuditMaxID,
		ReplicationMaxID: snapshot.ReplicationMaxID,
		SourceNode:       snapshot.SourceNode,
		Categories:       categories,
		Results:          results,
		Actor:            filter.actor,
		Repository:       filter.repository,
		Query:            filter.query,
		Method:           filter.method,
		Action:           filter.action,
		Email:            filter.email,
		ClientIP:         filter.clientIP,
		AuthSource:       filter.authSource,
		Attention:        filter.attention,
	}
}

func auditSummaryFilter(c *gin.Context, params GetAuditObservabilitySummaryParams) (auditViewFilter, bool) {
	method := auditOptionalParam(params.Method)
	attention := auditOptionalParam(params.Attention)
	return auditFilter(c, auditFilterInput{
		from: params.From, to: params.To, categories: params.Category, results: params.Result,
		actor: params.Actor, repository: params.Repository, query: params.Q,
		method: method, action: params.Action, email: params.ActorEmail,
		clientIP: params.ClientIp, authSource: params.AuthSource, attention: attention,
	})
}

func auditEventsFilter(c *gin.Context, params ListAuditObservabilityEventsParams) (auditViewFilter, bool) {
	method := auditOptionalParam(params.Method)
	attention := auditOptionalParam(params.Attention)
	return auditFilter(c, auditFilterInput{
		from: params.From, to: params.To, categories: params.Category, results: params.Result,
		actor: params.Actor, repository: params.Repository, query: params.Q,
		method: method, action: params.Action, email: params.ActorEmail,
		clientIP: params.ClientIp, authSource: params.AuthSource, attention: attention,
	})
}

func auditAttentionsFilter(c *gin.Context, params ListAuditAttentionsParams) (auditViewFilter, bool) {
	return auditFilter(c, auditFilterInput{
		from: params.From, to: params.To, categories: params.Category, results: params.Result,
		actor: params.Actor, repository: params.Repository,
	})
}

// auditOptionalParam 把端点枚举指针转成归一字符串（nil 保持 nil）；与包内既有的
// optionalString(string) 区分命名，避免重名与类型推断冲突。
func auditOptionalParam[T ~string](value *T) *string {
	if value == nil {
		return nil
	}
	converted := string(*value)
	return &converted
}

// auditFilterInput 汇总各端点共享的筛选参数；方法/批次状态用字符串归一，避免端点枚举类型耦合。
type auditFilterInput struct {
	from       *AuditFromParam
	to         *AuditToParam
	categories *AuditCategoryParam
	results    *AuditResultParam
	actor      *AuditActorParam
	repository *AuditRepositoryParam
	query      *AuditQueryParam
	method     *string
	action     *AuditActionParam
	email      *AuditActorEmailParam
	clientIP   *AuditClientIpParam
	authSource *AuditAuthSourceParam
	attention  *string
}

// 检索字段长度上限，与契约一致。
const (
	maxAuditQueryLength      = 256
	maxAuditActionLength     = 128
	maxAuditEmailLength      = 256
	maxAuditClientIPLength   = 64
	maxAuditAuthSourceLength = 64
)

func auditFilter(c *gin.Context, params auditFilterInput) (auditViewFilter, bool) {
	now := time.Now().UTC()
	filter := auditViewFilter{from: now.Add(-24 * time.Hour), to: now}
	if (params.from == nil) != (params.to == nil) {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "from 与 to 必须同时提供")
		return filter, false
	}
	if params.from != nil {
		filter.from, filter.to = params.from.UTC(), params.to.UTC()
	}
	if !filter.to.After(filter.from) || filter.to.Sub(filter.from) > maxAuditRange {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "时间范围无效或超过 30 天")
		return filter, false
	}
	filter.categories = auditCategorySet(params.categories)
	filter.results = auditResultSet(params.results)
	filter.actor = strings.TrimSpace(stringValue((*string)(params.actor)))
	filter.repository = strings.TrimSpace(stringValue((*string)(params.repository)))

	var ok bool
	if filter.query, ok = auditTextParam(c, "q", stringValue((*string)(params.query)), maxAuditQueryLength); !ok {
		return filter, false
	}
	if filter.action, ok = auditTextParam(c, "action", stringValue((*string)(params.action)), maxAuditActionLength); !ok {
		return filter, false
	}
	if filter.email, ok = auditTextParam(c, "actorEmail", stringValue((*string)(params.email)), maxAuditEmailLength); !ok {
		return filter, false
	}
	if filter.clientIP, ok = auditTextParam(c, "clientIp", stringValue((*string)(params.clientIP)), maxAuditClientIPLength); !ok {
		return filter, false
	}
	if filter.authSource, ok = auditTextParam(c, "authSource", stringValue((*string)(params.authSource)), maxAuditAuthSourceLength); !ok {
		return filter, false
	}
	if params.method != nil {
		method := strings.ToUpper(strings.TrimSpace(*params.method))
		if !auditMethodAllowed(method) {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "method 取值无效")
			return filter, false
		}
		filter.method = method
	}
	if params.attention != nil {
		attention := strings.TrimSpace(*params.attention)
		if attention != "pending" && attention != "acknowledged" {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "attention 取值无效")
			return filter, false
		}
		filter.attention = attention
	}
	return filter, true
}

// auditMethodAllowed 校验 HTTP 方法白名单（与契约枚举一致）。
func auditMethodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// auditTextParam 做长度校验：超限直接 400，避免超长检索串进入 SQL。
func auditTextParam(c *gin.Context, name, value string, maxLength int) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > maxLength {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", name+" 超出长度限制")
		return "", false
	}
	return trimmed, true
}

func auditCategorySet(values *AuditCategoryParam) map[AuditCategory]bool {
	if values == nil {
		return nil
	}
	set := make(map[AuditCategory]bool, len(*values))
	for _, value := range *values {
		if value.Valid() {
			set[value] = true
		}
	}
	return set
}

func auditResultSet(values *AuditResultParam) map[AuditResult]bool {
	if values == nil {
		return nil
	}
	set := make(map[AuditResult]bool, len(*values))
	for _, value := range *values {
		if value.Valid() {
			set[value] = true
		}
	}
	return set
}

func (h *Handlers) auditGroups(snapshot auditReadSnapshot, events []repository.ObservabilityEvent, filter auditViewFilter) (map[string]auditAttentionGroup, map[string]repository.AuditAttentionAck, error) {
	groups := make(map[string]auditAttentionGroup)
	for _, event := range events {
		if !isRiskEvent(event) {
			continue
		}
		occurred, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
		if err != nil {
			continue
		}
		bucket := auditBucket(occurred, filter.to.Sub(filter.from))
		key := auditGroupKey(event, bucket)
		group := groups[key]
		if group.payload.GroupKey == "" {
			group.payload = auditAttentionPayload{Snapshot: snapshot, GroupKey: key, Bucket: bucket}
		}
		group.events = append(group.events, event)
		groups[key] = group
	}
	all := make([]repository.ObservabilityEvent, 0)
	for _, group := range groups {
		all = append(all, group.events...)
	}
	acks, err := h.auditObservability.Acknowledgements(all)
	return groups, acks, err
}

// slowRequestThresholdMs 是慢请求阈值（毫秒），与前端 KPI 文案保持一致。
const slowRequestThresholdMs = 1000

func auditSummary(events []repository.ObservabilityEvent, filter auditViewFilter) AuditObservabilitySummary {
	summary := AuditObservabilitySummary{CategoryCounts: emptyCategoryCounts(), Trend: []AuditTrendPoint{}}
	if len(events) == 0 {
		summary.SnapshotAt = filter.to
		return summary
	}
	actors := map[string]bool{}
	categoryCounts := map[AuditCategory]int{}
	trend := map[string]*AuditTrendPoint{}
	clientIPs := map[string]bool{}
	var durationSum int64
	var durationSamples int64
	slowRequests := 0
	clientErrors, serverErrors := 0, 0
	for _, event := range events {
		category, result := classifyAuditCategory(event), classifyAuditResult(event)
		summary.TotalCount++
		categoryCounts[category]++
		if result == AuditResultFailure {
			summary.FailureCount++
		} else if result == AuditResultSuccess {
			summary.SuccessCount++
		}
		if isHighRisk(event) {
			summary.HighRiskCount++
		}
		if event.ClientIP != "" {
			clientIPs[event.ClientIP] = true
		}
		if event.DurationMs > 0 {
			durationSum += event.DurationMs
			durationSamples++
			if event.DurationMs >= slowRequestThresholdMs {
				slowRequests++
			}
		}
		switch {
		case event.StatusCode >= 500:
			serverErrors++
		case event.StatusCode >= 400:
			clientErrors++
		}
		if event.UserID != nil {
			actors["id:"+strconv.FormatInt(*event.UserID, 10)] = true
		} else if event.Source != replicationSource && event.Actor != "" {
			actors["name:"+event.Actor] = true
		}
		occurred, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
		if err != nil {
			continue
		}
		bucket := auditBucket(occurred, filter.to.Sub(filter.from))
		point := trend[bucket]
		if point == nil {
			from, _ := time.Parse(time.RFC3339Nano, bucket)
			to := nextAuditBucket(from, filter.to.Sub(filter.from))
			point = &AuditTrendPoint{From: from, To: to, CategoryCounts: emptyCategoryCounts()}
			trend[bucket] = point
		}
		point.TotalCount++
		if result == AuditResultFailure {
			point.FailureCount++
		}
		incrementCategoryCount(point.CategoryCounts, category)
	}
	summary.CategoryCounts = orderedCategoryCounts(categoryCounts)
	summary.DistinctActorCount = len(actors)
	distinctIPs := len(clientIPs)
	summary.DistinctClientIpCount = &distinctIPs
	summary.SlowRequestCount = &slowRequests
	clientErrorTotal := clientErrors
	summary.ClientErrorCount = &clientErrorTotal
	serverErrorTotal := serverErrors
	summary.ServerErrorCount = &serverErrorTotal
	averageDuration := int64(0)
	if durationSamples > 0 {
		averageDuration = durationSum / durationSamples
	}
	summary.AverageDurationMs = &averageDuration
	denominator := summary.SuccessCount + summary.FailureCount
	if denominator > 0 {
		summary.FailureRate = float64(summary.FailureCount) / float64(denominator)
	}
	keys := make([]string, 0, len(trend))
	for key := range trend {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		summary.Trend = append(summary.Trend, *trend[key])
	}
	return summary
}

// toAPIAuditHTTP 组装脱敏后的 HTTP 上下文；无任何字段时返回 nil（不返回空对象）。
func toAPIAuditHTTP(event repository.ObservabilityEvent) *AuditHttpContext {
	if event.HTTPMethod == "" && event.HTTPPath == "" && event.StatusCode == 0 &&
		event.RequestID == "" && event.UserAgent == "" && event.TokenPreview == "" && event.BodyPreview == "" {
		return nil
	}
	context := &AuditHttpContext{
		Method: AuditHttpContextMethod(event.HTTPMethod),
		Path:   event.HTTPPath,
	}
	if event.StatusCode > 0 {
		status := event.StatusCode
		context.StatusCode = &status
	}
	if event.RequestID != "" {
		requestID := event.RequestID
		context.RequestId = &requestID
	}
	if event.UserAgent != "" {
		userAgent := event.UserAgent
		context.UserAgent = &userAgent
	}
	if event.TokenPreview != "" {
		preview := event.TokenPreview
		context.TokenPreview = &preview
	}
	if event.BodyPreview != "" {
		body := event.BodyPreview
		context.BodyPreview = &body
	}
	return context
}

func (h *Handlers) toAPIAuditEvent(snapshot auditReadSnapshot, event repository.ObservabilityEvent, acknowledgements map[string]repository.AuditAttentionAck) AuditEvent {
	occurred, _ := time.Parse(time.RFC3339Nano, event.OccurredAt)
	apiEvent := AuditEvent{EventId: h.signEventID(event), OccurredAt: occurred, Action: safeAuditAction(event), Actor: toAPIActor(event), Category: classifyAuditCategory(event), Result: classifyAuditResult(event), Severity: auditSeverity(event), Summary: safeAuditSummary(event), Target: toAPIAuditTarget(event)}
	if event.CorrelationID != "" {
		value := event.CorrelationID
		apiEvent.OperationId = &value
	}
	// HTTP 上下文与耗时：复制来源事件没有这些字段，仅在确有值时返回。
	if http := toAPIAuditHTTP(event); http != nil {
		apiEvent.Http = http
	}
	if event.DurationMs > 0 {
		duration := event.DurationMs
		apiEvent.DurationMs = &duration
	}
	if event.ClientIP != "" {
		clientIP := event.ClientIP
		apiEvent.ClientIp = &clientIP
	}
	if isRiskEvent(event) {
		bucket := auditBucket(occurred, snapshot.rangeDuration())
		key := auditGroupKey(event, bucket)
		payload := auditAttentionPayload{Snapshot: snapshot, GroupKey: key, Bucket: bucket}
		state := AuditAttentionStateUnacknowledged
		var ack *AuditAcknowledgement
		if stored, found := acknowledgements[repositoryEventKey(event)]; found {
			state = AuditAttentionStateAcknowledged
			converted := toAPIAcknowledgement(stored)
			ack = &converted
		}
		apiEvent.Attention = &AuditEventAttention{AttentionId: h.signAttention(payload), State: state, Acknowledgement: ack}
	}
	return apiEvent
}

func (h *Handlers) toAPIAttention(payload auditAttentionPayload, group auditAttentionGroup, acknowledgements map[string]repository.AuditAttentionAck) AuditAttention {
	attention := AuditAttention{AttentionId: h.signAttention(payload), CategoryCounts: emptyCategoryCounts(), State: AuditAttentionStateAcknowledged}
	var first *AuditAcknowledgement
	for _, event := range group.events {
		occurred, _ := time.Parse(time.RFC3339Nano, event.OccurredAt)
		if attention.FirstOccurredAt.IsZero() || occurred.Before(attention.FirstOccurredAt) {
			attention.FirstOccurredAt = occurred
		}
		if occurred.After(attention.LatestOccurredAt) {
			attention.LatestOccurredAt = occurred
		}
		category := classifyAuditCategory(event)
		incrementCategoryCount(attention.CategoryCounts, category)
		if classifyAuditResult(event) == AuditResultFailure {
			attention.FailureCount++
		} else if classifyAuditResult(event) == AuditResultSuccess {
			attention.SuccessCount++
		}
		if event.CorrelationID != "" {
			value := event.CorrelationID
			attention.OperationId = &value
		}
		if isRiskEvent(event) {
			attention.RiskEventCount++
			stored, found := acknowledgements[repositoryEventKey(event)]
			if found {
				attention.AcknowledgedRiskEventCount++
				converted := toAPIAcknowledgement(stored)
				if first == nil || converted.AcknowledgedAt.Before(first.AcknowledgedAt) {
					first = &converted
				}
			} else {
				attention.UnacknowledgedRiskEventCount++
			}
		}
	}
	if attention.UnacknowledgedRiskEventCount > 0 {
		attention.State = AuditAttentionStateUnacknowledged
	}
	attention.FirstAcknowledgement = first
	primary := group.events[0]
	for _, candidate := range group.events {
		if auditEventLess(candidate, primary) {
			primary = candidate
		}
	}
	attention.Action, attention.Severity, attention.Summary, attention.Target = safeAuditAction(primary), auditSeverity(primary), safeAuditSummary(primary), toAPIAuditTarget(primary)
	attention.AffectedCount = len(group.events)
	if attention.FailureCount > 0 && attention.SuccessCount > 0 {
		attention.Result = AuditAttentionResultMixed
	} else if attention.FailureCount > 0 {
		attention.Result = AuditAttentionResultFailure
	} else {
		attention.Result = AuditAttentionResultSuccess
	}
	return attention
}

func (h *Handlers) signSnapshot(snapshot auditReadSnapshot) string {
	return h.signAuditValue("snapshot", snapshot)
}
func (h *Handlers) signAttention(payload auditAttentionPayload) string {
	return h.signAuditValue("attention", payload)
}
func (h *Handlers) signEventID(event repository.ObservabilityEvent) string {
	return h.signAuditValue("event", []any{event.Source, event.SourceEventID})
}

func (h *Handlers) signAuditValue(kind string, value any) string {
	body, _ := json.Marshal(value)
	mac := hmac.New(sha256.New, h.auditAttentionKey)
	_, _ = mac.Write([]byte(kind + "."))
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *Handlers) parseSnapshot(raw string) (auditReadSnapshot, error) {
	var snapshot auditReadSnapshot
	return snapshot, h.parseAuditValue("snapshot", raw, &snapshot)
}
func (h *Handlers) parseAttention(raw string) (auditAttentionPayload, error) {
	var payload auditAttentionPayload
	return payload, h.parseAuditValue("attention", raw, &payload)
}
func (h *Handlers) parseEventID(raw string) (string, int64, error) {
	var value []any
	if err := h.parseAuditValue("event", raw, &value); err != nil || len(value) != 2 {
		return "", 0, errors.New("事件标识无效")
	}
	source, ok := value[0].(string)
	if !ok || (source != auditSource && source != replicationSource) {
		return "", 0, errors.New("事件来源无效")
	}
	valueID, ok := value[1].(float64)
	if !ok || valueID <= 0 || valueID != float64(int64(valueID)) {
		return "", 0, errors.New("事件编号无效")
	}
	return source, int64(valueID), nil
}

func (h *Handlers) parseAuditValue(kind, raw string, target any) error {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return errors.New("签名格式无效")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, h.auditAttentionKey)
	_, _ = mac.Write([]byte(kind + "."))
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return errors.New("签名不匹配")
	}
	return json.Unmarshal(body, target)
}

func classifyAuditCategory(event repository.ObservabilityEvent) AuditCategory {
	if event.Source == replicationSource {
		return AuditCategoryReplication
	}
	if classifyAuditResult(event) == AuditResultFailure || strings.EqualFold(event.Result, "rejected") {
		return AuditCategorySecurityEvent
	}
	if strings.HasPrefix(event.Action, "asset.") || strings.Contains(event.Action, ".publish") || strings.Contains(event.Action, ".unpublish") {
		return AuditCategoryAssetChange
	}
	return AuditCategoryManagementChange
}
func classifyAuditResult(event repository.ObservabilityEvent) AuditResult {
	switch event.Result {
	case "ok", "applied", "success":
		return AuditResultSuccess
	case "failed", "error", "rejected":
		return AuditResultFailure
	case "pending":
		return AuditResultPending
	default:
		return AuditResultUnknown
	}
}
func isHighRisk(event repository.ObservabilityEvent) bool {
	switch event.Action {
	case "repository.delete", "repo.delete", "asset.delete", "user.delete", "token.revoke", "acl.set", "settings.update", "setting.set", "migration.switch", "migration.complete":
		return true
	}
	return false
}
func isRiskEvent(event repository.ObservabilityEvent) bool {
	return classifyAuditResult(event) == AuditResultFailure || isHighRisk(event)
}
func auditSeverity(event repository.ObservabilityEvent) AuditSeverity {
	if isCriticalRisk(event) {
		return AuditSeverityCritical
	}
	if classifyAuditResult(event) == AuditResultFailure {
		return AuditSeverityHigh
	}
	if isHighRisk(event) {
		return AuditSeverityHigh
	}
	return AuditSeverityNormal
}
func isCriticalRisk(event repository.ObservabilityEvent) bool {
	switch event.Action {
	case "repository.delete", "repo.delete", "asset.delete", "user.delete", "token.revoke", "migration.switch", "migration.complete":
		return true
	}
	return false
}
func safeAuditAction(event repository.ObservabilityEvent) string {
	if event.Action == "" {
		return "unknown"
	}
	return event.Action
}
func safeAuditSummary(event repository.ObservabilityEvent) string {
	if classifyAuditResult(event) == AuditResultFailure {
		return "操作被拒绝或未完成"
	}
	if event.Source == replicationSource {
		return "复制接收状态已记录"
	}
	return "操作已完成"
}
func toAPIAuditTarget(event repository.ObservabilityEvent) AuditTarget {
	target := AuditTarget{Kind: AuditTargetOther, Label: event.EntityKey}
	if event.Repository != "" {
		repo := event.Repository
		target.Repository = &repo
		target.Label = event.Repository
		target.Kind = AuditTargetRepository
	}
	switch event.EntityType {
	case "asset":
		target.Kind = AuditTargetArtifact
	case "user":
		target.Kind = AuditTargetUser
	case "token":
		target.Kind = AuditTargetToken
	case "acl":
		target.Kind = AuditTargetAcl
	case "repository":
		target.Kind = AuditTargetRepository
	case "setting":
		target.Kind = AuditTargetSetting
	case "migration":
		target.Kind = AuditTargetMigration
	}
	if target.Label == "" {
		target.Label = event.EntityType
	}
	return target
}
func toAPIActor(event repository.ObservabilityEvent) AuditActorSnapshot {
	actor := AuditActorSnapshot{DisplayName: event.Actor, AuthSource: event.AuthSource, SubjectType: AuditActorSystem}
	if event.ActorEmail != "" {
		email := event.ActorEmail
		actor.Email = &email
	}
	if event.Source == replicationSource {
		actor.SubjectType = AuditActorReplication
		if actor.DisplayName == "" {
			actor.DisplayName = "复制服务"
		}
	} else if event.UserID != nil || event.Actor != "" {
		actor.SubjectType = AuditActorUser
		actor.UserId = event.UserID
	} else {
		actor.SubjectType = AuditActorSystem
		actor.DisplayName = "系统"
	}
	return actor
}
func toAPIAcknowledgement(ack repository.AuditAttentionAck) AuditAcknowledgement {
	at, _ := time.Parse(time.RFC3339Nano, ack.AcknowledgedAt)
	actor := AuditActorSnapshot{DisplayName: ack.AcknowledgedByUsername, AuthSource: ack.AuthSource, SubjectType: AuditActorUser, UserId: ack.AcknowledgedByUserID}
	return AuditAcknowledgement{AcknowledgedAt: at, AcknowledgedBy: actor}
}
func repositoryEventKey(event repository.ObservabilityEvent) string {
	return event.Source + ":" + strconv.FormatInt(event.SourceEventID, 10)
}
func riskEvents(events []repository.ObservabilityEvent) []repository.ObservabilityEvent {
	out := make([]repository.ObservabilityEvent, 0, len(events))
	for _, event := range events {
		if isRiskEvent(event) {
			out = append(out, event)
		}
	}
	return out
}
func auditBucket(at time.Time, rangeSize time.Duration) string {
	if rangeSize <= 24*time.Hour {
		return at.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	}
	if rangeSize <= 7*24*time.Hour {
		return at.UTC().Truncate(time.Hour).Format(time.RFC3339Nano)
	}
	return time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
}
func nextAuditBucket(from time.Time, rangeSize time.Duration) time.Time {
	if rangeSize <= 24*time.Hour {
		return from.Add(time.Minute)
	}
	if rangeSize <= 7*24*time.Hour {
		return from.Add(time.Hour)
	}
	return from.AddDate(0, 0, 1)
}
func auditGroupKey(event repository.ObservabilityEvent, bucket string) string {
	if event.CorrelationID != "" {
		return "operation:" + event.CorrelationID + ":" + bucket
	}
	return repositoryEventKey(event) + ":" + bucket
}
func emptyCategoryCounts() []AuditCategoryCount {
	return []AuditCategoryCount{{Category: AuditCategoryManagementChange}, {Category: AuditCategoryAssetChange}, {Category: AuditCategorySecurityEvent}, {Category: AuditCategoryReplication}}
}
func incrementCategoryCount(items []AuditCategoryCount, category AuditCategory) {
	for i := range items {
		if items[i].Category == category {
			items[i].Count++
			return
		}
	}
}
func orderedCategoryCounts(counts map[AuditCategory]int) []AuditCategoryCount {
	items := emptyCategoryCounts()
	for i := range items {
		items[i].Count = counts[items[i].Category]
	}
	return items
}
func auditPageParams(c *gin.Context, value *AuditLimitParam, rawCursor *AuditCursorParam) (int, string, bool) {
	limit := defaultAuditLimit
	if value != nil {
		limit = *value
	}
	if limit < 1 || limit > maxAuditLimit {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "limit 须在 1 到 100 之间")
		return 0, "", false
	}
	cursor := ""
	if rawCursor != nil {
		cursor = *rawCursor
		if cursorOffset(cursor) < 0 {
			auth.WriteError(c, http.StatusBadRequest, "bad_request", "cursor 无效")
			return 0, "", false
		}
	}
	return limit, cursor, true
}
func cursorOffset(cursor string) int {
	if cursor == "" {
		return 0
	}
	value, err := strconv.Atoi(cursor)
	if err != nil || value < 0 {
		return -1
	}
	return value
}
func maxID(source string, id int64, wanted string) int64 {
	if source == wanted {
		return id
	}
	return 1 << 62
}
func auditEventLess(left, right repository.ObservabilityEvent) bool {
	leftFailure, rightFailure := classifyAuditResult(left) == AuditResultFailure, classifyAuditResult(right) == AuditResultFailure
	if leftFailure != rightFailure {
		return leftFailure
	}
	leftHigh, rightHigh := auditSeverity(left) == AuditSeverityHigh, auditSeverity(right) == AuditSeverityHigh
	if leftHigh != rightHigh {
		return leftHigh
	}
	if left.OccurredAt != right.OccurredAt {
		return left.OccurredAt > right.OccurredAt
	}
	return repositoryEventKey(left) > repositoryEventKey(right)
}
func notificationLess(left, right AuditAttentionPreview) bool {
	leftFailure, rightFailure := left.FailureCount > 0, right.FailureCount > 0
	if leftFailure != rightFailure {
		return leftFailure
	}
	leftSeverity, rightSeverity := notificationSeverityRank(left.Severity), notificationSeverityRank(right.Severity)
	if leftSeverity != rightSeverity {
		return leftSeverity > rightSeverity
	}
	if left.LatestOccurredAt != right.LatestOccurredAt {
		return left.LatestOccurredAt.After(right.LatestOccurredAt)
	}
	return left.AttentionId > right.AttentionId
}

// notificationTimeLess 是显式请求（消息中心）的时间排序：最新时间倒序，attentionId 打破平局。
func notificationTimeLess(left, right AuditAttentionPreview) bool {
	if left.LatestOccurredAt != right.LatestOccurredAt {
		return left.LatestOccurredAt.After(right.LatestOccurredAt)
	}
	return left.AttentionId > right.AttentionId
}

// notificationPreview 将完整批次摘要裁剪为通知条目（批次安全摘要投影）。
func notificationPreview(attention AuditAttention) AuditAttentionPreview {
	return AuditAttentionPreview{
		Action: attention.Action, AffectedCount: attention.AffectedCount, AttentionId: attention.AttentionId,
		CategoryCounts: attention.CategoryCounts, FailureCount: attention.FailureCount,
		FirstAcknowledgement: attention.FirstAcknowledgement, FirstOccurredAt: attention.FirstOccurredAt,
		LatestOccurredAt: attention.LatestOccurredAt, OperationId: attention.OperationId, Result: attention.Result,
		Severity: attention.Severity, State: attention.State, SuccessCount: attention.SuccessCount,
		Summary: attention.Summary, Target: attention.Target, UnacknowledgedRiskEventCount: attention.UnacknowledgedRiskEventCount,
	}
}

func notificationSeverityRank(severity AuditSeverity) int {
	switch severity {
	case AuditSeverityCritical:
		return 2
	case AuditSeverityHigh:
		return 1
	default:
		return 0
	}
}
