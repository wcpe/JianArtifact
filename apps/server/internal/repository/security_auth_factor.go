package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// ErrSecurityChallengeRejected 表示挑战或因子在提交点已不可用：
// 过期、已消费、已取消、超额或代次不符——对外统一不区分原因。
var ErrSecurityChallengeRejected = errors.New("认证挑战已失效")

// 挑战终态与用途常量（与迁移 0061 的 CHECK 一致）。
const (
	ChallengePurposeLoginMFA      = "login_mfa"
	ChallengePurposeFactorEnroll  = "factor_enroll"
	ChallengePurposeFactorRecover = "factor_recover"
	ChallengePurposeStepUp        = "step_up"

	ChallengeStateIssued    = "issued"
	ChallengeStateConsumed  = "consumed"
	ChallengeStateExpired   = "expired"
	ChallengeStateCancelled = "cancelled"
	ChallengeStateExhausted = "exhausted"

	TotpFactorStatePending = "enrollment_pending"
	TotpFactorStateActive  = "active"
	TotpFactorStateRetired = "retired"
)

// SecurityAuthChallenge 是一次受限认证挑战的持久行；token_hash 只存校验值。
type SecurityAuthChallenge struct {
	ChallengeID       string `db:"challenge_id"`
	UserID            int64  `db:"user_id"`
	AccountEpoch      int64  `db:"account_epoch"`
	Purpose           string `db:"purpose"`
	TokenHash         string `db:"token_hash"`
	AttemptsRemaining int64  `db:"attempts_remaining"`
	ExpiresAt         int64  `db:"expires_at"`
	State             string `db:"state"`
	CreatedAt         int64  `db:"created_at"`
	ConsumedAt        *int64 `db:"consumed_at"`
	AuthSource        string `db:"auth_source"`
	SessionBinding    string `db:"session_binding"`
}

// SecurityTotpFactor 是一个 TOTP 因子的持久行；secret_ciphertext 不可解明文。
type SecurityTotpFactor struct {
	FactorID         string `db:"factor_id"`
	UserID           int64  `db:"user_id"`
	AccountEpoch     int64  `db:"account_epoch"`
	State            string `db:"state"`
	SecretCiphertext string `db:"secret_ciphertext"`
	KeyVersion       int64  `db:"key_version"`
	LastAcceptedStep int64  `db:"last_accepted_step"`
	CreatedAt        int64  `db:"created_at"`
	ExpiresAt        *int64 `db:"expires_at"`
	ActivatedAt      *int64 `db:"activated_at"`
	RetiredAt        *int64 `db:"retired_at"`
	ChallengeID      string `db:"challenge_id"`
}

// SecurityRecoveryCode 是一枚恢复码的持久行；code_hash 为独立不可逆校验值。
type SecurityRecoveryCode struct {
	CodeID     string `db:"code_id"`
	UserID     int64  `db:"user_id"`
	FactorID   string `db:"factor_id"`
	Batch      int64  `db:"batch"`
	CodeHash   string `db:"code_hash"`
	CreatedAt  int64  `db:"created_at"`
	ConsumedAt *int64 `db:"consumed_at"`
}

// SecurityAuthFactorRepo 管理认证挑战、TOTP 因子与恢复码的持久状态。
// 所有转移用条件更新裁决，并发请求至多一行成功。
type SecurityAuthFactorRepo struct {
	db *persistence.DB
}

func NewSecurityAuthFactorRepo(db *persistence.DB) *SecurityAuthFactorRepo {
	return &SecurityAuthFactorRepo{db: db}
}

// Write 供跨表原子事务复用（如恢复：消费码+撤销因子+提升代次+签发新挑战）。
func (r *SecurityAuthFactorRepo) Write(ctx context.Context, apply func(*sqlx.Tx) error) error {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Read 只读事务（入口裁决等需要一致性读的路径）。
func (r *SecurityAuthFactorRepo) Read(ctx context.Context, apply func(*sqlx.Tx) error) error {
	conn, err := r.db.Connx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Rollback()
}

func InsertSecurityChallengeTx(ctx context.Context, tx *sqlx.Tx, c SecurityAuthChallenge) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO security_auth_challenge
		 (challenge_id,user_id,account_epoch,purpose,token_hash,attempts_remaining,expires_at,state,created_at,auth_source,session_binding)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.ChallengeID, c.UserID, c.AccountEpoch, c.Purpose, c.TokenHash, c.AttemptsRemaining,
		c.ExpiresAt, ChallengeStateIssued, c.CreatedAt, c.AuthSource, c.SessionBinding)
	return err
}

func (r *SecurityAuthFactorRepo) InsertChallenge(ctx context.Context, c SecurityAuthChallenge) error {
	return r.Write(ctx, func(tx *sqlx.Tx) error { return InsertSecurityChallengeTx(ctx, tx, c) })
}

// CancelIssuedChallengesTx 取消主体同用途的全部在途挑战（签发新挑战前的替换语义）。
func CancelIssuedChallengesTx(ctx context.Context, tx *sqlx.Tx, userID int64, purpose string, except string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE security_auth_challenge SET state=? WHERE user_id=? AND purpose=? AND state=? AND challenge_id<>?`,
		ChallengeStateCancelled, userID, purpose, ChallengeStateIssued, except)
	return err
}

// ChallengeByTokenHash 按校验值定位挑战；不存在的 token 与失效挑战对外同形。
func ChallengeByTokenHashTx(ctx context.Context, tx *sqlx.Tx, tokenHash string) (SecurityAuthChallenge, error) {
	var c SecurityAuthChallenge
	err := tx.GetContext(ctx, &c,
		`SELECT challenge_id,user_id,account_epoch,purpose,token_hash,attempts_remaining,expires_at,state,created_at,consumed_at,auth_source,session_binding
		 FROM security_auth_challenge WHERE token_hash=?`, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrSecurityChallengeRejected
	}
	return c, err
}

func (r *SecurityAuthFactorRepo) ChallengeByTokenHash(ctx context.Context, tokenHash string) (SecurityAuthChallenge, error) {
	var c SecurityAuthChallenge
	err := r.Write(ctx, func(tx *sqlx.Tx) error {
		var e error
		c, e = ChallengeByTokenHashTx(ctx, tx, tokenHash)
		return e
	})
	return c, err
}

// TouchChallengeTx 把在途且未过期的挑战取回供验证；过期者先落终态再拒绝。
// 返回挑战行与是否仍可用。
func TouchChallengeTx(ctx context.Context, tx *sqlx.Tx, tokenHash, purpose string, now int64) (SecurityAuthChallenge, error) {
	return TouchChallengePurposesTx(ctx, tx, tokenHash, now, purpose)
}

// TouchChallengePurposesTx 与 TouchChallengeTx 同语义，但接受一组合法用途
// （登记确认同时受理 factor_enroll 与 factor_recover 绑定的挑战）。
func TouchChallengePurposesTx(ctx context.Context, tx *sqlx.Tx, tokenHash string, now int64, purposes ...string) (SecurityAuthChallenge, error) {
	c, err := ChallengeByTokenHashTx(ctx, tx, tokenHash)
	if err != nil {
		return SecurityAuthChallenge{}, err
	}
	ok := false
	for _, p := range purposes {
		if c.Purpose == p {
			ok = true
			break
		}
	}
	if c.State != ChallengeStateIssued || !ok {
		return SecurityAuthChallenge{}, ErrSecurityChallengeRejected
	}
	if c.ExpiresAt <= now {
		if _, err := tx.ExecContext(ctx,
			`UPDATE security_auth_challenge SET state=? WHERE challenge_id=? AND state=?`,
			ChallengeStateExpired, c.ChallengeID, ChallengeStateIssued); err != nil {
			return SecurityAuthChallenge{}, err
		}
		return SecurityAuthChallenge{}, ErrSecurityChallengeRejected
	}
	return c, nil
}

// ConsumeChallengeTx 一次性把在途挑战置为 consumed；多请求至多一个成功。
func ConsumeChallengeTx(ctx context.Context, tx *sqlx.Tx, challengeID string, now int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE security_auth_challenge SET state=?,consumed_at=? WHERE challenge_id=? AND state=? AND expires_at>?`,
		ChallengeStateConsumed, now, challengeID, ChallengeStateIssued, now)
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

// FailChallengeAttemptTx 原子扣减一次尝试额度；归零时同语置 exhausted。
// 返回 false 表示挑战已不在可尝试状态（含被并发耗尽）。
func FailChallengeAttemptTx(ctx context.Context, tx *sqlx.Tx, challengeID string, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE security_auth_challenge SET
			attempts_remaining=attempts_remaining-1,
			state=CASE WHEN attempts_remaining<=1 THEN ? ELSE state END
		 WHERE challenge_id=? AND state=? AND expires_at>? AND attempts_remaining>0`,
		ChallengeStateExhausted, challengeID, ChallengeStateIssued, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CountActiveChallengesTx 统计主体某用途的在途挑战数（签发上限裁决）。
func CountActiveChallengesTx(ctx context.Context, tx *sqlx.Tx, userID int64, purpose string, now int64) (int, error) {
	var n int
	err := tx.GetContext(ctx, &n,
		`SELECT count(*) FROM security_auth_challenge WHERE user_id=? AND purpose=? AND state=? AND expires_at>?`,
		userID, purpose, ChallengeStateIssued, now)
	return n, err
}

// RecentChallengeAttemptsTx 统计主体近期挑战签发次数（每账号限流：10 次/5 分钟）。
func RecentChallengeAttemptsTx(ctx context.Context, tx *sqlx.Tx, userID int64, since int64) (int, error) {
	var n int
	err := tx.GetContext(ctx, &n,
		`SELECT count(*) FROM security_auth_challenge WHERE user_id=? AND created_at>=?`, userID, since)
	return n, err
}

// InsertTotpFactorTx 写入因子（enrollment_pending 或 active）；唯一索引裁决每账号各态至多一行。
func InsertTotpFactorTx(ctx context.Context, tx *sqlx.Tx, f SecurityTotpFactor) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO security_totp_factor
		 (factor_id,user_id,account_epoch,state,secret_ciphertext,key_version,last_accepted_step,created_at,expires_at,activated_at,challenge_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		f.FactorID, f.UserID, f.AccountEpoch, f.State, f.SecretCiphertext, f.KeyVersion,
		f.LastAcceptedStep, f.CreatedAt, f.ExpiresAt, f.ActivatedAt, f.ChallengeID)
	return err
}

// TotpFactorByUserTx 取主体当前因子；state 为空时取最新一行（任何态），否则取该态行。
func TotpFactorByUserTx(ctx context.Context, tx *sqlx.Tx, userID int64, state string) (SecurityTotpFactor, error) {
	var f SecurityTotpFactor
	// challenge_id 激活后清空：统一归一成空串避免 NULL 扫描。
	query := `SELECT factor_id,user_id,account_epoch,state,secret_ciphertext,key_version,last_accepted_step,created_at,expires_at,activated_at,retired_at,
		 COALESCE(challenge_id,'') AS challenge_id
		 FROM security_totp_factor WHERE user_id=?`
	args := []any{userID}
	if state != "" {
		query += ` AND state=?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at DESC LIMIT 1`
	err := tx.GetContext(ctx, &f, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrSecurityChallengeRejected
	}
	return f, err
}

func (r *SecurityAuthFactorRepo) ActiveTotpFactor(ctx context.Context, userID int64) (SecurityTotpFactor, error) {
	var f SecurityTotpFactor
	err := r.Write(ctx, func(tx *sqlx.Tx) error {
		var e error
		f, e = TotpFactorByUserTx(ctx, tx, userID, TotpFactorStateActive)
		return e
	})
	return f, err
}

// ActivateTotpFactorTx 把指定 pending 因子原子激活（唯一索引约束撤销并发第二行）。
// 挑战须同事务消费，过期 pending 一并失效。
func ActivateTotpFactorTx(ctx context.Context, tx *sqlx.Tx, factorID string, userID int64, now int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE security_totp_factor SET state=?,activated_at=?,expires_at=NULL,challenge_id=NULL
		 WHERE factor_id=? AND user_id=? AND state=? AND (expires_at IS NULL OR expires_at>?)`,
		TotpFactorStateActive, now, factorID, userID, TotpFactorStatePending, now)
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

// AcceptTotpStepTx 条件推进防重放水位：仅严格更大的步可被接受，并发至多一个成功。
// 登记验证同样消耗该因子步，故 pending/active 两态都接受推进。
func AcceptTotpStepTx(ctx context.Context, tx *sqlx.Tx, factorID string, step int64) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE security_totp_factor SET last_accepted_step=? WHERE factor_id=? AND state IN (?,?) AND last_accepted_step<?`,
		step, factorID, TotpFactorStatePending, TotpFactorStateActive, step)
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

// RetireTotpFactorsTx 撤销主体指定态的全部因子并打上退役时间。
func RetireTotpFactorsTx(ctx context.Context, tx *sqlx.Tx, userID int64, now int64, states ...string) error {
	for _, st := range states {
		if _, err := tx.ExecContext(ctx,
			`UPDATE security_totp_factor SET state=?,retired_at=? WHERE user_id=? AND state=?`,
			TotpFactorStateRetired, now, userID, st); err != nil {
			return err
		}
	}
	return nil
}

// ExpirePendingFactorTx 让到期待登记因子失效（过期废弃待登记种子）。
func ExpirePendingFactorTx(ctx context.Context, tx *sqlx.Tx, factorID string, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE security_totp_factor SET state=?,retired_at=? WHERE factor_id=? AND state=? AND expires_at IS NOT NULL AND expires_at<=?`,
		TotpFactorStateRetired, now, factorID, TotpFactorStatePending, now)
	return err
}

// ReplaceRecoveryBatchTx 插入新批次并使主体旧批次全部作废（仍由 hash 防误消费）。
// 返回作废的旧码数，供事件记录。
func ReplaceRecoveryBatchTx(ctx context.Context, tx *sqlx.Tx, codes []SecurityRecoveryCode) (int, error) {
	if len(codes) == 0 {
		return 0, ErrSecurityChallengeRejected
	}
	userID, batch := codes[0].UserID, codes[0].Batch
	var stale int
	if err := tx.GetContext(ctx, &stale,
		`SELECT count(*) FROM security_recovery_code WHERE user_id=? AND batch<>? AND consumed_at IS NULL`, userID, batch); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM security_recovery_code WHERE user_id=? AND batch<>?`, userID, batch); err != nil {
		return 0, err
	}
	for _, c := range codes {
		if c.UserID != userID || c.Batch != batch {
			return 0, ErrSecurityChallengeRejected
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO security_recovery_code (code_id,user_id,factor_id,batch,code_hash,created_at) VALUES (?,?,?,?,?,?)`,
			c.CodeID, c.UserID, c.FactorID, c.Batch, c.CodeHash, c.CreatedAt); err != nil {
			return 0, err
		}
	}
	return stale, nil
}

// NextRecoveryBatchTx 取主体下一批次号（防重放水位单调）。
func NextRecoveryBatchTx(ctx context.Context, tx *sqlx.Tx, userID int64) (int64, error) {
	var b int64
	err := tx.GetContext(ctx, &b,
		`SELECT COALESCE(MAX(batch),0)+1 FROM security_recovery_code WHERE user_id=?`, userID)
	return b, err
}

// ConsumeRecoveryCodeTx 按校验值原子消费一枚未用恢复码；多请求至多一个成功。
func ConsumeRecoveryCodeTx(ctx context.Context, tx *sqlx.Tx, codeHash string, now int64) (SecurityRecoveryCode, error) {
	var c SecurityRecoveryCode
	if err := tx.GetContext(ctx, &c,
		`SELECT code_id,user_id,factor_id,batch,code_hash,created_at,consumed_at
		 FROM security_recovery_code WHERE code_hash=?`, codeHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrSecurityChallengeRejected
		}
		return SecurityRecoveryCode{}, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE security_recovery_code SET consumed_at=? WHERE code_id=? AND consumed_at IS NULL`, now, c.CodeID)
	if err != nil {
		return SecurityRecoveryCode{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SecurityRecoveryCode{}, err
	}
	if n != 1 {
		return SecurityRecoveryCode{}, ErrSecurityChallengeRejected
	}
	return c, nil
}

// ConsumeRemainingRecoveryCodesTx 作废主体其余未消费恢复码（恢复成功后旧码全部失效）。
func ConsumeRemainingRecoveryCodesTx(ctx context.Context, tx *sqlx.Tx, userID int64, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE security_recovery_code SET consumed_at=? WHERE user_id=? AND consumed_at IS NULL`, now, userID)
	return err
}

// CountLiveRecoveryCodesTx 统计主体未消费恢复码（用于自身状态页）。
func CountLiveRecoveryCodesTx(ctx context.Context, tx *sqlx.Tx, userID int64) (int, error) {
	var n int
	err := tx.GetContext(ctx, &n,
		`SELECT count(*) FROM security_recovery_code WHERE user_id=? AND consumed_at IS NULL`, userID)
	return n, err
}

// BumpAccountEpochTx 在同一事务内提升账号代次（旧会话/Token/因子随之失效）；
// 条件更新要求代次仍一致，触发器同序推进签发水位。
func BumpAccountEpochTx(ctx context.Context, tx *sqlx.Tx, userID, expectEpoch int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE user SET account_epoch=account_epoch+1, updated_at=datetime('now') WHERE id=? AND account_epoch=?`,
		userID, expectEpoch)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrCredentialRejected
	}
	var epoch int64
	if err := tx.GetContext(ctx, &epoch, `SELECT account_epoch FROM user WHERE id=?`, userID); err != nil {
		return 0, err
	}
	return epoch, nil
}

// SweepExpiredChallenges 小批清理到期在途挑战（可观测的惰性收口，不在请求路径计数）。
func (r *SecurityAuthFactorRepo) SweepExpiredChallenges(ctx context.Context, now int64, limit int) (int, error) {
	var n int64
	err := r.Write(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE security_auth_challenge SET state=? WHERE state=? AND expires_at<=? AND challenge_id IN
			 (SELECT challenge_id FROM security_auth_challenge WHERE state=? AND expires_at<=? LIMIT ?)`,
			ChallengeStateExpired, ChallengeStateIssued, now, ChallengeStateIssued, now, limit)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// HasActiveTotpFactorTx 报告账号是否已有激活的强因子（签发门裁决用，锁内判断）。
func HasActiveTotpFactorTx(ctx context.Context, tx *sqlx.Tx, userID int64) (bool, error) {
	var fid string
	err := tx.GetContext(ctx, &fid,
		`SELECT factor_id FROM security_totp_factor WHERE user_id=? AND state='active' LIMIT 1`, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return fid != "", err
}
