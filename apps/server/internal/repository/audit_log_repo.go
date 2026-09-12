package repository

import (
	"database/sql"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// AuditLogEntry 是 audit_log 表的行模型：一条管理写操作的审计记录（FR-38）。
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
	// ActorEmail 是记录时固化的操作者邮箱快照（账号未绑定邮箱时为空）。
	ActorEmail string `db:"actor_email" json:"actorEmail"`
}

// AuditLogRepo 读写 audit_log 表（审计日志，FR-38）。
type AuditLogRepo struct{ db *persistence.DB }

// NewAuditLogRepo 构造 AuditLogRepo。
func NewAuditLogRepo(db *persistence.DB) *AuditLogRepo { return &AuditLogRepo{db: db} }

// AuditFilter 是审计日志查询筛选（FR-38）：全可选，空值不参与过滤。
type AuditFilter struct {
	Actor      string // 操作者（精确）
	UserID     string // 稳定用户 ID（精确）
	AuthSource string // 认证来源（精确）
	TokenID    string // Token ID（精确）
	Action     string // 操作类型（精确，如 asset.put）
	Repo       string // 关联仓库（精确）
	Result     string // 结果（精确，如 ok/error）
	IP         string // 来源 IP（精确）
	From       string // 起始时间（RFC3339Nano，含）
	To         string // 结束时间（RFC3339Nano，含）
	Limit      int    // 分页上限（≤0 默认 50，上限 200）
	Offset     int    // 分页偏移
}

// Insert 记录一条审计日志；记录失败不阻断业务写（审计尽力而为，见 FR-38）。
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
		 http_method, http_path, status_code, duration_ms, token_preview, body_preview, actor_email)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, e.Actor, e.Action, e.EntityType, e.EntityKey, e.Repo, e.Detail, e.Result, e.IP,
		e.UserID, e.AuthSource, e.TokenID, e.TokenName, e.UserAgent, e.RequestID, e.SourceNode, e.CorrelationID,
		e.HTTPMethod, e.HTTPPath, e.StatusCode, e.DurationMs, e.TokenPreview, e.BodyPreview, e.ActorEmail,
	)
	return err
}

// List 按时间倒序分页返回审计日志，支持筛选（FR-38）。
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
	if f.Repo != "" {
		where += " AND repo = ?"
		args = append(args, f.Repo)
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
		 http_method, http_path, status_code, duration_ms, token_preview, body_preview, actor_email
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

// Count 返回筛选条件下的审计日志总数（FR-38 分页用）。
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
	if f.Repo != "" {
		where += " AND repo = ?"
		args = append(args, f.Repo)
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
