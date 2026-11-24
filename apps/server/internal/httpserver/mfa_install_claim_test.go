// claim_install 安装认领端到端：空库首管理员须票引导，票与因子激活同事务。
package httpserver_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestMfaInstallClaimEndToEnd(t *testing.T) {
	e := newProtocolEnv(t)

	// 本机受控 CLI 出具安装认领票（仅未初始化库可出具）。
	ticket, err := e.mfaSvc.IssueInstallClaimTicket(t.Context(), "cli", "install-test")
	if err != nil {
		t.Fatalf("出具安装票：%v", err)
	}
	if ticket.Purpose != repository.ClaimPurposeInstall {
		t.Fatalf("票用途 = %q，期望 claim_install", ticket.Purpose)
	}

	// 持票自举：不直签会话，返回 claim_required + 登记挑战 + URI。
	var boot api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "root", Password: "root-pass-123", ClaimTicket: &ticket.Ticket}, &boot); code != http.StatusCreated {
		t.Fatalf("受控自举状态码 = %d，期望 201", code)
	}
	if boot.Next != api.ClaimRequired || boot.Token != "" || boot.ChallengeToken == nil || boot.EnrollmentUri == nil {
		t.Fatalf("受控自举裁决 = %q token=%q，期望 claim_required + 挑战 + URI", boot.Next, boot.Token)
	}
	secret := totpSecretFromURI(t, *boot.EnrollmentUri)

	// 缺票确认 → 401。
	var bad map[string]any
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: *boot.ChallengeToken, Otp: testTotp(t, secret, time.Now())}, &bad); code != http.StatusUnauthorized {
		t.Fatalf("缺票确认状态码 = %d，期望 401", code)
	}
	var codes api.MfaRecoveryCodes
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "",
		api.MfaConfirmRequest{ChallengeToken: *boot.ChallengeToken, Otp: testTotp(t, secret, time.Now()), ClaimTicket: &ticket.Ticket}, &codes); code != http.StatusOK || len(codes.RecoveryCodes) != 10 {
		t.Fatalf("持票确认状态码 = %d，期望 200 且 10 枚码", code)
	}

	// 票一次性：确认后不可再用。
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "root2", Password: "root-pass-456", ClaimTicket: &ticket.Ticket}, &bad); code != http.StatusUnauthorized && code != http.StatusConflict {
		t.Fatalf("票重放状态码 = %d，期望 401/409", code)
	}

	// 完成认领后：标准 mfa 登录门。
	var gated api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "root", Password: "root-pass-123"}, &gated); code != http.StatusOK || gated.Next != api.MfaRequired {
		t.Fatalf("安装后登录状态码 = %d 裁决 = %q，期望 mfa_required", code, gated.Next)
	}
	var done api.LoginResponse
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/auth/mfa/verify", "",
		api.MfaVerifyRequest{ChallengeToken: *gated.ChallengeToken, Otp: nextStepOtp(t, e, "root", secret)}, &done); code != http.StatusOK || done.Token == "" {
		t.Fatalf("兑现状态码 = %d，期望 200 且签发会话", code)
	}
}
