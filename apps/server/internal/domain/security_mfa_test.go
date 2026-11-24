package domain

import (
	"context"
	"encoding/base32"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

var testFactorKey = []byte("0123456789abcdef0123456789abcdef")

func mfaFixture(t *testing.T) (*SecurityMfaService, *repository.SecurityAuthFactorRepo, *repository.UserRepo, int64) {
	t.Helper()
	db := mutationTestDB(t)
	factors := repository.NewSecurityAuthFactorRepo(db)
	users := repository.NewUserRepo(db)
	svc, err := NewSecurityMfaService(factors, users, testFactorKey)
	if err != nil {
		t.Fatalf("构造服务：%v", err)
	}
	id, err := users.Create("mfa-user", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	return svc, factors, users, id
}

func mfaAccount(t *testing.T, users *repository.UserRepo, id int64) repository.CredentialAccount {
	t.Helper()
	u, err := users.GetByID(id)
	if err != nil {
		t.Fatalf("读用户：%v", err)
	}
	account, err := users.CredentialAccountByUsername(context.Background(), u.Username)
	if err != nil {
		t.Fatalf("读账号：%v", err)
	}
	return account
}

func enrollmentSecret(t *testing.T, uri string) []byte {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("解析登记 URI：%v", err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(u.Query().Get("secret"))
	if err != nil || len(secret) == 0 {
		t.Fatalf("登记 URI 不含种子：%v / %d", err, len(secret))
	}
	return secret
}

func totpNow(t *testing.T, secret []byte, when time.Time) string {
	t.Helper()
	code, err := totpCode(secret, when.Unix()/totpStepSeconds)
	if err != nil {
		t.Fatalf("生成 TOTP：%v", err)
	}
	return code
}

func TestTotpRFC6238Vector(t *testing.T) {
	// RFC 6238 附录 B：ASCII 种子 "12345678901234567890"，T=59s → 8 位 94287082，取 6 位 287082。
	secret := []byte("12345678901234567890")
	code, err := totpCode(secret, 59/30)
	if err != nil {
		t.Fatal(err)
	}
	if code != "287082" {
		t.Fatalf("RFC 6238 向量不符：%s", code)
	}
}

func TestMfaEnrollConfirmAndAntiReplay(t *testing.T) {
	svc, _, users, id := mfaFixture(t)
	account := mfaAccount(t, users, id)

	en, err := svc.BeginFactorEnrollment(context.Background(), account, "local", "")
	if err != nil {
		t.Fatalf("开始登记：%v", err)
	}
	if en.ChallengeToken == "" || en.EnrollmentURI == "" {
		t.Fatal("登记返回缺挑战或 URI")
	}
	secret := enrollmentSecret(t, en.EnrollmentURI)
	otp := totpNow(t, secret, svc.now())

	codes, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, otp, "")
	if err != nil {
		t.Fatalf("确认登记：%v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("恢复码数量：%d", len(codes))
	}
	st, err := svc.Status(context.Background(), id)
	if err != nil || !st.Enrolled || st.EnrollmentPending || st.RecoveryCodesRemaining != 10 {
		t.Fatalf("登记后状态：%+v / %v", st, err)
	}

	// 同一挑战 token 不能再次使用。
	if _, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, otp, ""); !errors.Is(err, ErrInvalidAuthChallenge) {
		t.Fatalf("重复消费挑战：%v", err)
	}
	// 登记验证消耗该因子步：同一 OTP 在激活后不能重放。
	if err := svc.VerifyTotp(context.Background(), id, otp); !errors.Is(err, repository.ErrSecurityChallengeRejected) && err != ErrInvalidAuthChallenge {
		t.Fatalf("重放登记 OTP：%v", err)
	}
	// 新的时间步口令可验证，但同一步并发/重放至多一次。
	next := totpNow(t, secret, svc.now().Add(60*time.Second))
	frozen := svc.now().Add(60 * time.Second)
	svc.setNow(func() time.Time { return frozen })
	if err := svc.VerifyTotp(context.Background(), id, next); err != nil {
		t.Fatalf("新步验证：%v", err)
	}
	if err := svc.VerifyTotp(context.Background(), id, next); !errors.Is(err, repository.ErrSecurityChallengeRejected) && err != ErrInvalidAuthChallenge {
		t.Fatalf("同一步重放：%v", err)
	}
}

func TestMfaConcurrentVerifySingleWinner(t *testing.T) {
	svc, _, users, id := mfaFixture(t)
	account := mfaAccount(t, users, id)
	en, err := svc.BeginFactorEnrollment(context.Background(), account, "local", "")
	if err != nil {
		t.Fatal(err)
	}
	secret := enrollmentSecret(t, en.EnrollmentURI)
	if _, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, totpNow(t, secret, svc.now()), ""); err != nil {
		t.Fatal(err)
	}
	frozen := svc.now().Add(90 * time.Second)
	svc.setNow(func() time.Time { return frozen })
	otp := totpNow(t, secret, frozen)

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.VerifyTotp(context.Background(), id, otp); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("并发同一 OTP 成功次数：%d", wins)
	}
}

func TestMfaChallengeAttemptExhaustion(t *testing.T) {
	svc, _, users, id := mfaFixture(t)
	account := mfaAccount(t, users, id)
	en, err := svc.BeginFactorEnrollment(context.Background(), account, "local", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < securityChallengeAttempts; i++ {
		if _, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, "000000", ""); !errors.Is(err, ErrInvalidAuthChallenge) {
			t.Fatalf("第 %d 次错误尝试未拒绝：%v", i+1, err)
		}
	}
	// 额度耗尽后挑战终止，正确口令也不再受理。
	secret := enrollmentSecret(t, en.EnrollmentURI)
	if _, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, totpNow(t, secret, svc.now()), ""); !errors.Is(err, ErrInvalidAuthChallenge) {
		t.Fatalf("耗尽后仍受理：%v", err)
	}
}

func TestMfaRecoveryAtomicity(t *testing.T) {
	svc, factors, users, id := mfaFixture(t)
	account := mfaAccount(t, users, id)
	en, err := svc.BeginFactorEnrollment(context.Background(), account, "local", "")
	if err != nil {
		t.Fatal(err)
	}
	secret := enrollmentSecret(t, en.EnrollmentURI)
	codes, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, totpNow(t, secret, svc.now()), "")
	if err != nil {
		t.Fatal(err)
	}

	recover, err := svc.RecoverFactor(context.Background(), account, codes[0], "local", "")
	if err != nil {
		t.Fatalf("恢复：%v", err)
	}
	if recover.ChallengeToken == "" {
		t.Fatal("恢复未签发受限挑战")
	}
	st, _ := svc.Status(context.Background(), id)
	if st.Enrolled || st.RecoveryCodesRemaining != 0 {
		t.Fatalf("恢复后旧因子/旧码未失效：%+v", st)
	}
	// 旧恢复码全部失效：用另一枚旧码不能再次恢复。
	if _, err := svc.RecoverFactor(context.Background(), account, codes[1], "local", ""); !errors.Is(err, ErrInvalidAuthChallenge) {
		t.Fatalf("旧恢复码重放：%v", err)
	}
	// 账号代次已提升：旧 account_epoch 不能再次执行恢复。
	var epoch int64
	if err := factors.Write(context.Background(), func(tx *sqlx.Tx) error {
		return tx.GetContext(context.Background(), &epoch, `SELECT account_epoch FROM user WHERE id=?`, id)
	}); err != nil {
		t.Fatal(err)
	}
	if epoch <= account.Epoch {
		t.Fatalf("恢复未提升代次：%d <= %d", epoch, account.Epoch)
	}
}

func TestMfaChallengeRateLimit(t *testing.T) {
	svc, _, users, id := mfaFixture(t)
	account := mfaAccount(t, users, id)
	for i := 0; i < securityChallengeRate; i++ {
		if _, err := svc.BeginFactorEnrollment(context.Background(), account, "local", ""); err != nil {
			t.Fatalf("第 %d 次签发被拒：%v", i+1, err)
		}
	}
	if _, err := svc.BeginFactorEnrollment(context.Background(), account, "local", ""); !errors.Is(err, ErrAuthRateLimited) {
		t.Fatalf("超过账号限流仍签发：%v", err)
	}
}

func TestMfaChallengeBindingEpochAndSession(t *testing.T) {
	svc, factors, users, id := mfaFixture(t)
	account := mfaAccount(t, users, id)

	// 会话绑定不符的确认必须拒绝。
	en, err := svc.BeginFactorEnrollment(context.Background(), account, "local", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	secret := enrollmentSecret(t, en.EnrollmentURI)
	if _, err := svc.ConfirmFactorEnrollment(context.Background(), id, en.ChallengeToken, totpNow(t, secret, svc.now()), "sess-2"); !errors.Is(err, ErrInvalidAuthChallenge) {
		t.Fatalf("跨会话确认未拒绝：%v", err)
	}

	// 代次提升后在途登记挑战作废：不能把旧代次授权延续到新一代。
	en2, err := svc.BeginFactorEnrollment(context.Background(), account, "local", "")
	if err != nil {
		t.Fatal(err)
	}
	secret2 := enrollmentSecret(t, en2.EnrollmentURI)
	err = factors.Write(context.Background(), func(tx *sqlx.Tx) error {
		_, err := repository.BumpAccountEpochTx(context.Background(), tx, id, account.Epoch)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmFactorEnrollment(context.Background(), id, en2.ChallengeToken, totpNow(t, secret2, svc.now()), ""); !errors.Is(err, ErrInvalidAuthChallenge) {
		t.Fatalf("代次提升后挑战仍可用：%v", err)
	}
}
