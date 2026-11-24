package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// SecurityCaseRow 是 security_case 的持久投影（FR-171）。
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
