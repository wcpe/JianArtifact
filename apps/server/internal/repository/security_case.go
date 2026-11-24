package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// SecurityCaseRow 是 security_case 的持久投影。
type SecurityCaseRow struct {
	CaseID        string `db:"case_id"`
	Title         string `db:"title"`
	State         string `db:"state"`
	Severity      string `db:"severity"`
	Resolution    string `db:"resolution"`
	TargetVersion int64  `db:"target_version"`
	OwnerUserID   *int64 `db:"owner_user_id"`
	OpenedBy      string `db:"opened_by"`
	Reason        string `db:"reason"`
	CreatedUnix   int64  `db:"created_unix"`
	UpdatedUnix   int64  `db:"updated_unix"`
	ClosedUnix    *int64 `db:"closed_unix"`
}

// SecurityCaseLinkRow 案件↔事件关联（fact 已采信 / candidate 候选）。
type SecurityCaseLinkRow struct {
	CaseID     string `db:"case_id"`
	EventID    string `db:"event_id"`
	LinkKind   string `db:"link_kind"`
	LinkedBy   string `db:"linked_by"`
	LinkedUnix int64  `db:"linked_unix"`
}

func InsertSecurityCaseTx(ctx context.Context, tx *sqlx.Tx, r SecurityCaseRow) error {
	_, err := tx.NamedExecContext(ctx, `INSERT INTO security_case
 (case_id,title,state,severity,resolution,target_version,owner_user_id,opened_by,reason,created_unix,updated_unix,closed_unix)
 VALUES (:case_id,:title,:state,:severity,:resolution,:target_version,:owner_user_id,:opened_by,:reason,:created_unix,:updated_unix,:closed_unix)`, r)
	return err
}

func GetSecurityCaseTx(ctx context.Context, tx *sqlx.Tx, caseID string) (*SecurityCaseRow, error) {
	var r SecurityCaseRow
	if err := tx.GetContext(ctx, &r, `SELECT * FROM security_case WHERE case_id=?`, caseID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// TransitionSecurityCaseTx CAS 推进案件状态；版本不符返回 false。
func TransitionSecurityCaseTx(ctx context.Context, tx *sqlx.Tx, caseID string, expectVersion int64, state, resolution string, owner *int64, closedAt *int64, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE security_case SET state=?, resolution=?, owner_user_id=?, closed_unix=?, target_version=target_version+1, updated_unix=?
 WHERE case_id=? AND target_version=?`, state, resolution, owner, closedAt, now, caseID, expectVersion)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func ListSecurityCasesTx(ctx context.Context, tx *sqlx.Tx, state string, limit int) ([]SecurityCaseRow, error) {
	var out []SecurityCaseRow
	if state == "" {
		return out, tx.SelectContext(ctx, &out, `SELECT * FROM security_case ORDER BY updated_unix DESC LIMIT ?`, limit)
	}
	return out, tx.SelectContext(ctx, &out, `SELECT * FROM security_case WHERE state=? ORDER BY updated_unix DESC LIMIT ?`, state, limit)
}

func LinkSecurityCaseEventTx(ctx context.Context, tx *sqlx.Tx, l SecurityCaseLinkRow) error {
	_, err := tx.NamedExecContext(ctx, `INSERT OR IGNORE INTO security_case_link
 (case_id,event_id,link_kind,linked_by,linked_unix) VALUES (:case_id,:event_id,:link_kind,:linked_by,:linked_unix)`, l)
	return err
}

func ListSecurityCaseLinksTx(ctx context.Context, tx *sqlx.Tx, caseID string) ([]SecurityCaseLinkRow, error) {
	var out []SecurityCaseLinkRow
	return out, tx.SelectContext(ctx, &out, `SELECT * FROM security_case_link WHERE case_id=? ORDER BY linked_unix`, caseID)
}

// SecurityCaseReadRow 案件既読（本人・按版本冪等）。
type SecurityCaseReadRow struct {
	CaseID      string `db:"case_id"`
	UserID      int64  `db:"user_id"`
	SeenVersion int64  `db:"seen_version"`
	ReadUnix    int64  `db:"read_unix"`
}

// MarkCaseReadTx 将本人已读水位推进到 max(seen_version)（同版本重放幂等）。
func MarkCaseReadTx(ctx context.Context, tx *sqlx.Tx, caseID string, userID, seenVersion, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO security_case_read (case_id,user_id,seen_version,read_unix)
		VALUES (?,?,?,?)
		ON CONFLICT (case_id,user_id) DO UPDATE SET seen_version=MAX(seen_version,excluded.seen_version), read_unix=excluded.read_unix`,
		caseID, userID, seenVersion, now); err != nil {
		return err
	}
	return nil
}

// CaseReadTx 返回本人已读水位（无记录则 seen_version=0）。
func CaseReadTx(ctx context.Context, tx *sqlx.Tx, caseID string, userID int64) (SecurityCaseReadRow, error) {
	var r SecurityCaseReadRow
	err := tx.GetContext(ctx, &r, `SELECT case_id,user_id,seen_version,read_unix FROM security_case_read WHERE case_id=? AND user_id=?`, caseID, userID)
	return r, err
}

// ---- 安全治理 案件四栏详情 ----

// SecurityCaseTimelineRow timeline 栏：关联事件按时序。
type SecurityCaseTimelineRow struct {
	Sequence       int64  `db:"sequence"`
	EventID        string `db:"event_id"`
	EventType      string `db:"event_type"`
	OccurredAt     int64  `db:"occurred_at"`
	Outcome        string `db:"outcome"`
	ErrorCode      string `db:"error_code"`
	ActorUserID    *int64 `db:"actor_user_id"`
	ClientIPFamily int    `db:"client_ip_family"`
	ClientIP       []byte `db:"client_ip"`
	LinkKind       string `db:"link_kind"`
}

// CaseTimelineTx 返回案件关联事件（按 occurred_at,sequence）。
func CaseTimelineTx(ctx context.Context, tx *sqlx.Tx, caseID string, limit int) ([]SecurityCaseTimelineRow, error) {
	var out []SecurityCaseTimelineRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT e.sequence, e.event_id, e.event_type, e.occurred_at, e.outcome, e.error_code,
		        e.actor_user_id, e.client_ip_family, e.client_ip, l.link_kind
		   FROM security_case_link l JOIN security_event e ON e.event_id = l.event_id
		  WHERE l.case_id=? ORDER BY e.occurred_at, e.sequence LIMIT ?`, caseID, limit)
}

// SecurityCaseUploadRow uploads 栏：关联事件的对象明细。
type SecurityCaseUploadRow struct {
	EventID                string `db:"event_id"`
	EventType              string `db:"event_type"`
	CanonicalPath          string `db:"canonical_path"`
	ObjectKind             string `db:"object_kind"`
	ObjectID               string `db:"object_id"`
	Digest                 string `db:"digest"`
	SizeBytes              *int64 `db:"size_bytes"`
	ManifestDigest         string `db:"manifest_digest"`
	TargetVersion          string `db:"target_version"`
	RepositoryNameSnapshot string `db:"repository_name_snapshot"`
	LinkKind               string `db:"link_kind"`
}

// CaseUploadsTx 返回案件关联事件的对象（上传清单证据）。
func CaseUploadsTx(ctx context.Context, tx *sqlx.Tx, caseID string, limit int) ([]SecurityCaseUploadRow, error) {
	var out []SecurityCaseUploadRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT e.event_id, e.event_type, o.canonical_path, o.object_kind, o.object_id, o.digest,
		        o.size_bytes, o.manifest_digest, o.target_version, o.repository_name_snapshot, l.link_kind
		   FROM security_case_link l
		   JOIN security_event e ON e.event_id = l.event_id
		   JOIN security_event_object o ON o.event_sequence = e.sequence
		  WHERE l.case_id=? ORDER BY e.occurred_at, o.ordinal LIMIT ?`, caseID, limit)
}

// SecurityCaseIdentityRow identity 栏：关联事件的主体/来源去重组。
type SecurityCaseIdentityRow struct {
	ActorUserID       *int64 `db:"actor_user_id"`
	ActorNameSnapshot string `db:"actor_name_snapshot"`
	CredentialID      string `db:"credential_id"`
	SessionID         string `db:"session_id"`
	AuthMethod        string `db:"auth_method"`
	AuthStrength      string `db:"auth_strength"`
	ClientIPFamily    int    `db:"client_ip_family"`
	ClientIP          []byte `db:"client_ip"`
	UserAgent         string `db:"user_agent"`
	Events            int    `db:"events"`
}

// CaseIdentityTx 按主体×凭据×会话×来源去重统计关联事件。
func CaseIdentityTx(ctx context.Context, tx *sqlx.Tx, caseID string, limit int) ([]SecurityCaseIdentityRow, error) {
	var out []SecurityCaseIdentityRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT e.actor_user_id, e.actor_name_snapshot, e.credential_id, e.session_id,
		        e.auth_method, e.auth_strength, e.client_ip_family, e.client_ip, e.user_agent,
		        count(*) AS events
		   FROM security_case_link l JOIN security_event e ON e.event_id = l.event_id
		  WHERE l.case_id=?
		  GROUP BY e.actor_user_id, e.credential_id, e.session_id, e.client_ip, e.user_agent
		  ORDER BY events DESC LIMIT ?`, caseID, limit)
}

// CaseFindingsTx 返回案件附着的规则命中。
func CaseFindingsTx(ctx context.Context, tx *sqlx.Tx, caseID string) ([]SecurityFindingRow, error) {
	var out []SecurityFindingRow
	return out, tx.SelectContext(ctx, &out,
		`SELECT finding_id,rule_id,rule_version,severity,confidence,subject_key,window_from,window_to,
		        summary,COALESCE(case_id,'') AS case_id,created_unix
		   FROM security_finding WHERE case_id=? ORDER BY created_unix`, caseID)
}

// CaseEvidenceHoldsTx 返回覆盖案件对象摘要的保全（按 digest 匹配）。
func CaseEvidenceHoldsTx(ctx context.Context, tx *sqlx.Tx, caseID string, limit int) ([]EvidenceHold, error) {
	var out []EvidenceHold
	return out, tx.SelectContext(ctx, &out,
		`SELECT DISTINCT h.* FROM evidence_hold h
		  JOIN security_case_link l ON l.case_id=?
		  JOIN security_event e ON e.event_id=l.event_id
		  JOIN security_event_object o ON o.event_sequence=e.sequence
		  WHERE h.digest=o.digest OR h.digest='sha256:'||o.digest
		  ORDER BY h.created_at LIMIT ?`, caseID, limit)
}
