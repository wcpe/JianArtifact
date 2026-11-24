package domain

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// recentMfaWindow 是敏感操作要求的近期强认证窗口（规格默认 5 分钟）。
const recentMfaWindow = 5 * time.Minute

// BeginStepUp 为当前会话签发 step_up 受限挑战（绑定会话，验证当前账号的活跃因子）。
// 账号无活跃因子时无强认证可做——按统一挑战错误拒绝。
func (s *SecurityMfaService) BeginStepUp(ctx context.Context, account repository.CredentialAccount, sessionID string) (*MfaEnrollment, error) {
	var out *MfaEnrollment
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		has, err := repository.HasActiveTotpFactorTx(ctx, tx, account.ID)
		if err != nil {
			return err
		}
		if !has {
			return repository.ErrSecurityChallengeRejected
		}
		challenge, token, err := s.issueChallenge(ctx, tx, account.ID, account.Epoch,
			repository.ChallengePurposeStepUp, "step_up", sessionID)
		if err != nil {
			return err
		}
		out = &MfaEnrollment{
			ChallengeID:    challenge.ChallengeID,
			ChallengeToken: token,
			ExpiresAt:      time.Unix(challenge.ExpiresAt, 0).UTC(),
		}
		return nil
	})
	if err != nil {
		return nil, mapChallengeErr(err)
	}
	return out, nil
}

// CompleteStepUp 兑现 step_up 挑战：复用 login_mfa 的触碰/代次/绑定复核，
// OTP 命中消耗步水位并消费挑战，随后把会话 mfa_verified_at 抬到 now（同一账号代次复核后）。
func (s *SecurityMfaService) CompleteStepUp(ctx context.Context, challengeToken, otp, sessionID string) (int64, error) {
	now := s.now().Unix()
	tokenHash := hashChallengeToken(challengeToken)
	var challenge repository.SecurityAuthChallenge
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		c, err := repository.TouchChallengeTx(ctx, tx, tokenHash, repository.ChallengePurposeStepUp, now)
		if err != nil {
			return err
		}
		if c.SessionBinding != sessionID {
			return repository.ErrSecurityChallengeRejected
		}
		var epoch int64
		if err := tx.GetContext(ctx, &epoch, `SELECT account_epoch FROM user WHERE id=?`, c.UserID); err != nil {
			return err
		}
		if epoch != c.AccountEpoch {
			return repository.ErrSecurityChallengeRejected
		}
		challenge = c
		return nil
	})
	if err != nil {
		return 0, mapChallengeErr(err)
	}
	if err := s.VerifyTotp(ctx, challenge.UserID, otp); err != nil {
		if err == ErrMfaFactorUnavailable {
			return 0, err
		}
		_ = s.factors.Write(ctx, func(tx *sqlx.Tx) error {
			_, werr := repository.FailChallengeAttemptTx(ctx, tx, challenge.ChallengeID, now)
			return werr
		})
		return 0, ErrInvalidAuthChallenge
	}
	err = s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		if err := repository.ConsumeChallengeTx(ctx, tx, challenge.ChallengeID, now); err != nil {
			return err
		}
		return repository.MarkSessionMfaVerifiedTx(ctx, tx, sessionID, now)
	})
	if err != nil {
		return 0, mapChallengeErr(err)
	}
	return now, nil
}

// RecentMfaVerifiedAt 读会话的近期强认证水位（会话须属于该账号且未撤销）。
// 返回 nil 表示无水位（敏感操作应拒绝并转 recent_mfa_required）。
func (s *SecurityMfaService) RecentMfaVerifiedAt(ctx context.Context, userID int64, sessionID string) (*int64, error) {
	var at *int64
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		var owner int64
		if err := tx.GetContext(ctx, &owner,
			`SELECT user_id FROM credential_session WHERE session_id=? AND revoked_at IS NULL`, sessionID); err != nil {
			return err
		}
		if owner != userID {
			return repository.ErrSecurityChallengeRejected
		}
		v, err := repository.SessionMfaVerifiedAtTx(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		at = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return at, nil
}

// RecentMfaFresh 判定水位是否在窗口内。
func RecentMfaFresh(at *int64, now time.Time) bool {
	return at != nil && now.Unix()-*at <= int64(recentMfaWindow.Seconds())
}
