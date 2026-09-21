package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ObservabilityEvent 是统一审计读模型的原始事件；调用方负责固定分类和脱敏映射。
type ObservabilityEvent struct {
	Source        string `db:"source"`
	SourceEventID int64  `db:"source_event_id"`
	OccurredAt    string `db:"occurred_at"`
	Actor         string `db:"actor"`
	UserID        *int64 `db:"user_id"`
	AuthSource    string `db:"auth_source"`
	Action        string `db:"action"`
	EntityType    string `db:"entity_type"`
	EntityKey     string `db:"entity_key"`
	Repository    string `db:"repository"`
	Result        string `db:"result"`
	CorrelationID string `db:"correlation_id"`
	Detail        string `db:"detail"`
	ErrorClass    string `db:"error_class"`
	// 以下为脱敏后的 HTTP 上下文与身份快照；复制来源事件没有对应信息，统一为空值。
	HTTPMethod   string `db:"http_method"`
	HTTPPath     string `db:"http_path"`
	StatusCode   int    `db:"status_code"`
	DurationMs   int64  `db:"duration_ms"`
	ClientIP     string `db:"client_ip"`
	UserAgent    string `db:"user_agent"`
	RequestID    string `db:"request_id"`
	TokenPreview string `db:"token_preview"`
	BodyPreview  string `db:"body_preview"`
	ActorEmail   string `db:"actor_email"`
}

// ObservabilityFilter 表示当前节点不可变审计事件查询边界。
type ObservabilityFilter struct {
	From             string
	To               string
	AuditMaxID       int64
	ReplicationMaxID int64
	SourceNode       string
	Categories       []string
	Results          []string
	Actor            string
	Repository       string
	// Query 是关键字检索：匹配动作、摘要/详情、目标、操作者与邮箱、请求路径与客户端 IP。
	Query string
	// Method 按 HTTP 请求方法精确筛选（大写）。
	Method string
	// Action 按操作名称精确筛选。
	Action string
	// Email 按操作者邮箱精确筛选。
	Email string
	// ClientIP 按客户端 IP 前缀筛选。
	ClientIP string
	// AuthSource 按认证方式精确筛选。
	AuthSource string
	// AuditOnly 限定只读 audit_log 源：聚合类路径（KPI/趋势/关注批次/通知）置位——
	// replication 同步记录 30 天可达百万级（线上实测 114 万），既拖垮聚合内存/耗时，
	// 又淹没「审计事件」的统计语义；它仍完整保留在事件流列表（ListEventPage 不置位）。
	AuditOnly bool
	// Attention 按风险批次状态筛选：pending（未确认）/ acknowledged（已确认）；空值不过滤。
	Attention string
}

// AuditAttentionAck 是当前节点的风险确认身份快照。
type AuditAttentionAck struct {
	Source                 string `db:"source"`
	SourceEventID          int64  `db:"source_event_id"`
	AcknowledgedByUserID   *int64 `db:"acknowledged_by_user_id"`
	AcknowledgedByUsername string `db:"acknowledged_by_username"`
	AuthSource             string `db:"acknowledged_by_auth_source"`
	AcknowledgedAt         string `db:"acknowledged_at"`
}

// AuditObservabilityRepo 读取当前节点的不可变审计事件并维护本地确认状态。
type AuditObservabilityRepo struct{ db *persistence.DB }

func NewAuditObservabilityRepo(db *persistence.DB) *AuditObservabilityRepo {
	return &AuditObservabilityRepo{db: db}
}

// SnapshotBoundary 返回当前两个不可变事件源的最高 ID。
func (r *AuditObservabilityRepo) SnapshotBoundary(sourceNode string) (int64, int64, error) {
	var auditMax, replicationMax int64
	if err := r.db.Get(&auditMax, `SELECT COALESCE(MAX(id), 0) FROM audit_log WHERE source_node = '' OR source_node = ?`, sourceNode); err != nil {
		return 0, 0, err
	}
	if err := r.db.Get(&replicationMax, `SELECT COALESCE(MAX(id), 0) FROM replication_apply_event`); err != nil {
		return 0, 0, err
	}
	return auditMax, replicationMax, nil
}

// ListEvents 按稳定时间顺序返回快照内的统一事件源。
func (r *AuditObservabilityRepo) ListEvents(f ObservabilityFilter) ([]ObservabilityEvent, error) {
	var events []ObservabilityEvent
	query, args := observabilityEventsQuery(f, observabilityEventSelect)
	err := r.db.Select(&events, query+` ORDER BY occurred_at DESC, source DESC, source_event_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []ObservabilityEvent{}
	}
	return events, nil
}

// ListEventsLimited 为**关注批次详情**（会把事件原样序列化给前端看证据）读取设定硬上限，
// 防止把大范围审计历史载入内存；用全量投影。
func (r *AuditObservabilityRepo) ListEventsLimited(f ObservabilityFilter, limit int) ([]ObservabilityEvent, bool, error) {
	return r.listEventsLimited(f, limit, observabilityEventSelect)
}

// ListAggregateEventsLimited 是**聚合路径**（概览 KPI/趋势、关注批次列表、通知）的读取入口：
// 同样的"取 limit+1 判是否超限"约定，但用窄投影（见 observabilityAggregateSelect）——
// 同内存预算下能覆盖的事件量比全量投影高一个数量级。线上 24h 窗口轻松过万条事件
// （实测生产库上线第三天 24h 已达 5900 条），若聚合仍按全量投影设 5000 上限，
// 默认视图会直接 409「审计聚合范围过大」——这正是线上那个 bug。
func (r *AuditObservabilityRepo) ListAggregateEventsLimited(f ObservabilityFilter, limit int) ([]ObservabilityEvent, bool, error) {
	return r.listEventsLimited(f, limit, observabilityAggregateSelect)
}

func (r *AuditObservabilityRepo) listEventsLimited(f ObservabilityFilter, limit int, selectClause string) ([]ObservabilityEvent, bool, error) {
	// 同 ListEventPage：无过滤时下推每源 LIMIT（走时间索引取满即停，避免全量物化排序）。
	query, args, limited := observabilityEventsQueryLimited(f, selectClause, limit+1)
	if !limited {
		query, args = observabilityEventsQuery(f, selectClause)
	}
	args = append(args, limit+1)
	var events []ObservabilityEvent
	if err := r.db.Select(&events, query+` ORDER BY occurred_at DESC, source DESC, source_event_id DESC LIMIT ?`, args...); err != nil {
		return nil, false, err
	}
	if len(events) <= limit {
		if events == nil {
			events = []ObservabilityEvent{}
		}
		return events, false, nil
	}
	return events[:limit], true, nil
}

// ListEventPage 按快照与筛选条件直接在 SQLite 中分页，避免请求页读取整段历史。
func (r *AuditObservabilityRepo) ListEventPage(f ObservabilityFilter, offset, limit int) ([]ObservabilityEvent, int, *int, error) {
	// COUNT 只在首屏（offset==0）执行：同一 CTE 的全量计数在翻页时重复代价极高——
	// 30 天范围下正是审计中心「切 30d 卡死」的主因之一。翻页返回 total=-1（未计数）。
	total := -1
	if offset == 0 {
		countQuery, countArgs := observabilityEventsQuery(f, "SELECT COUNT(*)")
		if err := r.db.Get(&total, countQuery, countArgs...); err != nil {
			return nil, 0, nil, err
		}
	}
	// 翻页（无精确 total）时多取 1 条用于精确判定「是否还有更多」；首屏无需多取。
	fetchLimit := limit
	if total < 0 {
		fetchLimit = limit + 1
	}
	// 无过滤时把每源 LIMIT 下推（走时间索引、取满即停）；有过滤时回退通用查询。
	query, args, limited := observabilityEventsQueryLimited(f, observabilityEventSelect, offset+fetchLimit)
	if !limited {
		query, args = observabilityEventsQuery(f, observabilityEventSelect)
	}
	args = append(args, fetchLimit, offset)
	var events []ObservabilityEvent
	if err := r.db.Select(&events, query+` ORDER BY occurred_at DESC, source DESC, source_event_id DESC LIMIT ? OFFSET ?`, args...); err != nil {
		return nil, 0, nil, err
	}
	if events == nil {
		events = []ObservabilityEvent{}
	}
	if total < 0 {
		hasMore := len(events) > limit
		if hasMore {
			events = events[:limit]
		}
		if !hasMore {
			return events, total, nil, nil
		}
		nextOffset := offset + len(events)
		return events, total, &nextOffset, nil
	}
	nextOffset := offset + len(events)
	if nextOffset >= total {
		return events, total, nil, nil
	}
	return events, total, &nextOffset, nil
}

// observabilityEventSelect 是统一读模型的投影列清单（真实列全部来自 CTE 别名）。
const observabilityEventSelect = `SELECT source, source_event_id, occurred_at, actor, user_id, auth_source,
	action, entity_type, entity_key, repository, result, correlation_id, detail, error_class,
	http_method, http_path, status_code, duration_ms, client_ip, user_agent, request_id,
	token_preview, body_preview, actor_email`

// observabilityAggregateSelect 是**聚合读**用的窄投影：只取统计与分组真正读到的列。
//
// 聚合路径（概览 KPI/趋势、关注批次列表、通知）只读 occurred_at / action / result /
// actor / status_code / duration_ms / client_ip 这类字段，`detail`、`body_preview`、
// `user_agent`、`token_preview`、`request_id`、`actor_email` 体积大却完全不参与聚合——
// 单条 `detail` / `body_preview` 可能是数百字节到数 KB，去掉后单行内存降一个数量级，
// 因此聚合事件上限可以设在量级更高的位置（见 api.maxAuditAggregationEvents）。
//
// 注意：**关注批次详情**会把事件原样序列化给前端看证据（含 detail/body_preview/user_agent），
// 那条路径必须继续用全量投影 observabilityEventSelect。
const observabilityAggregateSelect = `SELECT source, source_event_id, occurred_at, actor, user_id, auth_source,
	action, entity_type, entity_key, repository, result, correlation_id, '' AS detail, error_class,
	http_method, http_path, status_code, duration_ms, client_ip, '' AS user_agent, '' AS request_id,
	'' AS token_preview, '' AS body_preview, '' AS actor_email`

// hasEventFilters 判断是否存在外层筛选。下推 LIMIT 的优化**仅当无外层过滤**时正确
// （有过滤时源内前 K 名会被外层滤掉，导致漏数据）——判断保守：任一条件出现即视为有过滤，
// 回退到通用查询（此时过滤后行数已小，排序代价可控）。
func hasEventFilters(f ObservabilityFilter) bool {
	return f.Actor != "" || f.Repository != "" || f.Query != "" || f.Method != "" ||
		f.Action != "" || f.Email != "" || f.ClientIP != "" || f.AuthSource != "" ||
		f.Attention == "pending" || f.Attention == "acknowledged" || len(f.Categories) > 0
}

// observabilityEventsQueryLimited 把「每源按时间倒序取前 perSourceLimit 条」下推到各子查询
// （走时间索引、取满即停），外层只归并排序 ≤2×perSourceLimit 行。30 天默认视图由此从
// 「全量物化 + TEMP B-TREE 排序」（实测查询计划）降为毫秒级——审计中心「切 30d 卡死」的根治点。
//
// 正确性：全局第 K 名必属于某源的「前 K 名」（K = offset+limit）；仅无过滤时成立（见 hasEventFilters）。
// 返回 ok=false 时调用方应回退 observabilityEventsQuery。
func observabilityEventsQueryLimited(f ObservabilityFilter, selectClause string, perSourceLimit int) (string, []any, bool) {
	if hasEventFilters(f) {
		return "", nil, false
	}
	query := `WITH observability_events AS (
		SELECT * FROM (
			SELECT 'audit' AS source, id AS source_event_id, ts AS occurred_at, actor, user_id, auth_source,
				action, entity_type, entity_key, repo AS repository, result, correlation_id, detail, '' AS error_class,
				http_method, http_path, status_code, duration_ms, ip AS client_ip, user_agent, request_id,
				token_preview, body_preview, actor_email
			FROM audit_log
			WHERE (source_node = '' OR source_node = ?) AND id <= ? AND ts >= ? AND ts < ?
			ORDER BY ts DESC LIMIT ?
		)
	`
	args := []any{f.SourceNode, f.AuditMaxID, f.From, f.To, perSourceLimit}
	if !f.AuditOnly {
		query += `	UNION ALL
		SELECT * FROM (
			SELECT 'replication' AS source, id AS source_event_id, occurred_at, source_actor AS actor, source_user_id AS user_id,
				source_auth_source AS auth_source, op AS action, entity_type, entity_key, '' AS repository, result,
				operation_id AS correlation_id, '' AS detail, error_class,
				'' AS http_method, '' AS http_path, 0 AS status_code, 0 AS duration_ms, '' AS client_ip,
				'' AS user_agent, '' AS request_id, '' AS token_preview, '' AS body_preview, '' AS actor_email
			FROM replication_apply_event
			WHERE id <= ? AND occurred_at >= ? AND occurred_at < ?
			ORDER BY occurred_at DESC LIMIT ?
		)
	`
		args = append(args, f.ReplicationMaxID, f.From, f.To, perSourceLimit)
	}
	query += `)
	` + selectClause + ` FROM observability_events`
	return query, args, true
}

func observabilityEventsQuery(f ObservabilityFilter, selectClause string) (string, []any) {
	query := `WITH observability_events AS (
		SELECT 'audit' AS source, id AS source_event_id, ts AS occurred_at, actor, user_id, auth_source,
			action, entity_type, entity_key, repo AS repository, result, correlation_id, detail, '' AS error_class,
			http_method, http_path, status_code, duration_ms, ip AS client_ip, user_agent, request_id,
			token_preview, body_preview, actor_email
		FROM audit_log
		WHERE (source_node = '' OR source_node = ?) AND id <= ? AND ts >= ? AND ts < ?
	`
	args := []any{f.SourceNode, f.AuditMaxID, f.From, f.To}
	if !f.AuditOnly {
		query += `	UNION ALL
		SELECT 'replication' AS source, id AS source_event_id, occurred_at, source_actor AS actor, source_user_id AS user_id,
			source_auth_source AS auth_source, op AS action, entity_type, entity_key, '' AS repository, result,
			operation_id AS correlation_id, '' AS detail, error_class,
			'' AS http_method, '' AS http_path, 0 AS status_code, 0 AS duration_ms, '' AS client_ip,
			'' AS user_agent, '' AS request_id, '' AS token_preview, '' AS body_preview, '' AS actor_email
		FROM replication_apply_event
		WHERE id <= ? AND occurred_at >= ? AND occurred_at < ?
	`
		args = append(args, f.ReplicationMaxID, f.From, f.To)
	}
	query += `)
	` + selectClause + ` FROM observability_events WHERE 1=1`
	if f.Actor != "" {
		query += " AND actor = ?"
		args = append(args, f.Actor)
	}
	if f.Repository != "" {
		query += " AND repository = ?"
		args = append(args, f.Repository)
	}
	if f.Query != "" {
		needle := "%" + strings.ToLower(strings.TrimSpace(f.Query)) + "%"
		query += " AND (LOWER(action) LIKE ? OR LOWER(detail) LIKE ? OR LOWER(entity_key) LIKE ?" +
			" OR LOWER(entity_type) LIKE ? OR LOWER(actor) LIKE ? OR LOWER(actor_email) LIKE ?" +
			" OR LOWER(http_path) LIKE ? OR LOWER(client_ip) LIKE ? OR LOWER(user_agent) LIKE ?)"
		args = append(args, needle, needle, needle, needle, needle, needle, needle, needle, needle)
	}
	if f.Method != "" {
		query += " AND http_method = ?"
		args = append(args, strings.ToUpper(strings.TrimSpace(f.Method)))
	}
	if f.Action != "" {
		query += " AND action = ?"
		args = append(args, f.Action)
	}
	if f.Email != "" {
		query += " AND actor_email = ?"
		args = append(args, f.Email)
	}
	if f.ClientIP != "" {
		query += " AND client_ip LIKE ?"
		args = append(args, strings.TrimSpace(f.ClientIP)+"%")
	}
	if f.AuthSource != "" {
		query += " AND auth_source = ?"
		args = append(args, f.AuthSource)
	}
	if f.Attention == "pending" || f.Attention == "acknowledged" {
		const ackExists = `EXISTS (SELECT 1 FROM audit_attention_ack ack
			WHERE ack.source = observability_events.source AND ack.source_event_id = observability_events.source_event_id)`
		if f.Attention == "pending" {
			query += " AND NOT " + ackExists
		} else {
			query += " AND " + ackExists
		}
	}
	if clause, values := observabilityCategoryClause(f.Categories); clause != "" {
		query += " AND (" + clause + ")"
		args = append(args, values...)
	}
	if clause, values := observabilityResultClause(f.Results); clause != "" {
		query += " AND (" + clause + ")"
		args = append(args, values...)
	}
	return query, args
}

func observabilityCategoryClause(categories []string) (string, []any) {
	clauses := make([]string, 0, len(categories))
	for _, category := range categories {
		switch category {
		case "replication":
			clauses = append(clauses, "source = 'replication'")
		case "security_event":
			clauses = append(clauses, "source = 'audit' AND result IN ('failed', 'error', 'rejected')")
		case "asset_change":
			clauses = append(clauses, "source = 'audit' AND result NOT IN ('failed', 'error', 'rejected') AND (action LIKE 'asset.%' OR action LIKE '%.publish' OR action LIKE '%.unpublish')")
		case "management_change":
			clauses = append(clauses, "source = 'audit' AND result NOT IN ('failed', 'error', 'rejected') AND action NOT LIKE 'asset.%' AND action NOT LIKE '%.publish' AND action NOT LIKE '%.unpublish'")
		}
	}
	return strings.Join(clauses, " OR "), nil
}

func observabilityResultClause(results []string) (string, []any) {
	clauses := make([]string, 0, len(results))
	for _, result := range results {
		switch result {
		case "success":
			clauses = append(clauses, "result IN ('ok', 'applied', 'success')")
		case "failure":
			clauses = append(clauses, "result IN ('failed', 'error', 'rejected')")
		case "pending":
			clauses = append(clauses, "result = 'pending'")
		case "unknown":
			clauses = append(clauses, "result NOT IN ('ok', 'applied', 'success', 'failed', 'error', 'rejected', 'pending')")
		}
	}
	return strings.Join(clauses, " OR "), nil
}

// EventByID 返回指定不可变事件；事件标识由调用方先验证来源与数值。
func (r *AuditObservabilityRepo) EventByID(source string, id int64, sourceNode string) (*ObservabilityEvent, error) {
	var event ObservabilityEvent
	var err error
	switch source {
	case "audit":
		err = r.db.Get(&event, `SELECT 'audit' AS source, id AS source_event_id, ts AS occurred_at, actor, user_id, auth_source,
			action, entity_type, entity_key, repo AS repository, result, correlation_id, detail, '' AS error_class,
			http_method, http_path, status_code, duration_ms, ip AS client_ip, user_agent, request_id,
			token_preview, body_preview, actor_email
			FROM audit_log WHERE id = ? AND (source_node = '' OR source_node = ?)`, id, sourceNode)
	case "replication":
		err = r.db.Get(&event, `SELECT 'replication' AS source, id AS source_event_id, occurred_at, source_actor AS actor,
			source_user_id AS user_id, source_auth_source AS auth_source, op AS action, entity_type, entity_key,
			'' AS repository, result, operation_id AS correlation_id, '' AS detail, error_class,
			'' AS http_method, '' AS http_path, 0 AS status_code, 0 AS duration_ms, '' AS client_ip,
			'' AS user_agent, '' AS request_id, '' AS token_preview, '' AS body_preview, '' AS actor_email
			FROM replication_apply_event WHERE id = ?`, id)
	default:
		return nil, ErrNotFound
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &event, nil
}

// Acknowledgements 返回成员已确认的身份快照。source+id 为内部编码，永不由客户端直接传入。
func (r *AuditObservabilityRepo) Acknowledgements(events []ObservabilityEvent) (map[string]AuditAttentionAck, error) {
	return r.acknowledgements(r.db, events)
}

func (r *AuditObservabilityRepo) acknowledgements(exec sqlx.Queryer, events []ObservabilityEvent) (map[string]AuditAttentionAck, error) {
	out := make(map[string]AuditAttentionAck, len(events))
	for _, part := range observabilityEventChunks(events) {
		keys, args := observabilityEventKeys(part)
		query := `SELECT source, source_event_id, acknowledged_by_user_id, acknowledged_by_username,
			acknowledged_by_auth_source, acknowledged_at FROM audit_attention_ack
			WHERE source || ':' || source_event_id IN (` + strings.Join(keys, ",") + `)`
		var rows []AuditAttentionAck
		if err := sqlx.Select(exec, &rows, query, args...); err != nil {
			return nil, err
		}
		for _, row := range rows {
			out[observabilityEventKey(row.Source, row.SourceEventID)] = row
		}
	}
	return out, nil
}

// Acknowledge 原子写入整批事件确认，并返回最先成功的确认人与本请求新增数。
func (r *AuditObservabilityRepo) Acknowledge(events []ObservabilityEvent, userID *int64, username, authSource, at string) (AuditAttentionAck, int, error) {
	return r.AcknowledgeAndAudit(events, userID, username, authSource, at, AuditLogEntry{})
}

// AcknowledgeAndAudit 在同一事务中保存全部确认行与确认审计，任一失败均不提交。
func (r *AuditObservabilityRepo) AcknowledgeAndAudit(events []ObservabilityEvent, userID *int64, username, authSource, at string, audit AuditLogEntry) (AuditAttentionAck, int, error) {
	if len(events) == 0 {
		return AuditAttentionAck{}, 0, ErrNotFound
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return AuditAttentionAck{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	newCount := 0
	for _, event := range events {
		res, err := tx.Exec(`INSERT INTO audit_attention_ack
			(source, source_event_id, acknowledged_by_user_id, acknowledged_by_username, acknowledged_by_auth_source, acknowledged_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(source, source_event_id) DO NOTHING`,
			event.Source, event.SourceEventID, userID, username, authSource, at)
		if err != nil {
			return AuditAttentionAck{}, 0, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return AuditAttentionAck{}, 0, err
		} else {
			newCount += int(n)
		}
	}
	acks, err := r.acknowledgements(tx, events)
	if err != nil {
		return AuditAttentionAck{}, 0, err
	}
	var first AuditAttentionAck
	for _, ack := range acks {
		if first.AcknowledgedAt == "" || ack.AcknowledgedAt < first.AcknowledgedAt || (ack.AcknowledgedAt == first.AcknowledgedAt && observabilityEventKey(ack.Source, ack.SourceEventID) < observabilityEventKey(first.Source, first.SourceEventID)) {
			first = ack
		}
	}
	if first.AcknowledgedAt == "" {
		return AuditAttentionAck{}, 0, fmt.Errorf("确认写入后缺少确认快照")
	}
	if audit.Action != "" {
		if err := insertAudit(tx, audit); err != nil {
			return AuditAttentionAck{}, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AuditAttentionAck{}, 0, err
	}
	return first, newCount, nil
}

func observabilityEventChunks(events []ObservabilityEvent) [][]ObservabilityEvent {
	const maxChunk = 500
	parts := make([][]ObservabilityEvent, 0, (len(events)+maxChunk-1)/maxChunk)
	for len(events) > 0 {
		n := maxChunk
		if len(events) < n {
			n = len(events)
		}
		parts = append(parts, events[:n])
		events = events[n:]
	}
	return parts
}

func observabilityEventKeys(events []ObservabilityEvent) ([]string, []any) {
	placeholders := make([]string, 0, len(events))
	args := make([]any, 0, len(events))
	for _, event := range events {
		placeholders = append(placeholders, "?")
		args = append(args, observabilityEventKey(event.Source, event.SourceEventID))
	}
	return placeholders, args
}

func observabilityEventKey(source string, id int64) string {
	return fmt.Sprintf("%s:%d", source, id)
}
