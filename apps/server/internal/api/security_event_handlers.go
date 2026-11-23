package api

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// SecurityAuditQuery 绑定当前节点的安全事件仓储、采集器及本次启动的游标栅栏。
type SecurityAuditQuery struct {
	events    *repository.SecurityEventRepo
	collector *domain.SecurityAuditService
	cursors   *securityEventCursorCodec
}

// NewSecurityAuditQuery 从启动秘密独立派生游标密钥，并为本次进程生成随机栅栏。
func NewSecurityAuditQuery(events *repository.SecurityEventRepo, collector *domain.SecurityAuditService, startupSecret []byte, nodeID string) (*SecurityAuditQuery, error) {
	if len(startupSecret) == 0 || events == nil || collector == nil {
		return nil, errSecurityEventCursorConfig
	}
	var boot [16]byte
	if _, err := rand.Read(boot[:]); err != nil {
		return nil, errSecurityEventCursorConfig
	}
	key := securityEventCursorMAC(startupSecret, "jianartifact/security-audit-query/startup-key/v1")
	defer clear(key[:])
	codec, err := newSecurityEventCursorCodec(key[:], nodeID, boot, nil)
	if err != nil {
		return nil, err
	}
	return &SecurityAuditQuery{events: events, collector: collector, cursors: codec}, nil
}

// AuthorizeSecurityAuditRead 在生成的参数绑定前授权，不信任请求提供的主体、角色或会话标识。
func AuthorizeSecurityAuditRead(c *gin.Context) {
	if _, ok := requireSecurityAuditSession(c); ok && len(c.Request.URL.RawQuery) > maxSecurityAuditQueryBytes {
		WriteSecurityAuditBindingError(c)
	}
}

func validSecurityEventCursorSession(session securityEventCursorSession) bool {
	// 只接受本服务签发的正用户标识和小写十六进制会话随机号。
	if session.userID <= 0 || len(session.jti) != 32 {
		return false
	}
	for _, ch := range session.jti {
		if !('0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f') {
			return false
		}
	}
	return true
}

func requireSecurityAuditSession(c *gin.Context) (securityEventCursorSession, bool) {
	c.Header("Cache-Control", "no-store")
	p, ok := auth.PrincipalFrom(c)
	if !ok {
		writeSecurityAuditError(c, http.StatusUnauthorized, "unauthenticated", "未认证或凭据无效")
		return securityEventCursorSession{}, false
	}
	if !p.IsAdmin() || p.WebLoginDisabled || p.Kind != auth.KindSession || p.AuthSource != auth.AuthSourceWebJWT || p.ExpiresAt.IsZero() || !validSecurityEventCursorSession(securityEventCursorSession{userID: p.UserID, jti: p.JTI}) {
		RejectSecurityAuditPrincipal(c)
		return securityEventCursorSession{}, false
	}
	return securityEventCursorSession{userID: p.UserID, jti: p.JTI}, true
}

// RejectSecurityAuditPrincipal 拒绝没有交互式管理读取权限的已认证主体。
func RejectSecurityAuditPrincipal(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	writeSecurityAuditError(c, http.StatusForbidden, "forbidden", "需要全局管理员交互会话")
}

func writeSecurityAuditError(c *gin.Context, status int, code, message string) {
	response := Error{}
	response.Error.Code, response.Error.Message = code, message
	if source, ok := auditctx.SourceFrom(c.Request.Context()); ok {
		response.RequestId = optionalString(source.RequestID)
	}
	c.AbortWithStatusJSON(status, response)
}

// WriteSecurityAuditBindingError 映射生成绑定错误，不返回其内部原因或原始参数。
func WriteSecurityAuditBindingError(c *gin.Context) {
	writeSecurityAuditError(c, http.StatusBadRequest, "invalid_security_filter", "安全事件查询参数无效")
}

func securityAuditQueryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errInvalidSecurityEventCursor), errors.Is(err, repository.ErrInvalidSecurityEventFilter):
		WriteSecurityAuditBindingError(c)
	case errors.Is(err, errExpiredSecurityEventCursor), errors.Is(err, repository.ErrSecurityEventSnapshotExpired):
		writeSecurityAuditError(c, http.StatusGone, "security_cursor_expired", "安全事件游标已过期或快照不可继续")
	case errors.Is(err, repository.ErrSecurityEventNotFound):
		writeSecurityAuditError(c, http.StatusNotFound, "security_event_not_found", "安全事件不存在")
	default:
		writeSecurityAuditError(c, http.StatusServiceUnavailable, "security_audit_unavailable", "安全审计暂不可用")
	}
}

const securityAuditListParams = " from to limit cursor ip cidr eventType outcome actorUserId repositoryId credentialId sessionId requestId originRequestId operationId taskId actionId canonicalPath objectId digest "
const securityAuditObjectParams = " limit cursor "

// 在语法解析前限制原始长度，避免重复或超长输入造成无界分配。
const maxSecurityAuditQueryBytes = 16 * 1024

// 用已解码的原始时间文本检查无损毫秒精度，避免绑定器忽略超出纳秒精度的尾数。
var securityAuditRFC3339Millis = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,3}0*)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func securityAuditQueryParams(c *gin.Context, allow string) (url.Values, bool) {
	raw := c.Request.URL.RawQuery
	if len(raw) > maxSecurityAuditQueryBytes || raw == "" && c.Request.URL.ForceQuery || raw != "" && (raw[0] == '&' || raw[len(raw)-1] == '&' || strings.Contains(raw, "&&")) {
		WriteSecurityAuditBindingError(c)
		return nil, false
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		WriteSecurityAuditBindingError(c)
		return nil, false
	}
	for key, vals := range values {
		if key == "" || strings.ContainsRune(key, ' ') || !strings.Contains(allow, " "+key+" ") || len(vals) != 1 || strings.TrimSpace(vals[0]) == "" || !utf8.ValidString(vals[0]) || strings.ContainsFunc(vals[0], unicode.IsControl) {
			WriteSecurityAuditBindingError(c)
			return nil, false
		}
	}
	return values, true
}

func securityAuditLimit(value *SecurityAuditLimitParam) (int, bool) {
	if value == nil {
		return 50, true
	}
	return *value, *value >= 1 && *value <= 100
}

func securityAuditString[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func (h *Handlers) ListSecurityAuditEvents(c *gin.Context, params ListSecurityAuditEventsParams) {
	session, ok := requireSecurityAuditSession(c)
	if !ok {
		return
	}
	query, ok := securityAuditQueryParams(c, securityAuditListParams)
	if !ok {
		return
	}
	limit, validLimit := securityAuditLimit(params.Limit)
	if !validLimit || !securityAuditRFC3339Millis.MatchString(query.Get("from")) || !securityAuditRFC3339Millis.MatchString(query.Get("to")) {
		WriteSecurityAuditBindingError(c)
		return
	}
	filter := repository.SecurityEventFilter{
		From: params.From.UTC(), To: params.To.UTC(), Limit: limit, IP: stringValue(params.Ip), CIDR: stringValue(params.Cidr),
		EventType: securityAuditString(params.EventType), Outcome: securityAuditString(params.Outcome), ActorUserID: params.ActorUserId, RepositoryID: params.RepositoryId,
		CredentialID: stringValue(params.CredentialId), SessionID: stringValue(params.SessionId), RequestID: stringValue(params.RequestId), OriginRequestID: stringValue(params.OriginRequestId),
		OperationID: stringValue(params.OperationId), TaskID: stringValue(params.TaskId), ActionID: stringValue(params.ActionId), CanonicalPath: stringValue(params.CanonicalPath), ObjectID: stringValue(params.ObjectId), Digest: stringValue(params.Digest),
	}
	fingerprint, err := repository.SecurityEventFilterFingerprint(filter)
	if err != nil {
		securityAuditQueryError(c, err)
		return
	}
	s := h.securityAuditQuery
	if s == nil {
		securityAuditQueryError(c, repository.ErrSecurityEventUnavailable)
		return
	}
	var cursor securityEventListCursor
	var key *repository.SecurityEventPageKey
	var generation uint64
	var state securityEventCursorListState
	if values := query["cursor"]; values != nil {
		cursor, err = s.cursors.decodeList(values, session, fingerprint)
		if err == nil {
			state, generation, err = cursor.snapshot()
		}
		if err != nil {
			securityAuditQueryError(c, err)
			return
		}
		key = &repository.SecurityEventPageKey{SnapshotWatermark: state.watermark, BeforeSequence: state.beforeSequence, FilterFingerprint: state.fingerprint}
	}
	view, err := s.events.ListView(c.Request.Context(), filter, key, int64(generation))
	if err != nil {
		securityAuditQueryError(c, err)
		return
	}
	coverage := securityAuditCoverageToCursor(view.Coverage)
	if key != nil {
		if _, err := cursor.page(uint64(view.RetentionGeneration)); err != nil {
			securityAuditQueryError(c, err)
			return
		}
		coverage = state.coverage
	}
	result := SecurityAuditEventPage{Items: make([]SecurityAuditEvent, len(view.Items)), HasMore: view.HasMore, SnapshotWatermark: view.SnapshotWatermark, ProjectionWatermark: view.SnapshotWatermark, ProjectionHealth: SecurityAuditEventPageProjectionHealthHealthy, Coverage: securityAuditCoverageFromCursor(coverage)}
	for i, item := range view.Items {
		result.Items[i] = toAPISecurityAuditEvent(item)
	}
	if view.HasMore {
		var next string
		if key == nil {
			next, err = s.cursors.issueList(session, uint64(view.RetentionGeneration), securityEventCursorListState{watermark: view.SnapshotWatermark, beforeSequence: view.Next.BeforeSequence, fingerprint: fingerprint, coverage: coverage})
			if err != nil {
				err = repository.ErrSecurityEventUnavailable
			}
		} else {
			next, err = cursor.next(uint64(view.RetentionGeneration), view.Next.BeforeSequence)
		}
		if err != nil {
			securityAuditQueryError(c, err)
			return
		}
		result.NextCursor = &next
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handlers) GetSecurityAuditEvent(c *gin.Context, eventID SecurityAuditEventIdParam) {
	if _, ok := requireSecurityAuditSession(c); !ok {
		return
	}
	if _, ok := securityAuditQueryParams(c, ""); !ok {
		return
	}
	if !securityEventCursorRef(eventID, false) {
		WriteSecurityAuditBindingError(c)
		return
	}
	s := h.securityAuditQuery
	if s == nil {
		securityAuditQueryError(c, repository.ErrSecurityEventUnavailable)
		return
	}
	item, err := s.events.Get(c.Request.Context(), eventID)
	if err != nil {
		securityAuditQueryError(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPISecurityAuditEvent(item))
}

func (h *Handlers) ListSecurityAuditEventObjects(c *gin.Context, eventID SecurityAuditEventIdParam, params ListSecurityAuditEventObjectsParams) {
	session, ok := requireSecurityAuditSession(c)
	if !ok {
		return
	}
	query, ok := securityAuditQueryParams(c, securityAuditObjectParams)
	if !ok {
		return
	}
	limit, validLimit := securityAuditLimit(params.Limit)
	if !validLimit || !securityEventCursorRef(eventID, false) {
		WriteSecurityAuditBindingError(c)
		return
	}
	s := h.securityAuditQuery
	if s == nil {
		securityAuditQueryError(c, repository.ErrSecurityEventUnavailable)
		return
	}
	var cursor securityEventObjectCursor
	var key *repository.SecurityEventObjectViewKey
	if values := query["cursor"]; values != nil {
		chain, err := s.cursors.decodeObjects(values, session, eventID)
		if err != nil {
			securityAuditQueryError(c, err)
			return
		}
		cursor = chain
		state, gen, err := cursor.snapshot()
		if err != nil {
			securityAuditQueryError(c, err)
			return
		}
		key = &repository.SecurityEventObjectViewKey{EventSequence: state.eventSequence, AfterOrdinal: state.afterOrdinal, RetentionGeneration: int64(gen)}
	}
	view, err := s.events.ListObjectsView(c.Request.Context(), eventID, limit, key)
	if err != nil {
		securityAuditQueryError(c, err)
		return
	}
	if key != nil {
		if _, err := cursor.page(uint64(view.RetentionGeneration), view.EventSequence); err != nil {
			securityAuditQueryError(c, err)
			return
		}
	}
	result := SecurityAuditEventObjectPage{Items: make([]SecurityAuditEventObject, len(view.Items)), HasMore: view.HasMore}
	for i, item := range view.Items {
		result.Items[i] = toAPISecurityAuditObject(item)
	}
	if view.HasMore {
		var next string
		if key == nil {
			next, err = s.cursors.issueObjects(session, uint64(view.RetentionGeneration), eventID, securityEventCursorObjectState{eventSequence: view.EventSequence, afterOrdinal: view.NextOrdinal})
			if err != nil {
				err = repository.ErrSecurityEventUnavailable
			}
		} else {
			next, err = cursor.next(uint64(view.RetentionGeneration), view.EventSequence, view.NextOrdinal)
		}
		if err != nil {
			securityAuditQueryError(c, err)
			return
		}
		result.NextCursor = &next
	}
	c.JSON(http.StatusOK, result)
}

func securityAuditCoverageToCursor(coverage repository.SecurityQueryCoverage) securityEventCursorCoverage {
	switch coverage {
	case repository.SecurityQueryCoverageComplete:
		return securityEventCursorCoverageComplete
	case repository.SecurityQueryCoverageGapped:
		return securityEventCursorCoverageGap
	default:
		return securityEventCursorCoverageUnknown
	}
}

func securityAuditCoverageFromCursor(coverage securityEventCursorCoverage) SecurityAuditCoverage {
	switch coverage {
	case securityEventCursorCoverageComplete:
		return SecurityAuditCoverageComplete
	case securityEventCursorCoverageGap:
		return SecurityAuditCoverageGapped
	default:
		return SecurityAuditCoverageUnknown
	}
}

func toAPISecurityAuditEvent(e repository.SecurityEvent) SecurityAuditEvent {
	v := SecurityAuditEvent{
		EventId: e.EventID, EventType: SecurityAuditEventType(e.EventType), EventVersion: e.EventVersion,
		OccurredAt: e.OccurredAt.UTC(), IngestedAt: e.IngestedAt.UTC(), ActorKind: SecurityAuditEventActorKind(e.ActorKind), ActorUserId: e.ActorUserID, ActorNameSnapshot: optionalString(e.ActorNameSnapshot),
		CredentialId: optionalString(e.CredentialID), SessionId: optionalString(e.SessionID), ProxyTrust: SecurityAuditEventProxyTrust(e.ProxyTrust), PeerIp: securityAuditIP(e.PeerIP), ClientIp: securityAuditIP(e.ClientIP), UserAgent: optionalString(e.UserAgent),
		RequestId: optionalString(e.RequestID), OriginRequestId: optionalString(e.OriginRequestID), OperationId: optionalString(e.OperationID), TaskId: optionalString(e.TaskID), ActionId: optionalString(e.ActionID), RepositoryId: e.RepositoryID,
		Outcome: SecurityAuditOutcome(e.Outcome), Committed: SecurityAuditEventCommitted(e.Committed), HttpStatus: e.HTTPStatus, ErrorCode: optionalString(e.ErrorCode),
	}
	if e.AuthMethod != "" {
		v.AuthMethod = ptr(SecurityAuditEventAuthMethod(e.AuthMethod))
	}
	if e.AuthStrength != "" {
		v.AuthStrength = ptr(SecurityAuditEventAuthStrength(e.AuthStrength))
	}
	if len(e.ForwardedChain) > 0 {
		chain := make([]string, len(e.ForwardedChain))
		for i, ip := range e.ForwardedChain {
			chain[i] = ip.String()
		}
		v.ForwardedChain = &chain
	}
	return v
}

func securityAuditIP(ip netip.Addr) *string {
	if ip.IsValid() {
		return ptr(ip.String())
	}
	return nil
}

func toAPISecurityAuditObject(o repository.SecurityEventObject) SecurityAuditEventObject {
	return SecurityAuditEventObject{
		Ordinal: o.Ordinal, ObjectKind: SecurityAuditEventObjectObjectKind(o.ObjectKind), ObjectId: optionalString(o.ObjectID), CanonicalPath: optionalString(o.CanonicalPath), RepositoryId: o.RepositoryID, RepositoryNameSnapshot: optionalString(o.RepositoryNameSnapshot),
		Digest: optionalString(o.Digest), ManifestDigest: optionalString(o.ManifestDigest), SizeBytes: o.SizeBytes, TargetVersion: optionalString(o.TargetVersion),
	}
}
