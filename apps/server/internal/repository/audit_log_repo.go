package repository

import (
	"database/sql"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AuditLogEntry 是 audit_log 表的行模型：一条管理写操作的审计记录。
type AuditLogEntry struct {
	ID            int64  `db:"id" json:"id"`
	TS            string `db:"ts" json:"ts"`
	Actor         string `db:"actor" json:"actor"`
	Action        string `db:"action" json:"action"`
	EntityType    string `db:"entity_type" json:"entityType"`
	EntityKey     string `db:"entity_key" json:"entityKey"`
	Repo          string `db:"repo" json:"repo"`
	Detail        string `db:"detail" json:"detail"`
	Result        string `db:"result" json:"result"`
	IP            string `db:"ip" json:"ip"`
	UserID        *int64 `db:"user_id" json:"userId,omitempty"`
	AuthSource    string `db:"auth_source" json:"authSource"`
	TokenID       *int64 `db:"token_id" json:"tokenId,omitempty"`
	TokenName     string `db:"token_name" json:"tokenName,omitempty"`
	UserAgent     string `db:"user_agent" json:"userAgent"`
	RequestID     string `db:"request_id" json:"requestId"`
	SourceNode    string `db:"source_node" json:"sourceNode"`
	CorrelationID string `db:"correlation_id" json:"correlationId"`
	// HTTPMethod/HTTPPath/StatusCode/DurationMs 为脱敏后的请求上下文（路由模板，不含查询串）。
	HTTPMethod   string `db:"http_method" json:"httpMethod"`
	HTTPPath     string `db:"http_path" json:"httpPath"`
	StatusCode   int    `db:"status_code" json:"statusCode"`
	DurationMs   int64  `db:"duration_ms" json:"durationMs"`
	TokenPreview string `db:"token_preview" json:"tokenPreview,omitempty"`
	BodyPreview  string `db:"body_preview" json:"bodyPreview,omitempty"`
	// HTTPHeaders 是常用白名单请求头的紧凑 JSON 文本（已过滤敏感头、单值截断；空串表示未捕获）。
	HTTPHeaders string `db:"http_headers" json:"httpHeaders,omitempty"`
	// DurationServerMs 是单段服务端耗时（进入业务路由处理到审计落笔，毫秒；
	// 口径见 auditctx.ServerTimingMiddleware；未进入契约路由时为 0）。
	DurationServerMs int64 `db:"duration_server_ms" json:"durationServerMs"`
	// ActorEmail 是记录时固化的操作者邮箱快照（账号未绑定邮箱时为空）。
	ActorEmail string `db:"actor_email" json:"actorEmail"`
}

// AuditLogRepo 读写 audit_log 表（审计日志）。
type AuditLogRepo struct{ db *persistence.DB }

// SettingAuditAction 是设置写入的审计动作名。
const SettingAuditAction = "setting.set"

// settingHistorySep 分隔 detail 中的旧值与新值。
//
// 用 ASCII Unit Separator（0x1F）而不是冒号或竖线：设置值里可能出现任意可打印字符
// （public_url 含 ':://'、白名单是逗号串），用可打印分隔符会让解析出现歧义。
// 0x1F 不可能出现在这些值里（HTTP 头与 JSON 都会转义控制字符）。
const settingHistorySep = "\x1f"

// EncodeSettingHistoryDetail 把「旧值 → 新值」编码成可回读的审计 detail。
//
// 值 ≤0 视为「该项此前没有存储值」，用空串表示，界面上显示为「未设置」。
func EncodeSettingHistoryDetail(oldVal, newVal string) string {
	return oldVal + settingHistorySep + newVal
}

// decodeSettingHistoryDetail 从审计 detail 还原前后值。
//
// 解析不出分隔符时返回两个空串：历史记录宁可显示「未知」，
// 也不要把一条格式不符的 detail 当作真实取值展示给运维。
func decodeSettingHistoryDetail(detail string) (string, string) {
	oldVal, newVal, ok := strings.Cut(detail, settingHistorySep)
	if !ok {
		return "", ""
	}
	return oldVal, newVal
}

// SettingHistoryEntry 是一条设置变更历史（供设置页逐项展示）。
//
// 只抽取展示所需字段：设置历史是给运维看「谁在什么时候把值改成了什么」，
// 不需要审计原文里的 IP、User-Agent 等排障字段。
type SettingHistoryEntry struct {
	TS     string `json:"ts"`
	Actor  string `json:"actor"`
	OldVal string `json:"oldValue"`
	NewVal string `json:"newValue"`
}

// ListSettingHistory 按设置键查询最近的变更历史（新→旧）。
//
// 数据源是 audit_log：设置写入会逐键落一条 action='setting.set'、
// entity_type='setting'、entity_key=<设置键> 的记录，前后值编码在 detail 里
// （格式见 encodeSettingHistoryDetail）。
func (r *AuditLogRepo) ListSettingHistory(key string, limit int) ([]SettingHistoryEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	const query = `SELECT ts, actor, detail FROM audit_log
		WHERE action = ? AND entity_type = ? AND entity_key = ?
		ORDER BY ts DESC, id DESC LIMIT ?`
	var rows []struct {
		TS     string `db:"ts"`
		Actor  string `db:"actor"`
		Detail string `db:"detail"`
	}
	if err := r.db.Select(&rows, query, SettingAuditAction, "setting", key, limit); err != nil {
		return nil, err
	}
	out := make([]SettingHistoryEntry, 0, len(rows))
	for _, row := range rows {
		oldVal, newVal := decodeSettingHistoryDetail(row.Detail)
		out = append(out, SettingHistoryEntry{
			TS: row.TS, Actor: row.Actor, OldVal: oldVal, NewVal: newVal,
		})
	}
	return out, nil
}

// NewAuditLogRepo 构造 AuditLogRepo。
func NewAuditLogRepo(db *persistence.DB) *AuditLogRepo { return &AuditLogRepo{db: db} }

// AuditFilter 是审计日志查询筛选：全可选，空值不参与过滤。
type AuditFilter struct {
	Actor      string // 操作者（精确）
	UserID     string // 稳定用户 ID（精确）
	AuthSource string // 认证来源（精确）
	TokenID    string // Token ID（精确）
	Action     string // 操作类型（精确，如 asset.put）
	// Repos 关联仓库：按「主名 ∪ 别名」全集过滤（ADR-0028 要求按仓库维度的检索处理全集，
	// 否则重命名后按新主名检索会漏掉记在旧名下的历史事件）；空表示不按仓库过滤。
	Repos  []string
	Result string // 结果（精确，如 ok/error）
	IP     string // 来源 IP（精确）
	From   string // 起始时间（RFC3339Nano，含）
	To     string // 结束时间（RFC3339Nano，含）
	Limit  int    // 分页上限（≤0 默认 50，上限 200）
	Offset int    // 分页偏移
}

// Insert 记录一条审计日志；记录失败不阻断业务写（审计尽力而为，失败只影响可追溯性）。
func (r *AuditLogRepo) Insert(e AuditLogEntry) error {
	return insertAudit(r.db, e)
}

// InsertTx 在调用方事务内写入审计，供原子资产操作与审计同成同败。
func (r *AuditLogRepo) InsertTx(tx *sqlx.Tx, e AuditLogEntry) error {
	return insertAudit(tx, e)
}

type auditExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertAudit(exec auditExecutor, e AuditLogEntry) error {
	_, err := exec.Exec(
		`INSERT INTO audit_log (ts, actor, action, entity_type, entity_key, repo, detail, result, ip,
		 user_id, auth_source, token_id, token_name, user_agent, request_id, source_node, correlation_id,
		 http_method, http_path, status_code, duration_ms, token_preview, body_preview, actor_email,
		 http_headers, duration_server_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, e.Actor, e.Action, e.EntityType, e.EntityKey, e.Repo, e.Detail, e.Result, e.IP,
		e.UserID, e.AuthSource, e.TokenID, e.TokenName, e.UserAgent, e.RequestID, e.SourceNode, e.CorrelationID,
		e.HTTPMethod, e.HTTPPath, e.StatusCode, e.DurationMs, e.TokenPreview, e.BodyPreview, e.ActorEmail,
		e.HTTPHeaders, e.DurationServerMs,
	)
	return err
}

// List 按时间倒序分页返回审计日志，支持筛选。
func (r *AuditLogRepo) List(f AuditFilter) ([]AuditLogEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	where := ""
	args := []any{}
	if f.Actor != "" {
		where += " AND actor = ?"
		args = append(args, f.Actor)
	}
	if f.UserID != "" {
		where += " AND user_id = ?"
		args = append(args, f.UserID)
	}
	if f.AuthSource != "" {
		where += " AND auth_source = ?"
		args = append(args, f.AuthSource)
	}
	if f.TokenID != "" {
		where += " AND token_id = ?"
		args = append(args, f.TokenID)
	}
	if f.Action != "" {
		where += " AND action = ?"
		args = append(args, f.Action)
	}
	if len(f.Repos) > 0 {
		clause, repoArgs, inErr := sqlx.In(" AND repo IN (?)", f.Repos)
		if inErr != nil {
			return nil, inErr
		}
		where += clause
		args = append(args, repoArgs...)
	}
	if f.Result != "" {
		where += " AND result = ?"
		args = append(args, f.Result)
	}
	if f.IP != "" {
		where += " AND ip = ?"
		args = append(args, f.IP)
	}
	if f.From != "" {
		where += " AND ts >= ?"
		args = append(args, f.From)
	}
	if f.To != "" {
		where += " AND ts <= ?"
		args = append(args, f.To)
	}
	query := `SELECT id, ts, actor, action, entity_type, entity_key, repo, detail, result, ip,
		 user_id, auth_source, token_id, token_name, user_agent, request_id, source_node, correlation_id,
		 http_method, http_path, status_code, duration_ms, token_preview, body_preview, actor_email,
		 http_headers, duration_server_ms
	          FROM audit_log WHERE 1=1` + where + ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, f.Offset)
	var entries []AuditLogEntry
	if err := r.db.Select(&entries, query, args...); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []AuditLogEntry{}
	}
	return entries, nil
}

// Count 返回筛选条件下的审计日志总数（分页用）。
func (r *AuditLogRepo) Count(f AuditFilter) (int, error) {
	where := ""
	args := []any{}
	if f.Actor != "" {
		where += " AND actor = ?"
		args = append(args, f.Actor)
	}
	if f.UserID != "" {
		where += " AND user_id = ?"
		args = append(args, f.UserID)
	}
	if f.AuthSource != "" {
		where += " AND auth_source = ?"
		args = append(args, f.AuthSource)
	}
	if f.TokenID != "" {
		where += " AND token_id = ?"
		args = append(args, f.TokenID)
	}
	if f.Action != "" {
		where += " AND action = ?"
		args = append(args, f.Action)
	}
	if len(f.Repos) > 0 {
		clause, repoArgs, inErr := sqlx.In(" AND repo IN (?)", f.Repos)
		if inErr != nil {
			return 0, inErr
		}
		where += clause
		args = append(args, repoArgs...)
	}
	if f.Result != "" {
		where += " AND result = ?"
		args = append(args, f.Result)
	}
	if f.IP != "" {
		where += " AND ip = ?"
		args = append(args, f.IP)
	}
	if f.From != "" {
		where += " AND ts >= ?"
		args = append(args, f.From)
	}
	if f.To != "" {
		where += " AND ts <= ?"
		args = append(args, f.To)
	}
	var n int
	if err := r.db.Get(&n, `SELECT COUNT(*) FROM audit_log WHERE 1=1`+where, args...); err != nil {
		return 0, err
	}
	return n, nil
}
