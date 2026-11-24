package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// cmd 测试包的 TOTP 工具：与 internal/httpserver 的 testTotpAt 同一 RFC 6238 口径。
func cmdTestTotpAt(t *testing.T, secretB32 string, step int64) string {
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

func cmdTotpSecretFromURI(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("解析登记 URI：%v", err)
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		t.Fatalf("登记 URI 缺 secret：%s", uri)
	}
	return secret
}

// cmdInstallAdmin 走完整 claim_install 安装流（票据→受控自举→持票确认→登录→OTP 兑现），
// 返回已认证会话令牌。等价于 CLI 发票 + 管理员完成引导。
func cmdInstallAdmin(t *testing.T, server *httptest.Server, svc *appServices, username, password string,
	request func(method, path, token string, body, out any) int) string {
	t.Helper()
	ticket, err := svc.mfaSvc.IssueInstallClaimTicket(t.Context(), "test", "install")
	if err != nil {
		t.Fatalf("出具安装票：%v", err)
	}
	var boot api.LoginResponse
	if code := request(http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: username, Password: password, ClaimTicket: &ticket.Ticket}, &boot); code != http.StatusCreated {
		t.Fatalf("受控自举状态码 = %d，期望 201", code)
	}
	secret := cmdTotpSecretFromURI(t, *boot.EnrollmentUri)
	var codes api.MfaRecoveryCodes
	if code := request(http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: *boot.ChallengeToken, Otp: cmdTestTotpAt(t, secret, time.Now().Unix()/30), ClaimTicket: &ticket.Ticket}, &codes); code != http.StatusOK {
		t.Fatalf("安装确认状态码 = %d，期望 200", code)
	}
	var gated api.LoginResponse
	if code := request(http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: username, Password: password}, &gated); code != http.StatusOK || gated.Next != api.MfaRequired {
		t.Fatalf("安装后登录状态码 = %d 裁决 = %q，期望 mfa_required", code, gated.Next)
	}
	var last int64
	if err := svc.db.Get(&last,
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
		time.Sleep(time.Duration(step*30-time.Now().Unix()+1) * time.Second)
	}
	var done api.LoginResponse
	if code := request(http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: cmdTestTotpAt(t, secret, step)}, &done); code != http.StatusOK || done.Token == "" {
		t.Fatalf("安装后兑现状态码 = %d，期望 200 且签发会话", code)
	}
	return done.Token
}
