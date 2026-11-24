// 强认证登录门端到端：登记→确认→login_mfa 挑战→OTP 兑现会话→恢复码重绑。
package httpserver_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// testTotpAt 用 RFC 6238 口径计算指定步的 OTP（与领域实现同一算法，避免跨包私用）。
func testTotpAt(t *testing.T, secretB32 string, step int64) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secretB32))
	if err != nil {
		t.Fatalf("解码 TOTP 种子：%v", err)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off]&0x7f) << 24) | (uint32(sum[off+1]) << 16) | (uint32(sum[off+2]) << 8) | uint32(sum[off+3])
	out := ""
	for i := 0; i < 6; i++ {
		out = string(rune('0'+code%10)) + out
		code /= 10
	}
	return out
}

func testTotp(t *testing.T, secretB32 string, at time.Time) string {
	t.Helper()
	return testTotpAt(t, secretB32, at.Unix()/30)
}

// nextStepOtp 计算「严格大于已接受水位且在 ±1 验证窗内」的下一步 OTP：
// 登记确认与验证都消耗步水位，紧跟其后的验证必须落到下一可用步；
// 罕见边界下水位已领先墙钟一步，等墙钟追平至多一个周期。
func nextStepOtp(t *testing.T, e *protocolEnv, username, secretB32 string) string {
	t.Helper()
	var last int64
	if err := e.db.Get(&last,
		`SELECT f.last_accepted_step FROM security_totp_factor f JOIN user u ON u.id=f.user_id
		 WHERE u.username=? AND f.state='active'`, username); err != nil {
		t.Fatalf("读取防重放水位：%v", err)
	}
	now := time.Now().Unix() / 30
	step := now
	if step <= last {
		step = last + 1
	}
	if step > now+1 {
		// 水位领先墙钟：等墙钟进入所需步（至多 31 秒）。
		time.Sleep(time.Duration(step*30-time.Now().Unix()+1) * time.Second)
	}
	return testTotpAt(t, secretB32, step)
}

// totpSecretFromURI 从 otpauth:// 登记 URI 解出 base32 种子。
func totpSecretFromURI(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("解析登记 URI：%v", err)
	}
	s := u.Query().Get("secret")
	if s == "" {
		t.Fatal("登记 URI 缺 secret")
	}
	return s
}

// installAdmin 走完整 claim_install 安装流（票据→自举→持票确认→登录→OTP 兑现），
// 返回已认证会话令牌、TOTP 种子与恢复码批次；等价于 CLI 发票 + 管理员完成引导。
func installAdmin(t *testing.T, e *protocolEnv, username, password string) (string, string, []string) {
	t.Helper()
	ticket, err := e.mfaSvc.IssueInstallClaimTicket(t.Context(), "test", "install")
	if err != nil {
		t.Fatalf("出具安装票：%v", err)
	}
	var boot api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: username, Password: password, ClaimTicket: &ticket.Ticket}, &boot); code != http.StatusCreated {
		t.Fatalf("受控自举状态码 = %d，期望 201", code)
	}
	secret := totpSecretFromURI(t, *boot.EnrollmentUri)
	var codes api.MfaRecoveryCodes
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: *boot.ChallengeToken, Otp: testTotp(t, secret, time.Now()), ClaimTicket: &ticket.Ticket}, &codes); code != http.StatusOK {
		t.Fatalf("安装确认状态码 = %d，期望 200", code)
	}
	var gated api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: username, Password: password}, &gated); code != http.StatusOK || gated.Next != api.MfaRequired {
		t.Fatalf("安装后登录状态码 = %d 裁决 = %q，期望 mfa_required", code, gated.Next)
	}
	var done api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: nextStepOtp(t, e, username, secret)}, &done); code != http.StatusOK || done.Token == "" {
		t.Fatalf("安装后兑现状态码 = %d，期望 200 且签发会话", code)
	}
	return done.Token, secret, codes.RecoveryCodes
}

func TestMfaLoginGateEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)

	// 空库自举须持票：缺票 → 401；持票安装后即带激活因子。
	var bad map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("缺票自举状态码 = %d，期望 401", code)
	}
	_, secret, codes := installAdmin(t, e, "admin", "admin-pass-123")

	// 登录门：同账号再登录必须得到 mfa_required + 挑战，不签会话。
	var gated api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "admin", Password: "admin-pass-123"}, &gated); code != http.StatusOK {
		t.Fatalf("登录状态码 = %d，期望 200", code)
	}
	if gated.Next != api.MfaRequired || gated.Token != "" || gated.ChallengeToken == nil || *gated.ChallengeToken == "" {
		t.Fatalf("登录门裁决 = %q token=%q，期望 mfa_required 且无会话令牌", gated.Next, gated.Token)
	}

	// 错误 OTP：统一 invalid_auth_challenge，不签会话。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: "000000"}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("错误 OTP 状态码 = %d，期望 401", code)
	}

	// 正确 OTP：兑现挑战签发会话。
	var done api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: nextStepOtp(t, e, "admin", secret)}, &done); code != http.StatusOK || done.Token == "" {
		t.Fatalf("OTP 兑现状态码 = %d，期望 200 且签发会话", code)
	}

	// 挑战单用途：同一挑战再次兑现被拒。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: nextStepOtp(t, e, "admin", secret)}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("挑战重放状态码 = %d，期望 401", code)
	}

	// 会话可用：状态接口回报已登记。
	var st api.MfaStatusResponse
	if code := e.jsonReq(t, http.MethodGet, "/api/v1/auth/mfa/status", done.Token, nil, &st); code != http.StatusOK || !st.Enrolled || st.RecoveryCodesRemaining != 10 {
		t.Fatalf("状态码 = %d，enrolled=%v codes=%d，期望已登记且 10 枚码", code, st.Enrolled, st.RecoveryCodesRemaining)
	}

	// 恢复路径：登录→mfa 挑战→恢复码→factor_recover 重绑 URI→确认→重新登录。
	var gated2 api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "admin", Password: "admin-pass-123"}, &gated2); code != http.StatusOK || gated2.Next != api.MfaRequired {
		t.Fatalf("恢复前置登录状态码 = %d，期望 mfa_required", code)
	}
	var rec api.MfaEnrollResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/recover", "",
		api.MfaRecoverRequest{ChallengeToken: *gated2.ChallengeToken, RecoveryCode: codes[0]}, &rec); code != http.StatusOK || rec.EnrollmentUri == "" {
		t.Fatalf("恢复状态码 = %d，期望 200 且返回重绑 URI", code)
	}
	newSecret := totpSecretFromURI(t, rec.EnrollmentUri)
	var newCodes api.MfaRecoveryCodes
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: rec.ChallengeToken, Otp: testTotp(t, newSecret, time.Now())}, &newCodes); code != http.StatusOK {
		t.Fatalf("重绑确认状态码 = %d，期望 200", code)
	}
	// 旧恢复码全部作废。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/recover", "",
		api.MfaRecoverRequest{ChallengeToken: *gated2.ChallengeToken, RecoveryCode: codes[1]}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("旧恢复码重放状态码 = %d，期望 401", code)
	}
	// 恢复要求重新登录：口令 → 新因子 OTP → 会话。
	var gated3 api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "admin", Password: "admin-pass-123"}, &gated3); code != http.StatusOK || gated3.Next != api.MfaRequired {
		t.Fatalf("恢复后登录状态码 = %d，期望 mfa_required", code)
	}
	var relogin api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated3.ChallengeToken, Otp: nextStepOtp(t, e, "admin", newSecret)}, &relogin); code != http.StatusOK || relogin.Token == "" {
		t.Fatalf("恢复后兑现状态码 = %d，期望 200 且签发会话", code)
	}

	// ── 受控认领：未登记管理员登录只得 claim_required，须持票完成认领。 ──
	adminRole := api.CreateUserRequestRoleAdmin
	var admin2 api.User
	// done 会话已被恢复流程的账号代次提升撤销，使用恢复后的会话。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", relogin.Token,
		api.CreateUserRequest{Username: "admin-claim", Password: "claim-pass-123", Role: &adminRole}, &admin2); code != http.StatusCreated {
		t.Fatalf("创建管理员状态码 = %d，期望 201", code)
	}
	var noFactor api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "admin-claim", Password: "claim-pass-123"}, &noFactor); code != http.StatusOK {
		t.Fatalf("未登记管理员登录状态码 = %d，期望 200", code)
	}
	if noFactor.Next != api.ClaimRequired || noFactor.Token != "" {
		t.Fatalf("未登记管理员裁决 = %q，期望 claim_required 且无会话令牌", noFactor.Next)
	}

	// 本机受控 CLI 出具 claim_admin 票（测试直连领域服务，等价 admin mfa-claim 命令）。
	ticket, err := e.mfaSvc.IssueClaimTicket(t.Context(), "admin-claim", repository.ClaimPurposeAdmin, "cli", "test-claim")
	if err != nil {
		t.Fatalf("出具认领票：%v", err)
	}
	var claimEn api.MfaEnrollResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/claim", "",
		api.MfaClaimRequest{Username: "admin-claim", Password: "claim-pass-123", ClaimTicket: ticket.Ticket}, &claimEn); code != http.StatusOK || claimEn.EnrollmentUri == "" {
		t.Fatalf("认领兑换状态码 = %d，期望 200 且返回登记 URI", code)
	}
	claimSecret := totpSecretFromURI(t, claimEn.EnrollmentUri)

	// 认领挑战缺票确认 → 401（一次认领成立要求）。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: claimEn.ChallengeToken, Otp: testTotp(t, claimSecret, time.Now())}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("缺票确认状态码 = %d，期望 401", code)
	}
	claimTicket := ticket.Ticket
	var claimCodes api.MfaRecoveryCodes
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: claimEn.ChallengeToken, Otp: testTotp(t, claimSecret, time.Now()), ClaimTicket: &claimTicket}, &claimCodes); code != http.StatusOK || len(claimCodes.RecoveryCodes) != 10 {
		t.Fatalf("认领确认状态码 = %d，期望 200 且 10 枚码", code)
	}

	// 票一次性：同票再次兑换被拒。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/claim", "",
		api.MfaClaimRequest{Username: "admin-claim", Password: "claim-pass-123", ClaimTicket: ticket.Ticket}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("票重放状态码 = %d，期望 401", code)
	}

	// 认领完成 → 登录走标准 mfa_required 门。
	var cg api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "admin-claim", Password: "claim-pass-123"}, &cg); code != http.StatusOK || cg.Next != api.MfaRequired {
		t.Fatalf("认领后登录状态码 = %d 裁决 = %q，期望 mfa_required", code, cg.Next)
	}
	var cdone api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *cg.ChallengeToken, Otp: nextStepOtp(t, e, "admin-claim", claimSecret)}, &cdone); code != http.StatusOK || cdone.Token == "" {
		t.Fatalf("认领后兑现状态码 = %d，期望 200 且签发会话", code)
	}

	// ── step_up：水位过期会话做敏感写入被 403 拦截，step-up 兑现后放行。 ──
	// cdone 由 OTP 兑现签发，自带水位；清库水位模拟窗口过期（不可时间旅行）。
	var sessID string
	if err := e.db.Get(&sessID,
		`SELECT session_id FROM credential_session WHERE user_id=? ORDER BY expires_at DESC LIMIT 1`, admin2.Id); err != nil {
		t.Fatalf("查会话：%v", err)
	}
	if _, err := e.db.Exec(`UPDATE credential_session SET mfa_verified_at=NULL WHERE session_id=?`, sessID); err != nil {
		t.Fatalf("清水位：%v", err)
	}
	var rootID int64
	if err := e.db.Get(&rootID, `SELECT id FROM user WHERE username='admin'`); err != nil {
		t.Fatalf("查根管理员：%v", err)
	}
	pwdBody := api.PasswordChangeRequest{Password: "claim-pass-rotated-1"}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users/"+itoa64(rootID)+"/password", cdone.Token,
		pwdBody, &bad); code != http.StatusForbidden {
		t.Fatalf("无水位跨账户改密状态码 = %d，期望 403 recent_mfa_required", code)
	}
	var stepup api.MfaStepUpChallenge
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/step-up", cdone.Token, nil, &stepup); code != http.StatusOK || stepup.ChallengeToken == "" {
		t.Fatalf("step-up 签发状态码 = %d，期望 200", code)
	}
	var verified api.MfaVerifiedResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/step-up/verify", cdone.Token,
		api.MfaVerifyRequest{ChallengeToken: stepup.ChallengeToken, Otp: nextStepOtp(t, e, "admin-claim", claimSecret)}, &verified); code != http.StatusOK || verified.MfaVerifiedAt == "" {
		t.Fatalf("step-up 兑现状态码 = %d，期望 200 且返回水位", code)
	}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users/"+itoa64(rootID)+"/password", cdone.Token,
		pwdBody, nil); code != http.StatusNoContent {
		t.Fatalf("step-up 后跨账户改密状态码 = %d，期望 204", code)
	}
}
