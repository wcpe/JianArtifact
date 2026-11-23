package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const securityEventReadTimeout = 2 * time.Second

// SecurityEventFilter 按发生时间的半开区间和可信客户端地址筛选；IP 与 CIDR 互斥。
type SecurityEventFilter struct {
	From, To                        time.Time
	Limit                           int
	IP, CIDR                        string
	EventType, Outcome              string
	ActorUserID, RepositoryID       *int64
	CredentialID, SessionID         string
	RequestID, OriginRequestID      string
	OperationID, TaskID, ActionID   string
	CanonicalPath, ObjectID, Digest string
}

// SecurityEventPageKey 是内部固定水位与降序锚点；对外须认证、签名并校验期限及恢复代次。
type SecurityEventPageKey struct {
	SnapshotWatermark int64
	BeforeSequence    int64
	FilterFingerprint [sha256.Size]byte
}

// SecurityEventPage 只包含事件头；对象通过独立分页读取。
type SecurityEventPage struct {
	Items             []SecurityEvent
	SnapshotWatermark int64
	Next              *SecurityEventPageKey
	HasMore           bool
}

// SecurityEventObjectPage 使用事件内稳定明细序号递增读取。
type SecurityEventObjectPage struct {
	Items       []SecurityEventObject
	NextOrdinal int
	HasMore     bool
}

// Get 按事件标识读取头部，不读取对象明细。
func (r *SecurityEventRepo) Get(ctx context.Context, eventID string) (SecurityEvent, error) {
	if eventID == "" || !validSecurityEventRef(eventID) {
		return SecurityEvent{}, ErrInvalidSecurityEventFilter
	}
	ctx, cancel := context.WithTimeout(ctx, securityEventReadTimeout)
	defer cancel()
	row := r.db.QueryRowxContext(ctx, `SELECT `+securityEventColumns+` FROM security_event WHERE event_id=?`, eventID)
	return scanSecurityEvent(row)
}

// List 以持久序号降序翻页。首次读取水位后，新写入的事件在此快照中不可见。
func (r *SecurityEventRepo) List(ctx context.Context, filter SecurityEventFilter, key *SecurityEventPageKey) (SecurityEventPage, error) {
	ctx, cancel := context.WithTimeout(ctx, securityEventReadTimeout)
	defer cancel()
	return listSecurityEvents(ctx, r.db, filter, key)
}

// SecurityEventFilterFingerprint 无须查询数据库，用实际查询的规范条件计算游标绑定指纹。
// 页大小参与校验，但不影响指纹。
func SecurityEventFilterFingerprint(filter SecurityEventFilter) ([sha256.Size]byte, error) {
	plan, err := prepareSecurityEventList(filter)
	return plan.fingerprint, err
}

type securityEventListPlan struct {
	index       string
	conditions  []string
	params      []any
	fingerprint [sha256.Size]byte
}

func prepareSecurityEventList(filter SecurityEventFilter) (securityEventListPlan, error) {
	if err := validateSecurityEventFilter(filter); err != nil {
		return securityEventListPlan{}, err
	}
	index, conditions, params, err := securityEventConditions(filter)
	if err != nil {
		return securityEventListPlan{}, err
	}
	fp, err := json.Marshal(params)
	if err != nil {
		return securityEventListPlan{}, ErrInvalidSecurityEventFilter
	}
	return securityEventListPlan{
		index: index, conditions: conditions, params: params,
		fingerprint: sha256.Sum256(append([]byte(strings.Join(conditions, "\n")+"\n"), fp...)),
	}, nil
}

func listSecurityEvents(ctx context.Context, q sqlx.QueryerContext, filter SecurityEventFilter, key *SecurityEventPageKey) (SecurityEventPage, error) {
	plan, err := prepareSecurityEventList(filter)
	if err != nil {
		return SecurityEventPage{}, err
	}
	page := SecurityEventPage{Items: make([]SecurityEvent, 0)}
	var before int64
	if key == nil {
		// 主键末端查一行，不扫描或统计全部事件。
		if err := sqlx.GetContext(ctx, q, &page.SnapshotWatermark, `SELECT COALESCE((SELECT sequence FROM security_event ORDER BY sequence DESC LIMIT 1),0)`); err != nil {
			return SecurityEventPage{}, ErrSecurityEventUnavailable
		}
	} else {
		if key.SnapshotWatermark < 1 || key.BeforeSequence < 1 || key.BeforeSequence > key.SnapshotWatermark || key.FilterFingerprint != plan.fingerprint {
			return SecurityEventPage{}, ErrInvalidSecurityEventFilter
		}
		page.SnapshotWatermark, before = key.SnapshotWatermark, key.BeforeSequence
	}
	limit, _ := securityEventPageLimit(filter.Limit)
	query, args := securityEventListSQL(plan.index, plan.conditions, plan.params, page.SnapshotWatermark, before, limit)
	rows, err := q.QueryxContext(ctx, query, args...)
	if err != nil {
		return SecurityEventPage{}, ErrSecurityEventUnavailable
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(page.Items) == limit {
			page.HasMore = true
			page.Next = &SecurityEventPageKey{SnapshotWatermark: page.SnapshotWatermark, BeforeSequence: page.Items[limit-1].Sequence, FilterFingerprint: plan.fingerprint}
			break
		}
		event, err := scanSecurityEvent(rows)
		if err != nil {
			return SecurityEventPage{}, err
		}
		page.Items = append(page.Items, event)
	}
	if err := rows.Err(); err != nil {
		return SecurityEventPage{}, ErrSecurityEventUnavailable
	}
	return page, nil
}

// ListObjects 从已知明细锚点之后取有界页面，零锚点表示首屏。
func (r *SecurityEventRepo) ListObjects(ctx context.Context, eventID string, afterOrdinal, limit int) (SecurityEventObjectPage, error) {
	limit, ok := securityEventPageLimit(limit)
	if eventID == "" || !validSecurityEventRef(eventID) || afterOrdinal < 0 || afterOrdinal > securityEventMaxObjects || !ok {
		return SecurityEventObjectPage{}, ErrInvalidSecurityEventFilter
	}
	ctx, cancel := context.WithTimeout(ctx, securityEventReadTimeout)
	defer cancel()
	var sequence int64
	if err := r.db.GetContext(ctx, &sequence, `SELECT sequence FROM security_event WHERE event_id=?`, eventID); errors.Is(err, sql.ErrNoRows) {
		return SecurityEventObjectPage{}, ErrSecurityEventNotFound
	} else if err != nil {
		return SecurityEventObjectPage{}, ErrSecurityEventUnavailable
	}
	return listSecurityEventObjects(ctx, r.db, sequence, afterOrdinal, limit)
}

func listSecurityEventObjects(ctx context.Context, q sqlx.QueryerContext, sequence int64, afterOrdinal, limit int) (SecurityEventObjectPage, error) {
	page := SecurityEventObjectPage{Items: make([]SecurityEventObject, 0)}
	if err := sqlx.SelectContext(ctx, q, &page.Items, `SELECT `+securityEventObjectColumns+` FROM security_event_object
		WHERE event_sequence=? AND ordinal>? ORDER BY ordinal LIMIT ?`, sequence, afterOrdinal, limit+1); err != nil {
		return SecurityEventObjectPage{}, ErrSecurityEventUnavailable
	}
	if len(page.Items) > limit {
		page.HasMore, page.NextOrdinal = true, page.Items[limit-1].Ordinal
		page.Items = page.Items[:limit:limit]
	}
	return page, nil
}

func securityEventPageLimit(limit int) (int, bool) {
	if limit == 0 {
		return 50, true
	}
	return limit, limit > 0 && limit <= 100
}

func validateSecurityEventFilter(f SecurityEventFilter) error {
	if _, ok := securityEventPageLimit(f.Limit); !ok || !validSecurityEventTime(f.From) || !validSecurityEventTime(f.To) ||
		f.To.UnixMilli() <= f.From.UnixMilli() || f.To.Sub(f.From) > 30*24*time.Hour ||
		(f.IP != "" && f.CIDR != "") || len(f.IP) > 64 || len(f.CIDR) > 64 ||
		(f.EventType != "" && !validSecurityEventType(f.EventType)) || (f.Outcome != "" && !validSecurityEventOutcome(f.Outcome)) ||
		!validSecurityEventEntity(f.ActorUserID) || !validSecurityEventEntity(f.RepositoryID) ||
		!validSecurityEventPublicID(f.CredentialID) || !validSecurityEventPublicID(f.SessionID) ||
		(f.CanonicalPath != "" && !validSecurityEventPath(f.CanonicalPath)) || !validSecurityEventDigest(f.Digest) {
		return ErrInvalidSecurityEventFilter
	}
	for _, id := range []string{f.RequestID, f.OriginRequestID, f.OperationID, f.TaskID, f.ActionID, f.ObjectID} {
		if !validSecurityEventRef(id) {
			return ErrInvalidSecurityEventFilter
		}
	}
	return nil
}

func securityEventConditions(f SecurityEventFilter) (string, []string, []any, error) {
	index := "idx_security_event_occurred"
	conditions := []string{"occurred_at >= ?", "occurred_at < ?"}
	args := []any{f.From.UnixMilli(), f.To.UnixMilli()}
	add := func(condition string, values ...any) {
		conditions, args = append(conditions, condition), append(args, values...)
	}
	if f.EventType != "" {
		add("event_type = ?", f.EventType)
	}
	if f.Outcome != "" {
		add("outcome = ?", f.Outcome)
	}
	if f.ActorUserID != nil {
		index = "idx_security_event_actor"
		add("actor_user_id = ?", *f.ActorUserID)
	}
	for _, v := range []struct{ value, column, index string }{
		{f.CredentialID, "credential_id", "idx_security_event_credential"}, {f.SessionID, "session_id", "idx_security_event_session"},
		{f.RequestID, "request_id", ""}, {f.OriginRequestID, "origin_request_id", ""}, {f.TaskID, "task_id", ""}, {f.ActionID, "action_id", ""},
		{f.OperationID, "operation_id", "idx_security_event_operation"},
	} {
		if v.value == "" {
			continue
		}
		if v.index != "" {
			index = v.index
			add(v.column + " <> ''")
		}
		add(v.column+" = ?", v.value)
	}
	if f.IP != "" {
		ip, err := netip.ParseAddr(f.IP)
		if err != nil || ip.Zone() != "" {
			return "", nil, nil, ErrInvalidSecurityEventFilter
		}
		family, address := securityEventIPKey(ip)
		index = "idx_security_event_client_ip"
		add("client_ip_family = ? AND client_ip = ?", family, address)
	} else if f.CIDR != "" {
		family, low, high, ok := securityEventCIDRRange(f.CIDR)
		if !ok {
			return "", nil, nil, ErrInvalidSecurityEventFilter
		}
		index = "idx_security_event_client_ip"
		add("client_ip_family = ? AND client_ip >= ? AND client_ip <= ?", family, low, high)
	}
	objectConditions := []string{"o.event_sequence = security_event.sequence"}
	var objectArgs []any
	for _, v := range []struct{ value, column string }{{f.CanonicalPath, "canonical_path"}, {f.ObjectID, "object_id"}, {f.Digest, "digest"}} {
		if v.value != "" {
			objectConditions, objectArgs = append(objectConditions, "o."+v.column+" = ?"), append(objectArgs, v.value)
		}
	}
	if f.RepositoryID != nil {
		objectConditions, objectArgs = append(objectConditions, "COALESCE(o.repository_id, security_event.repository_id) = ?"), append(objectArgs, *f.RepositoryID)
	}
	if len(objectConditions) > 1 {
		objectSQL := `EXISTS (SELECT 1 FROM security_event_object o WHERE ` + strings.Join(objectConditions, " AND ") + ")"
		if f.RepositoryID != nil && len(objectConditions) == 2 {
			add("(repository_id = ? OR "+objectSQL+")", *f.RepositoryID, *f.RepositoryID)
		} else {
			add(objectSQL, objectArgs...)
		}
	}
	return index, conditions, args, nil
}

func securityEventListSQL(index string, conditions []string, args []any, watermark, before int64, limit int) (string, []any) {
	query := `SELECT ` + securityEventColumns + ` FROM security_event INDEXED BY ` + index + ` WHERE ` + strings.Join(conditions, " AND ") + ` AND sequence <= ?`
	args = append(args, watermark)
	if before != 0 {
		query += ` AND sequence < ?`
		args = append(args, before)
	}
	return query + ` ORDER BY sequence DESC LIMIT ?`, append(args, limit+1)
}

func securityEventCIDRRange(value string) (int, []byte, []byte, bool) {
	p, err := netip.ParsePrefix(value)
	if err != nil {
		return 0, nil, nil, false
	}
	if p.Addr().Is4In6() {
		if p.Bits() < 96 {
			return 0, nil, nil, false
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	family, low := securityEventIPKey(p.Masked().Addr())
	high := append([]byte(nil), low...)
	for bit := p.Bits(); bit < len(high)*8; bit++ {
		high[bit/8] |= 1 << (7 - bit%8)
	}
	return family, low, high, true
}

type securityEventScanner interface{ Scan(...any) error }

func scanSecurityEvent(row securityEventScanner) (SecurityEvent, error) {
	var e SecurityEvent
	var occurred, ingested int64
	var peerFamily, clientFamily int
	var peer, client, chain []byte
	err := row.Scan(&e.Sequence, &e.EventID, &e.EventVersion, &e.EventType, &occurred, &ingested,
		&e.RequestID, &e.OriginRequestID, &e.OperationID, &e.TaskID, &e.ActionID,
		&e.ActorKind, &e.ActorUserID, &e.ActorNameSnapshot, &e.CredentialID, &e.SessionID, &e.AuthMethod, &e.AuthStrength,
		&peerFamily, &peer, &clientFamily, &client, &e.ProxyTrust, &e.SourceTrustVersion, &chain,
		&e.UserAgent, &e.RepositoryID, &e.Outcome, &e.Committed, &e.HTTPStatus, &e.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return SecurityEvent{}, ErrSecurityEventNotFound
	}
	if err != nil {
		return SecurityEvent{}, ErrSecurityEventUnavailable
	}
	var ok bool
	e.PeerIP, ok = decodeSecurityEventIP(peerFamily, peer)
	if !ok {
		return SecurityEvent{}, ErrSecurityEventUnavailable
	}
	e.ClientIP, ok = decodeSecurityEventIP(clientFamily, client)
	if !ok {
		return SecurityEvent{}, ErrSecurityEventUnavailable
	}
	e.ForwardedChain, ok = decodeSecurityEventChain(chain)
	if !ok {
		return SecurityEvent{}, ErrSecurityEventUnavailable
	}
	e.OccurredAt, e.IngestedAt = time.UnixMilli(occurred).UTC(), time.UnixMilli(ingested).UTC()
	return e, nil
}
