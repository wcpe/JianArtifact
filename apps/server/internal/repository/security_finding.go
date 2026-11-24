package repository

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// SecurityFindingRow 是规则命中的持久行。
type SecurityFindingRow struct {
	FindingID   string `db:"finding_id"`
	RuleID      string `db:"rule_id"`
	RuleVersion int    `db:"rule_version"`
	Severity    string `db:"severity"`
	Confidence  string `db:"confidence"`
	SubjectKey  string `db:"subject_key"`
	WindowFrom  int64  `db:"window_from"`
	WindowTo    int64  `db:"window_to"`
	Summary     string `db:"summary"`
	CaseID      string `db:"case_id"`
	CreatedUnix int64  `db:"created_unix"`
}

// SecurityFindingEventRow 是投影读取的最小事件行。
type SecurityFindingEventRow struct {
	Sequence       int64  `db:"sequence"`
	EventID        string `db:"event_id"`
	EventType      string `db:"event_type"`
	OccurredAt     int64  `db:"occurred_at"`
	ActorUserID    *int64 `db:"actor_user_id"`
	CredentialID   string `db:"credential_id"`
	SessionID      string `db:"session_id"`
	ClientIPFamily int    `db:"client_ip_family"`
	ClientIP       []byte `db:"client_ip"`
	Outcome        string `db:"outcome"`
	Committed      string `db:"committed"`
	ErrorCode      string `db:"error_code"`
	OperationID    string `db:"operation_id"`
}

// FindingWatermarkTx 返回投影消费水位（无则 0）。
func FindingWatermarkTx(ctx context.Context, tx *sqlx.Tx) (int64, error) {
	var seq int64
	err := tx.GetContext(ctx, &seq,
		`SELECT last_sequence FROM security_finding_watermark WHERE id=1`)
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// InitFindingWatermarkTx 未初始化时创建水位行。
func InitFindingWatermarkTx(ctx context.Context, tx *sqlx.Tx) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO security_finding_watermark (id,last_sequence,updated_unix) VALUES (1,0,0)`)
	return err
}

// SetFindingWatermarkTx 在批次结算后推进水位。
func SetFindingWatermarkTx(ctx context.Context, tx *sqlx.Tx, lastSeq, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE security_finding_watermark SET last_sequence=?, updated_unix=? WHERE id=1`, lastSeq, now)
	return err
}

// EventsAfterSeqTx 按 sequence 顺序返回水位之后的最小事件。
func EventsAfterSeqTx(ctx context.Context, tx *sqlx.Tx, after int64, limit int) ([]SecurityFindingEventRow, error) {
	var out []SecurityFindingEventRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT sequence, event_id, event_type, occurred_at, actor_user_id, credential_id, session_id,
		        client_ip_family, client_ip, outcome, committed, error_code, operation_id
		   FROM security_event WHERE sequence>? ORDER BY sequence LIMIT ?`, after, limit)
}

// CountDeniedByActorTx 统计 主体×窗口 的拒绝件数（规则 subject_denial_burst）。
func CountDeniedByActorTx(ctx context.Context, tx *sqlx.Tx, col string, val any, from, to int64) (int, error) {
	var n int
	q := `SELECT count(*) FROM security_event WHERE outcome='denied' AND occurred_at>=? AND occurred_at<=? AND ` + col + `=?`
	return n, tx.GetContext(ctx, &n, q, from, to, val)
}

// DeniedFromOtherIPBeforeTx 统计同一主体在其他 client_ip 被拒的先前事件数（规则 denied_then_committed）。
func DeniedFromOtherIPTx(ctx context.Context, tx *sqlx.Tx, col string, val any, from, to int64, ipFamily int, ip []byte) (int, error) {
	var n int
	q := `SELECT count(*) FROM security_event WHERE outcome='denied' AND occurred_at>=? AND occurred_at<=?
	        AND ` + col + `=? AND NOT (client_ip_family=? AND client_ip=?)`
	return n, tx.GetContext(ctx, &n, q, from, to, val, ipFamily, ip)
}

// SubjectSeenFromIPTx 判断主体是否曾用此 client_ip（规则 sensitive_change_new_source）。
func SubjectSeenFromIPTx(ctx context.Context, tx *sqlx.Tx, col string, val any, before int64, ipFamily int, ip []byte) (int, error) {
	var n int
	q := `SELECT count(*) FROM security_event WHERE occurred_at<? AND ` + col + `=? AND client_ip_family=? AND client_ip=?`
	return n, tx.GetContext(ctx, &n, q, before, val, ipFamily, ip)
}

// InsertFindingTx 幂等 INSERT（重算不重复）。新登记返回 true。
func InsertFindingTx(ctx context.Context, tx *sqlx.Tx, r SecurityFindingRow) (bool, error) {
	res, err := tx.NamedExecContext(ctx, `INSERT OR IGNORE INTO security_finding
 (finding_id,rule_id,rule_version,severity,confidence,subject_key,window_from,window_to,summary,case_id,created_unix)
 VALUES (:finding_id,:rule_id,:rule_version,:severity,:confidence,:subject_key,:window_from,:window_to,:summary,NULLIF(:case_id,''),:created_unix)`, r)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// LinkFindingEventTx 幂等登记证据事件。
func LinkFindingEventTx(ctx context.Context, tx *sqlx.Tx, findingID string, eventSeq int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO security_finding_event (finding_id,event_sequence) VALUES (?,?)`, findingID, eventSeq)
	return err
}

// AttachFindingCaseTx 把命中关联到案件。
func AttachFindingCaseTx(ctx context.Context, tx *sqlx.Tx, findingID, caseID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE security_finding SET case_id=? WHERE finding_id=?`, caseID, findingID)
	return err
}

// OpenCaseForSubjectTx 返回同主体的未决案件（取含同系规则命中的优先，否则普通 open 案件）。
func OpenCaseForSubjectTx(ctx context.Context, tx *sqlx.Tx, subjectKey string) (string, error) {
	var id string
	err := tx.GetContext(ctx, &id,
		`SELECT f.case_id FROM security_finding f JOIN security_case c ON c.case_id=f.case_id
		  WHERE f.subject_key=? AND c.state='open' LIMIT 1`, subjectKey)
	return id, err
}

// ListFindingsTx 命中列表（按新到旧）。
func ListFindingsTx(ctx context.Context, tx *sqlx.Tx, limit int) ([]SecurityFindingRow, error) {
	var out []SecurityFindingRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT finding_id,rule_id,rule_version,severity,confidence,subject_key,window_from,window_to,summary,COALESCE(case_id,'') AS case_id,created_unix FROM security_finding ORDER BY created_unix DESC LIMIT ?`, limit)
}
