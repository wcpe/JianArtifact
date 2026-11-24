package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// VerifyLoginMfa 兑现 login_mfa 挑战：OTP 命中签发完整会话，未命中统一 invalid_auth_challenge。
func (h *Handlers) VerifyLoginMfa(c *gin.Context) {
	if h.mfa == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req MfaVerifyRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.ChallengeToken == "" || req.Otp == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "挑战与一次性口令不能为空")
		return
	}
	token, user, err := h.auth.CompleteLoginMfa(c.Request.Context(), req.ChallengeToken, req.Otp, "")
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, LoginResponse{Next: Authenticated, Token: token, User: toAPIUser(user)})
}

// RecoverLoginMfa 凭 login_mfa 挑战（第一因素已验证）+ 恢复码换取 factor_recover 重绑挑战；
// 响应直接携带新登记 URI，客户端确认后重新登录。
func (h *Handlers) RecoverLoginMfa(c *gin.Context) {
	if h.mfa == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req MfaRecoverRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.ChallengeToken == "" || req.RecoveryCode == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "挑战与恢复码不能为空")
		return
	}
	ctx := c.Request.Context()
	challenge, err := h.mfa.TouchLoginChallenge(ctx, req.ChallengeToken)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	account, err := h.mfa.CredentialAccountByID(ctx, challenge.UserID)
	if err != nil || account.Epoch != challenge.AccountEpoch || account.Status != "active" || account.WebLoginDisabled {
		writeDomainErr(c, domain.ErrInvalidAuthChallenge)
		return
	}
	en, err := h.mfa.RecoverFactor(ctx, account, req.RecoveryCode, "recovery_code", "")
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, mfaEnrollResponse(en))
}

// GetMfaStatus 返回当前会话账号的强因子最小投影。
func (h *Handlers) GetMfaStatus(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	if h.mfa == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	st, err := h.mfa.Status(c.Request.Context(), p.UserID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, MfaStatusResponse{
		Enrolled:               st.Enrolled,
		EnrollmentPending:      st.EnrollmentPending,
		RecoveryCodesRemaining: st.RecoveryCodesRemaining,
		MfaRequired:            p.Role == "admin" && !st.Enrolled,
	})
}

// BeginMfaEnroll 发起因子登记：挑战绑定当前会话，登记 URI 只展示一次。
func (h *Handlers) BeginMfaEnroll(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	if h.mfa == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	if p.Kind != auth.KindSession {
		// 登记是交互式管理面动作，机器凭据不能代理。
		auth.WriteError(c, http.StatusForbidden, "forbidden", "需要会话凭据")
		return
	}
	account, err := h.mfa.CredentialAccountByID(c.Request.Context(), p.UserID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	en, err := h.mfa.BeginFactorEnrollment(c.Request.Context(), account, "session", p.SessionID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, mfaEnrollResponse(en))
}

// ConfirmMfaEnroll 用首枚 OTP 确认登记并返回恢复码批次。
// 同时服务两条路径：会话内 factor_enroll（挑战已绑会话）与恢复后 factor_recover
// （无会话可登，挑战本身即凭据）；绑定复核在领域层做，此处不强行要会话。
func (h *Handlers) ConfirmMfaEnroll(c *gin.Context) {
	if h.mfa == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req MfaConfirmRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.ChallengeToken == "" || req.Otp == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "挑战与一次性口令不能为空")
		return
	}
	binding := ""
	if p, has := auth.PrincipalFrom(c); has && p.Kind == auth.KindSession {
		binding = p.SessionID
	}
	var claimTicket string
	if req.ClaimTicket != nil {
		claimTicket = *req.ClaimTicket
	}
	codes, err := h.mfa.ConfirmFactorEnrollment(c.Request.Context(), req.ChallengeToken, req.Otp, claimTicket, binding)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, MfaRecoveryCodes{RecoveryCodes: codes})
}

// ClaimMfaEnroll 用第一因素 + 认领票进入受控登记：
// 管理员未登记强因子登录只会得到 claim_required，须持受控票在此换登记挑战，
// 再与票一起提交 enroll/confirm 完成一次认领。无会话要求（登录不上的人才需要认领）。
func (h *Handlers) ClaimMfaEnroll(c *gin.Context) {
	if h.mfa == nil {
		writeDomainErr(c, domain.ErrMfaFactorUnavailable)
		return
	}
	var req MfaClaimRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" || req.ClaimTicket == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "账号、口令与认领票不能为空")
		return
	}
	binding := ""
	if p, has := auth.PrincipalFrom(c); has && p.Kind == auth.KindSession {
		binding = p.SessionID
	}
	en, err := h.mfa.RedeemClaimTicket(c.Request.Context(), req.Username, req.Password, req.ClaimTicket, binding)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, mfaEnrollResponse(en))
}

func mfaEnrollResponse(en *domain.MfaEnrollment) MfaEnrollResponse {
	return MfaEnrollResponse{
		ChallengeId:    en.ChallengeID,
		ChallengeToken: en.ChallengeToken,
		ExpiresAt:      en.ExpiresAt.UTC().Format(time.RFC3339),
		EnrollmentUri:  en.EnrollmentURI,
	}
}
