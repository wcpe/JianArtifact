package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// Bootstrap 首启管理员自举：仅当 user 表为空时开放，创建首个管理员并返回会话。
func (h *Handlers) Bootstrap(c *gin.Context) {
	var req BootstrapRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "用户名与口令不能为空")
		return
	}
	token, user, err := h.auth.Bootstrap(req.Username, req.Password)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, LoginResponse{Next: Authenticated, Token: token, User: toAPIUser(user)})
}

// GetCurrentUser 返回当前会话对应的用户：OIDC 回调后前端据此取身份快照，
// 避免把用户信息塞进 URL 片段。
func (h *Handlers) GetCurrentUser(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	u, err := h.users.Get(p.UserID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAPIUser(u))
}

// Login 用户名 + 口令换取会话 JWT；本地口令优先，未通过且启用 LDAP 时再试目录。
func (h *Handlers) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "用户名与口令不能为空")
		return
	}
	ctx := c.Request.Context()
	var revision int64
	if h.ldapAuth != nil {
		var err error
		revision, err = h.auth.CredentialRevision(ctx)
		if err != nil {
			writeDomainErr(c, domain.ErrSecurityAuditUnavailable)
			return
		}
	}
	outcome, err := h.auth.LoginOutcome(ctx, req.Username, req.Password, "")
	if err == nil {
		writeLoginOutcome(c, outcome)
		return
	}
	// 本地未通过：启用 LDAP 时再试目录。目录侧的身份同样要过 AuthService 的绑定/建号与
	// 状态校验（管理员不会被自动绑定、内置主体不可登录），故这里不绕过领域逻辑。
	if h.ldapAuth != nil && errors.Is(err, domain.ErrInvalidCredentials) {
		if identity, ldapErr := h.ldapAuth(ctx, req.Username, req.Password); ldapErr == nil {
			external := domain.ExternalIdentity{
				Source:   "ldap",
				Subject:  identity.Subject,
				Username: identity.Username,
				Email:    identity.Email,
			}
			if ext, externalErr := h.auth.LoginExternalOutcome(ctx, external, revision, ""); externalErr == nil {
				writeLoginOutcome(c, ext)
				return
			}
		} else {
			h.auth.RecordExternalAuthRejection(ctx, "ldap", "invalid_credentials", auth.ClassifyProviderAuthFailure(ldapErr))
		}
	}
	// 本地与目录都未通过：对外表现与纯本地失败完全一致（不泄露账号存在于哪一侧）。
	writeDomainErr(c, err)
}

// writeLoginOutcome 把签发裁决映射为统一登录响应。
func writeLoginOutcome(c *gin.Context, outcome *domain.AuthOutcome) {
	resp := LoginResponse{Next: Authenticated, User: toAPIUser(outcome.User)}
	switch outcome.Next {
	case domain.AuthNextMfaRequired:
		resp.Next = MfaRequired
		resp.ChallengeId = &outcome.ChallengeID
		resp.ChallengeToken = &outcome.ChallengeToken
		exp := outcome.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		resp.ChallengeExpiresAt = &exp
	case domain.AuthNextClaimRequired:
		resp.Next = ClaimRequired
	default:
		resp.Token = outcome.Token
	}
	c.JSON(http.StatusOK, resp)
}

// Logout 注销当前会话：会话 jti 记入吊销名单直至过期。API Token 凭据登出为无操作。
func (h *Handlers) Logout(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	var expiresAt int64
	if !p.ExpiresAt.IsZero() {
		expiresAt = p.ExpiresAt.Unix()
	}
	if err := h.auth.Logout(p.JTI, expiresAt); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
