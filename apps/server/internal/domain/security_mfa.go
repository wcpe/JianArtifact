package domain

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 强认证（FR-167）首增量：挑战状态机 + TOTP 因子 + 恢复码。
// 本服务只裁决因子与挑战生命周期，不签发会话；登录门与敏感操作确认在后续增量接入。
const (
	securityChallengeTTL      = 5 * time.Minute
	securityChallengeAttempts = 5
	securityRecoveryCodeCount = 10
	securityFactorKeyVersion  = 1
	defaultTotpIssuer         = "JianArtifact"
	securityChallengeRate     = 10 // 每账号每 5 分钟挑战签发上限（规格限流首版）
)

var (
	// ErrInvalidAuthChallenge 对外统一错误：身份/代次/用途不符、过期、已消费、超额同形。
	ErrInvalidAuthChallenge = errors.New("invalid_auth_challenge")
	// ErrMfaFactorUnavailable 表示实例缺少因子加密密钥等运行前提（503 语义由上层映射）。
	ErrMfaFactorUnavailable = errors.New("security_state_unavailable")
	// ErrMfaRequired 表示账号必须启用强因子才可完成当前动作。
	ErrMfaRequired = errors.New("mfa_required")
	// ErrAuthRateLimited 表示账号或来源的挑战签发超过有界限流。
	ErrAuthRateLimited = errors.New("auth_rate_limited")
)

// SecurityMfaService 管理挑战、TOTP 因子与恢复码的原子生命周期。
type SecurityMfaService struct {
	factors *repository.SecurityAuthFactorRepo
	users   *repository.UserRepo
	cipher  *securityFactorCipher
	issuer  string
	now     func() time.Time
	audit   *SecurityAuditService
}

// NewSecurityMfaService 构造服务；factorKey 为实例专用 32 字节密钥（不入库、不复用 JWT 密钥）。
func NewSecurityMfaService(factors *repository.SecurityAuthFactorRepo, users *repository.UserRepo, factorKey []byte) (*SecurityMfaService, error) {
	cipher, err := newSecurityFactorCipher(factorKey, securityFactorKeyVersion)
	if err != nil {
		return nil, err
	}
	return &SecurityMfaService{factors: factors, users: users, cipher: cipher, issuer: defaultTotpIssuer, now: time.Now}, nil
}

func (s *SecurityMfaService) SetSecurityAudit(a *SecurityAuditService) { s.audit = a }

// SetTotpIssuer 覆盖登记 URI 的签发者名（装配期注入实例名）。
func (s *SecurityMfaService) SetTotpIssuer(issuer string) {
	if issuer != "" {
		s.issuer = issuer
	}
}

// setNow 仅供测试注入确定性时钟。
func (s *SecurityMfaService) setNow(f func() time.Time) { s.now = f }

// MfaEnrollment 是登记启动的一次性返回：种子 URI 只展示一次，不持久明文。
type MfaEnrollment struct {
	ChallengeID    string
	ChallengeToken string
	ExpiresAt      time.Time
	EnrollmentURI  string
}

// MfaStatus 是账号强认证状态的最小投影。
type MfaStatus struct {
	Enrolled               bool
	EnrollmentPending      bool
	RecoveryCodesRemaining int
}

// Status 返回账号当前强认证状态（自身状态页的最小投影，不泄露秘密字段）。
func (s *SecurityMfaService) Status(ctx context.Context, userID int64) (MfaStatus, error) {
	var st MfaStatus
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		if _, err := repository.TotpFactorByUserTx(ctx, tx, userID, repository.TotpFactorStateActive); err == nil {
			st.Enrolled = true
		} else if !errors.Is(err, repository.ErrSecurityChallengeRejected) {
			return err
		}
		if _, err := repository.TotpFactorByUserTx(ctx, tx, userID, repository.TotpFactorStatePending); err == nil {
			st.EnrollmentPending = true
		} else if !errors.Is(err, repository.ErrSecurityChallengeRejected) {
			return err
		}
		n, err := repository.CountLiveRecoveryCodesTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		st.RecoveryCodesRemaining = n
		return nil
	})
	return st, err
}

// randB32 生成高熵不透明串（Base32 无填充小写）。
func randB32(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// issueChallenge 在事务内签发新挑战并替换同用途旧挑战（同账户/用途至多一个生效）。
// 签发前按「每账号 10 次/5 分钟」有界限流；来源与实例级限流在 HTTP 边界另施。
func (s *SecurityMfaService) issueChallenge(ctx context.Context, tx *sqlx.Tx, userID, epoch int64, purpose, authSource, sessionBinding string) (repository.SecurityAuthChallenge, string, error) {
	recent, err := repository.RecentChallengeAttemptsTx(ctx, tx, userID, s.now().Add(-5*time.Minute).Unix())
	if err != nil {
		return repository.SecurityAuthChallenge{}, "", err
	}
	if recent >= securityChallengeRate {
		return repository.SecurityAuthChallenge{}, "", ErrAuthRateLimited
	}
	c := repository.SecurityAuthChallenge{
		ChallengeID:       "ch-" + uuid.NewString(),
		UserID:            userID,
		AccountEpoch:      epoch,
		Purpose:           purpose,
		AttemptsRemaining: securityChallengeAttempts,
		ExpiresAt:         s.now().Add(securityChallengeTTL).Unix(),
		State:             repository.ChallengeStateIssued,
		CreatedAt:         s.now().Unix(),
		AuthSource:        authSource,
		SessionBinding:    sessionBinding,
	}
	token, err := randB32(32)
	if err != nil {
		return repository.SecurityAuthChallenge{}, "", err
	}
	token = "ct-" + token
	c.TokenHash = hashChallengeToken(token)
	if err := repository.InsertSecurityChallengeTx(ctx, tx, c); err != nil {
		return repository.SecurityAuthChallenge{}, "", err
	}
	if err := repository.CancelIssuedChallengesTx(ctx, tx, userID, purpose, c.ChallengeID); err != nil {
		return repository.SecurityAuthChallenge{}, "", err
	}
	return c, token, nil
}

// BeginFactorEnrollment 为当前会话用户开启因子登记：
// 写入待登记因子（绑定挑战、到期废弃）并签发 factor_enroll 挑战。
// 已有 pending 因子时先退役（重新登记），不影响已激活因子。
func (s *SecurityMfaService) BeginFactorEnrollment(ctx context.Context, account repository.CredentialAccount, authSource, sessionBinding string) (*MfaEnrollment, error) {
	if s.cipher == nil {
		return nil, ErrMfaFactorUnavailable
	}
	secret, secretB32, err := newTotpSecret()
	if err != nil {
		return nil, err
	}
	factorID := "tf-" + uuid.NewString()
	ciphertext, err := s.cipher.seal(factorID, secret)
	if err != nil {
		return nil, err
	}
	var out *MfaEnrollment
	err = s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		if err := repository.RetireTotpFactorsTx(ctx, tx, account.ID, s.now().Unix(), repository.TotpFactorStatePending); err != nil {
			return err
		}
		challenge, token, err := s.issueChallenge(ctx, tx, account.ID, account.Epoch, repository.ChallengePurposeFactorEnroll, authSource, sessionBinding)
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
			CreatedAt:        s.now().Unix(),
			ChallengeID:      challenge.ChallengeID,
		}
		exp := challenge.ExpiresAt
		factor.ExpiresAt = &exp
		if err := repository.InsertTotpFactorTx(ctx, tx, factor); err != nil {
			return err
		}
		out = &MfaEnrollment{
			ChallengeID:    challenge.ChallengeID,
			ChallengeToken: token,
			ExpiresAt:      time.Unix(challenge.ExpiresAt, 0).UTC(),
			EnrollmentURI:  totpEnrollmentURI(s.issuer, account.Username, secretB32),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ConfirmFactorEnrollment 校验登记挑战与首枚 TOTP。事务拆为读→算→写：
// 读事务只取挑战与因子状态，口令比对在锁外；命中走条件写（消耗步、消费挑战、
// 退役旧因子、激活新因子、签发恢复码），未命中走独立的尝试扣减事务——
// 失败也必须落额度扣减，不能随验证错误回滚。
func (s *SecurityMfaService) ConfirmFactorEnrollment(ctx context.Context, challengeToken, otp, claimTicket, sessionBinding string) ([]string, error) {
	now := s.now().Unix()
	var challenge repository.SecurityAuthChallenge
	var factor repository.SecurityTotpFactor
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		c, err := repository.TouchChallengePurposesTx(ctx, tx, hashChallengeToken(challengeToken), now,
			repository.ChallengePurposeFactorEnroll, repository.ChallengePurposeFactorRecover)
		if err != nil {
			return err
		}
		// 账号身份从服务端挑战行解析，不从请求输入重建。
		userID := c.UserID
		// 会话绑定只存不验即为空检查：签发时绑定的会话与确认方必须一致。
		if c.SessionBinding != "" && c.SessionBinding != sessionBinding {
			return repository.ErrSecurityChallengeRejected
		}
		f, err := repository.TotpFactorByUserTx(ctx, tx, userID, repository.TotpFactorStatePending)
		if err != nil {
			return err
		}
		if f.ChallengeID != c.ChallengeID {
			return repository.ErrSecurityChallengeRejected
		}
		// 认领来源挑战要求一次认领成立：提交方必须同时出示绑定同账号同代次的
		// 在途 claim_admin 票，票在激活事务内消费；缺票/错票统一拒绝。
		if c.AuthSource == "claim" {
			if claimTicket == "" {
				return repository.ErrSecurityChallengeRejected
			}
			t, terr := repository.TouchClaimTicketTx(ctx, tx, hashChallengeToken(claimTicket), repository.ClaimPurposeAdmin, now)
			if terr != nil {
				return terr
			}
			if t.UserID == nil || *t.UserID != c.UserID || t.AccountEpoch != c.AccountEpoch {
				return repository.ErrSecurityChallengeRejected
			}
		}
		challenge, factor = c, f
		return nil
	})
	if err != nil {
		return nil, mapChallengeErr(err)
	}
	secret, err := s.cipher.open(factor.FactorID, factor.SecretCiphertext)
	if err != nil {
		return nil, ErrMfaFactorUnavailable
	}
	step := totpMatchStep(secret, otp, now)
	if step < 0 {
		// 失败尝试独立事务落账：多请求至多一次扣减，归零同语耗尽。
		err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
			ok, err := repository.FailChallengeAttemptTx(ctx, tx, challenge.ChallengeID, now)
			if err != nil {
				return err
			}
			if !ok {
				return repository.ErrSecurityChallengeRejected
			}
			return nil
		})
		if err != nil {
			return nil, mapChallengeErr(err)
		}
		return nil, ErrInvalidAuthChallenge
	}
	var codes []string
	err = s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		// 挑战绑定签发时的账号代次：其间任何账号级安全变更（改密/恢复/处置）
		// 都使在途挑战作废，不能把旧代次的授权延续到新一代。
		var epoch int64
		if err := tx.GetContext(ctx, &epoch, `SELECT account_epoch FROM user WHERE id=?`, challenge.UserID); err != nil {
			return err
		}
		if epoch != challenge.AccountEpoch {
			return repository.ErrSecurityChallengeRejected
		}
		// 登记验证消耗该因子步；先退役旧激活因子再激活，保证「唯一 active」不被并发冲破。
		if err := repository.AcceptTotpStepTx(ctx, tx, factor.FactorID, step); err != nil {
			return err
		}
		if err := repository.ConsumeChallengeTx(ctx, tx, challenge.ChallengeID, now); err != nil {
			return err
		}
		if err := repository.RetireTotpFactorsTx(ctx, tx, challenge.UserID, now, repository.TotpFactorStateActive); err != nil {
			return err
		}
		if err := repository.ActivateTotpFactorTx(ctx, tx, factor.FactorID, challenge.UserID, now); err != nil {
			return err
		}
		// 认领票与因子激活同事务消费：票只用一次，失败重试须操作者重新出具。
		if challenge.AuthSource == "claim" {
			t, terr := repository.ClaimTicketByTokenHashTx(ctx, tx, hashChallengeToken(claimTicket))
			if terr != nil {
				return terr
			}
			if err := repository.ConsumeClaimTicketTx(ctx, tx, t.TicketID, now); err != nil {
				return err
			}
		}
		issued, err := s.issueRecoveryBatch(ctx, tx, challenge.UserID, factor.FactorID, now)
		if err != nil {
			return err
		}
		codes = issued
		return nil
	})
	if err != nil {
		return nil, mapChallengeErr(err)
	}
	return codes, nil
}

// mapChallengeErr 把持久层的挑战/凭据拒绝归一为对外的统一挑战错误。
func mapChallengeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, repository.ErrSecurityChallengeRejected) || errors.Is(err, repository.ErrCredentialRejected) {
		return ErrInvalidAuthChallenge
	}
	return err
}

// issueRecoveryBatch 签发一批恢复码并作废旧批；明文只经返回一次展示。
func (s *SecurityMfaService) issueRecoveryBatch(ctx context.Context, tx *sqlx.Tx, userID int64, factorID string, now int64) ([]string, error) {
	plain, err := newRecoveryCodes(securityRecoveryCodeCount)
	if err != nil {
		return nil, err
	}
	batch, err := repository.NextRecoveryBatchTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	rows := make([]repository.SecurityRecoveryCode, 0, len(plain))
	for _, c := range plain {
		rows = append(rows, repository.SecurityRecoveryCode{
			CodeID:    "rc-" + uuid.NewString(),
			UserID:    userID,
			FactorID:  factorID,
			Batch:     batch,
			CodeHash:  hashRecoveryCode(c),
			CreatedAt: now,
		})
	}
	if _, err := repository.ReplaceRecoveryBatchTx(ctx, tx, rows); err != nil {
		return nil, err
	}
	return plain, nil
}

// VerifyTotp 校验已激活因子的动态口令并推进防重放水位。
// 同一时间步全系统至多一次成功（含登记验证），并发重放被拒。
func (s *SecurityMfaService) VerifyTotp(ctx context.Context, userID int64, otp string) error {
	var factor repository.SecurityTotpFactor
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		f, err := repository.TotpFactorByUserTx(ctx, tx, userID, repository.TotpFactorStateActive)
		if err != nil {
			return err
		}
		factor = f
		return nil
	})
	if err != nil {
		return mapChallengeErr(err)
	}
	secret, err := s.cipher.open(factor.FactorID, factor.SecretCiphertext)
	if err != nil {
		return ErrMfaFactorUnavailable
	}
	step := totpMatchStep(secret, otp, s.now().Unix())
	if step < 0 {
		return ErrInvalidAuthChallenge
	}
	if err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		return repository.AcceptTotpStepTx(ctx, tx, factor.FactorID, step)
	}); err != nil {
		return mapChallengeErr(err)
	}
	return nil
}

// RecoverFactor 用一枚有效恢复码执行受控恢复：同一事务内消费该码、作废其余旧码、
// 撤销旧因子、提升账号代次，并签发 factor_recover 受限挑战（只可绑定新因子）——
// 同事务直接种下新的待登记因子并返回登记 URI，确认走同一 ConfirmFactorEnrollment。
// 不沿用旧挑战/会话，旧因子不可中途恢复。
func (s *SecurityMfaService) RecoverFactor(ctx context.Context, account repository.CredentialAccount, recoveryCode, authSource, sessionBinding string) (*MfaEnrollment, error) {
	if s.cipher == nil {
		return nil, ErrMfaFactorUnavailable
	}
	secret, secretB32, err := newTotpSecret()
	if err != nil {
		return nil, err
	}
	factorID := "tf-" + uuid.NewString()
	ciphertext, err := s.cipher.seal(factorID, secret)
	if err != nil {
		return nil, err
	}
	var out *MfaEnrollment
	err = s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		now := s.now().Unix()
		code, err := repository.ConsumeRecoveryCodeTx(ctx, tx, hashRecoveryCode(recoveryCode), now)
		if err != nil {
			return err
		}
		if code.UserID != account.ID {
			return repository.ErrSecurityChallengeRejected
		}
		if err := repository.ConsumeRemainingRecoveryCodesTx(ctx, tx, account.ID, now); err != nil {
			return err
		}
		if err := repository.RetireTotpFactorsTx(ctx, tx, account.ID, now, repository.TotpFactorStateActive, repository.TotpFactorStatePending); err != nil {
			return err
		}
		epoch, err := repository.BumpAccountEpochTx(ctx, tx, account.ID, account.Epoch)
		if err != nil {
			return err
		}
		challenge, token, err := s.issueChallenge(ctx, tx, account.ID, epoch, repository.ChallengePurposeFactorRecover, authSource, sessionBinding)
		if err != nil {
			return err
		}
		factor := repository.SecurityTotpFactor{
			FactorID:         factorID,
			UserID:           account.ID,
			AccountEpoch:     epoch,
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
			ChallengeToken: token,
			ExpiresAt:      time.Unix(challenge.ExpiresAt, 0).UTC(),
			EnrollmentURI:  totpEnrollmentURI(s.issuer, account.Username, secretB32),
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, repository.ErrSecurityChallengeRejected) || errors.Is(err, repository.ErrCredentialRejected) {
			return nil, ErrInvalidAuthChallenge
		}
		return nil, err
	}
	return out, nil
}

// TouchLoginChallenge 复核 login_mfa 挑战并返回其绑定信息（恢复路径的第一因素凭据）。
func (s *SecurityMfaService) TouchLoginChallenge(ctx context.Context, challengeToken string) (repository.SecurityAuthChallenge, error) {
	now := s.now().Unix()
	var c repository.SecurityAuthChallenge
	err := s.factors.Write(ctx, func(tx *sqlx.Tx) error {
		var err error
		c, err = repository.TouchChallengeTx(ctx, tx, hashChallengeToken(challengeToken), repository.ChallengePurposeLoginMFA, now)
		return err
	})
	if err != nil {
		return repository.SecurityAuthChallenge{}, mapChallengeErr(err)
	}
	return c, nil
}

// CredentialAccountByID 按挑战解析的账号主键取凭据账号快照（恢复路径复核用）。
func (s *SecurityMfaService) CredentialAccountByID(ctx context.Context, id int64) (repository.CredentialAccount, error) {
	return s.users.CredentialAccountByID(ctx, id)
}
