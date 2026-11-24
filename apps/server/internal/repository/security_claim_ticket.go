package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// 认领票用途。
const (
	ClaimPurposeInstall = "claim_install"
	ClaimPurposeAdmin   = "claim_admin"
)

// SecurityClaimTicket 一次性认领票：绑定账号、代次、用途与期限；
// token_hash 存高熵不透明串的校验值（不回读明文），消费与认领同事务。
type SecurityClaimTicket struct {
	TicketID          string `db:"ticket_id"`
	UserID            *int64 `db:"user_id"`
	Purpose           string `db:"purpose"`
	TokenHash         string `db:"token_hash"`
	AccountEpoch      int64  `db:"account_epoch"`
	IssuedBy          string `db:"issued_by"`
	Reason            string `db:"reason"`
	AttemptsRemaining int    `db:"attempts_remaining"`
	ExpiresAt         int64  `db:"expires_at"`
	State             string `db:"state"`
	CreatedAt         int64  `db:"created_at"`
	ConsumedAt        *int64 `db:"consumed_at"`
}

// InsertClaimTicketTx 先取消同主体同用途旧票（重新出具使旧票不可用），再写新票。
func InsertClaimTicketTx(ctx context.Context, tx *sqlx.Tx, t SecurityClaimTicket) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE security_claim_ticket SET state=? WHERE state=? AND purpose=? AND
			((user_id IS NULL AND ? IS NULL) OR user_id=?)`,
		ChallengeStateCancelled, ChallengeStateIssued, t.Purpose, t.UserID, t.UserID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO security_claim_ticket
			(ticket_id,user_id,purpose,token_hash,account_epoch,issued_by,reason,
			 attempts_remaining,expires_at,state,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.TicketID, t.UserID, t.Purpose, t.TokenHash, t.AccountEpoch, t.IssuedBy, t.Reason,
		t.AttemptsRemaining, t.ExpiresAt, ChallengeStateIssued, t.CreatedAt)
	return err
}

// ClaimTicketByTokenHashTx 按校验值取票；不存在回 ErrNotFound（调用方统一映射）。
func ClaimTicketByTokenHashTx(ctx context.Context, tx *sqlx.Tx, tokenHash string) (SecurityClaimTicket, error) {
	var t SecurityClaimTicket
	err := sqlx.GetContext(ctx, tx, &t,
		`SELECT * FROM security_claim_ticket WHERE token_hash=?`, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return SecurityClaimTicket{}, ErrNotFound
	}
	return t, err
}

// TouchClaimTicketTx 校验票在可用态与期限内，否则就地结转过期态并拒绝。
func TouchClaimTicketTx(ctx context.Context, tx *sqlx.Tx, tokenHash, purpose string, now int64) (SecurityClaimTicket, error) {
	t, err := ClaimTicketByTokenHashTx(ctx, tx, tokenHash)
	if err != nil {
		return SecurityClaimTicket{}, err
	}
	if t.State != ChallengeStateIssued || (purpose != "" && t.Purpose != purpose) {
		return SecurityClaimTicket{}, ErrSecurityChallengeRejected
	}
	if t.ExpiresAt <= now {
		if _, err := tx.ExecContext(ctx,
			`UPDATE security_claim_ticket SET state=? WHERE ticket_id=? AND state=?`,
			ChallengeStateExpired, t.TicketID, ChallengeStateIssued); err != nil {
			return SecurityClaimTicket{}, err
		}
		return SecurityClaimTicket{}, ErrSecurityChallengeRejected
	}
	return t, nil
}

// ConsumeClaimTicketTx 一次性把在途票置为 consumed（认领兑现与因子激活同事务调用）。
func ConsumeClaimTicketTx(ctx context.Context, tx *sqlx.Tx, ticketID string, now int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE security_claim_ticket SET state=?,consumed_at=? WHERE ticket_id=? AND state=? AND expires_at>?`,
		ChallengeStateConsumed, now, ticketID, ChallengeStateIssued, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSecurityChallengeRejected
	}
	return nil
}

// FailClaimTicketAttemptTx 原子扣减一次尝试额度；归零同语置 exhausted。
// 返回 false 表示票已不在可尝试状态（含被并发耗尽）。
func FailClaimTicketAttemptTx(ctx context.Context, tx *sqlx.Tx, ticketID string, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE security_claim_ticket SET
			attempts_remaining=attempts_remaining-1,
			state=CASE WHEN attempts_remaining<=1 THEN ? ELSE state END
		 WHERE ticket_id=? AND state=? AND expires_at>? AND attempts_remaining>0`,
		ChallengeStateExhausted, ticketID, ChallengeStateIssued, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
