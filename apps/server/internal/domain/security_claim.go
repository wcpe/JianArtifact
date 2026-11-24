package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 认领票口径：15 分钟、5 次尝试；重新出具即作废同主体同用途旧票。
const (
	securityClaimTicketTTL      = 15 * time.Minute
	securityClaimTicketAttempts = 5
)

// ClaimTicket 是出具给受控操作者的一次性认领凭据；明文仅返回一次。
type ClaimTicket struct {
	TicketID  string    `json:"ticket_id"`
	Ticket    string    `json:"ticket"`
	Purpose   string    `json:"purpose"`
	Username  string    `json:"username,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// IssueClaimTicket 为目标账号出具 claim_admin 票（本机受控 CLI 或后续近期强认证管理员）。
// 票绑定账号当前代次：期间任何账号级安全变更都使票作废。失败不遗留半成品。
func (s *SecurityMfaService) IssueClaimTicket(ctx context.Context, username, purpose, issuedBy, reason string) (*ClaimTicket, error) {
	if s.cipher == nil {
		return nil, ErrMfaFactorUnavailable
	}
	account, err := s.users.CredentialAccountByUsername(ctx, username)
	if err != nil {
		return nil, ErrInvalidAuthChallenge
	}
	if account.Status != "active" {
		return nil, ErrInvalidAuthChallenge
	}
	return s.issueClaimTicketFor(ctx, account, purpose, issuedBy, reason)
}

func (s *SecurityMfaService) issueClaimTicketFor(ctx context.Context, account repository.CredentialAccount, purpose, issuedBy, reason string) (*ClaimTicket, error) {
	token, err := randB32(32)
	if err != nil {
		return nil, err
	}
	token = "tk-" + token
	now := s.now()
	t := repository.SecurityClaimTicket{
		TicketID:          "tki-" + uuid.NewString(),
		UserID:            &account.ID,
		Purpose:           purpose,
		TokenHash:         hashChallengeToken(token),
		AccountEpoch:      account.Epoch,
		IssuedBy:          issuedBy,
		Reason:            reason,
		AttemptsRemaining: securityClaimTicketAttempts,
		ExpiresAt:         now.Add(securityClaimTicketTTL).Unix(),
		CreatedAt:         now.Unix(),
	}
	if err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		return repository.InsertClaimTicketTx(ctx, tx, t)
	}); err != nil {
		return nil, err
	}
	return &ClaimTicket{
		TicketID: t.TicketID, Ticket: token, Purpose: purpose,
		Username: account.Username, ExpiresAt: time.Unix(t.ExpiresAt, 0).UTC(),
	}, nil
}

// RedeemClaimTicket 以「第一因素 + 认领票」进入受控认领：
// 口令核验在锁外；事务内复核票的主体/代次绑定（不符记一次票尝试并拒绝），
// 然后退役旧待登记因子、种新待登记因子并签发 claim 来源的 factor_enroll 挑战。
// 票保持 issued 直至确认——一次认领＝票验证、因子激活、账户事件同事务。
func (s *SecurityMfaService) RedeemClaimTicket(ctx context.Context, username, password, ticket, sessionBinding string) (*MfaEnrollment, error) {
	if s.cipher == nil {
		return nil, ErrMfaFactorUnavailable
	}
	account, err := s.users.CredentialAccountByUsername(ctx, username)
	if err != nil || account.Status != "active" || account.WebLoginDisabled {
		return nil, ErrInvalidAuthChallenge
	}
	if err := auth.VerifyPassword(password, account.PasswordHash); err != nil {
		return nil, ErrInvalidAuthChallenge
	}
	tokenHash := hashChallengeToken(ticket)
	secret, secretB32, err := newTotpSecret()
	if err != nil {
		return nil, err
	}
	factorID := "tf-" + uuid.NewString()
	ciphertext, err := s.cipher.seal(factorID, secret)
	if err != nil {
		return nil, ErrMfaFactorUnavailable
	}
	var out *MfaEnrollment
	err = s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		now := s.now().Unix()
		t, err := repository.TouchClaimTicketTx(ctx, tx, tokenHash, repository.ClaimPurposeAdmin, now)
		if err != nil {
			return err
		}
		if t.UserID == nil || *t.UserID != account.ID || t.AccountEpoch != account.Epoch {
			// 绑定不符记一次票尝试；扣减独立提交不因拒绝丢账。
			_, _ = repository.FailClaimTicketAttemptTx(ctx, tx, t.TicketID, now)
			return repository.ErrSecurityChallengeRejected
		}
		if err := repository.RetireTotpFactorsTx(ctx, tx, account.ID, now, repository.TotpFactorStatePending); err != nil {
			return err
		}
		challenge, ctoken, err := s.issueChallenge(ctx, tx, account.ID, account.Epoch,
			repository.ChallengePurposeFactorEnroll, "claim", sessionBinding)
		if err != nil {
			return err
		}
		factor := repository.SecurityTotpFactor{
			FactorID:         factorID,
			UserID:           account.ID,
			AccountEpoch:     account.Epoch,
			State:            repository.TotpFactorStatePending,
			SecretCiphertext: ciphertext,
			KeyVersion:       s.cipher.keyVer,
			LastAcceptedStep: -1,
			CreatedAt:        now,
			ChallengeID:      challenge.ChallengeID,
		}
		exp := challenge.ExpiresAt
		factor.ExpiresAt = &exp
		if err := repository.InsertTotpFactorTx(ctx, tx, factor); err != nil {
			return err
		}
		out = &MfaEnrollment{
			ChallengeID:    challenge.ChallengeID,
			ChallengeToken: ctoken,
			ExpiresAt:      time.Unix(challenge.ExpiresAt, 0).UTC(),
			EnrollmentURI:  totpEnrollmentURI(s.issuer, account.Username, secretB32),
		}
		return nil
	})
	if err != nil {
		return nil, mapChallengeErr(err)
	}
	return out, nil
}
