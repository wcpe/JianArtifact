package repository

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// SecurityFindingRow は規則命中の永続行。
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

// SecurityFindingEventRow は投影が読む最小イベント行。
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

// FindingWatermarkTx は投影消費水位（無ければ 0）。
func FindingWatermarkTx(ctx context.Context, tx *sqlx.Tx) (int64, error) {
	var seq int64
	err := tx.GetContext(ctx, &seq,
		`SELECT last_sequence FROM security_finding_watermark WHERE id=1`)
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// InitFindingWatermarkTx は未初期化時に水位行を作る。
func InitFindingWatermarkTx(ctx context.Context, tx *sqlx.Tx) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO security_finding_watermark (id,last_sequence,updated_unix) VALUES (1,0,0)`)
	return err
}

// SetFindingWatermarkTx はバッチ確定後に水位を前進させる。
func SetFindingWatermarkTx(ctx context.Context, tx *sqlx.Tx, lastSeq, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE security_finding_watermark SET last_sequence=?, updated_unix=? WHERE id=1`, lastSeq, now)
	return err
}

// EventsAfterSeqTx は水位以降の最小イベントを sequence 順で返す。
func EventsAfterSeqTx(ctx context.Context, tx *sqlx.Tx, after int64, limit int) ([]SecurityFindingEventRow, error) {
	var out []SecurityFindingEventRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT sequence, event_id, event_type, occurred_at, actor_user_id, credential_id, session_id,
		        client_ip_family, client_ip, outcome, committed, error_code, operation_id
		   FROM security_event WHERE sequence>? ORDER BY sequence LIMIT ?`, after, limit)
}

// CountDeniedByActorTx は主体×窓の拒否件数（規則 subject_denial_burst）。
func CountDeniedByActorTx(ctx context.Context, tx *sqlx.Tx, col string, val any, from, to int64) (int, error) {
	var n int
	q := `SELECT count(*) FROM security_event WHERE outcome='denied' AND occurred_at>=? AND occurred_at<=? AND ` + col + `=?`
	return n, tx.GetContext(ctx, &n, q, from, to, val)
}

// DeniedFromOtherIPBeforeTx は同一主体が別 client_ip で拒否された直前イベント数（規則 denied_then_committed）。
func DeniedFromOtherIPTx(ctx context.Context, tx *sqlx.Tx, col string, val any, from, to int64, ipFamily int, ip []byte) (int, error) {
	var n int
	q := `SELECT count(*) FROM security_event WHERE outcome='denied' AND occurred_at>=? AND occurred_at<=?
	        AND ` + col + `=? AND NOT (client_ip_family=? AND client_ip=?)`
	return n, tx.GetContext(ctx, &n, q, from, to, val, ipFamily, ip)
}

// SubjectSeenFromIPTx は主体がこの client_ip を過去に使ったことがあるか（規則 sensitive_change_new_source）。
func SubjectSeenFromIPTx(ctx context.Context, tx *sqlx.Tx, col string, val any, before int64, ipFamily int, ip []byte) (int, error) {
	var n int
	q := `SELECT count(*) FROM security_event WHERE occurred_at<? AND ` + col + `=? AND client_ip_family=? AND client_ip=?`
	return n, tx.GetContext(ctx, &n, q, before, val, ipFamily, ip)
}

// InsertFindingTx は冪等 INSERT（再計算で重複しない）。新規なら true。
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

// LinkFindingEventTx は証証拠イベントを冪等登録。
func LinkFindingEventTx(ctx context.Context, tx *sqlx.Tx, findingID string, eventSeq int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO security_finding_event (finding_id,event_sequence) VALUES (?,?)`, findingID, eventSeq)
	return err
}

// AttachFindingCaseTx は命中を案件に紐付ける。
func AttachFindingCaseTx(ctx context.Context, tx *sqlx.Tx, findingID, caseID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE security_finding SET case_id=? WHERE finding_id=?`, caseID, findingID)
	return err
}

// OpenCaseForSubjectTx は同主体の未決案件（同規則系の命中を持つもの優先でなく単純な open 案件）を返す。
func OpenCaseForSubjectTx(ctx context.Context, tx *sqlx.Tx, subjectKey string) (string, error) {
	var id string
	err := tx.GetContext(ctx, &id,
		`SELECT f.case_id FROM security_finding f JOIN security_case c ON c.case_id=f.case_id
		  WHERE f.subject_key=? AND c.state='open' LIMIT 1`, subjectKey)
	return id, err
}

// ListFindingsTx は命中一覧（新しい順）。
func ListFindingsTx(ctx context.Context, tx *sqlx.Tx, limit int) ([]SecurityFindingRow, error) {
	var out []SecurityFindingRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT finding_id,rule_id,rule_version,severity,confidence,subject_key,window_from,window_to,summary,COALESCE(case_id,'') AS case_id,created_unix FROM security_finding ORDER BY created_unix DESC LIMIT ?`, limit)
}
