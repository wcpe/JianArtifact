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
func nextStepOtp(t *testing.T, e *protocolEnv, secretB32 string) string {
	t.Helper()
	var last int64
	if err := e.db.Get(&last, `SELECT last_accepted_step FROM security_totp_factor WHERE state='active'`); err != nil {
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

func TestMfaLoginGateEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)

	// 自举 + 普通登录直通（未启用因子时 next=authenticated）。
	var boot api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated || boot.Token == "" {
		t.Fatalf("自举状态码 = %d，期望 201 且签发会话", code)
	}

	// 登记：挑战 + otpauth URI。
	var en api.MfaEnrollResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll", boot.Token, nil, &en); code != http.StatusOK {
		t.Fatalf("发起登记状态码 = %d，期望 200", code)
	}
	secret := totpSecretFromURI(t, en.EnrollmentUri)

	// 确认：首枚 OTP 换恢复码批次。
	var codes api.MfaRecoveryCodes
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", boot.Token,
		api.MfaConfirmRequest{ChallengeToken: en.ChallengeToken, Otp: testTotp(t, secret, time.Now())}, &codes); code != http.StatusOK || len(codes.RecoveryCodes) != 10 {
		t.Fatalf("确认登记状态码 = %d，码数 = %d，期望 200 且 10 枚", code, len(codes.RecoveryCodes))
	}

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
	var bad map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: "000000"}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("错误 OTP 状态码 = %d，期望 401", code)
	}

	// 正确 OTP：兑现挑战签发会话。
	var done api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: nextStepOtp(t, e, secret)}, &done); code != http.StatusOK || done.Token == "" {
		t.Fatalf("OTP 兑现状态码 = %d，期望 200 且签发会话", code)
	}

	// 挑战单用途：同一挑战再次兑现被拒。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: nextStepOtp(t, e, secret)}, &bad); code != http.StatusUnauthorized {
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
		api.MfaRecoverRequest{ChallengeToken: *gated2.ChallengeToken, RecoveryCode: codes.RecoveryCodes[0]}, &rec); code != http.StatusOK || rec.EnrollmentUri == "" {
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
		api.MfaRecoverRequest{ChallengeToken: *gated2.ChallengeToken, RecoveryCode: codes.RecoveryCodes[1]}, &bad); code != http.StatusUnauthorized {
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
		api.MfaVerifyRequest{ChallengeToken: *gated3.ChallengeToken, Otp: nextStepOtp(t, e, newSecret)}, &relogin); code != http.StatusOK || relogin.Token == "" {
		t.Fatalf("恢复后兑现状态码 = %d，期望 200 且签发会话", code)
	}
}
