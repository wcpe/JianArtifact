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
	c.JSON(http.StatusCreated, LoginResponse{Token: token, User: toAPIUser(user)})
}

// GetCurrentUser 返回当前会话对应的用户：OIDC 回调后前端据此取身份快照，
// 避免把用户信息塞进 URL 片段（FR-34）。
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

// Login 用户名 + 口令换取会话 JWT；本地口令优先，未通过且启用 LDAP 时再试目录（FR-35）。
func (h *Handlers) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "用户名与口令不能为空")
		return
	}
	token, user, err := h.auth.Login(req.Username, req.Password)
	if err == nil {
		c.JSON(http.StatusOK, LoginResponse{Token: token, User: toAPIUser(user)})
		return
	}
	// 本地未通过：启用 LDAP 时再试目录。目录侧的身份同样要过 AuthService 的绑定/建号与
	// 状态校验（管理员不会被自动绑定、内置主体不可登录），故这里不绕过领域逻辑。
	if h.ldapAuth != nil && errors.Is(err, domain.ErrInvalidCredentials) {
		if identity, ldapErr := h.ldapAuth(c.Request.Context(), req.Username, req.Password); ldapErr == nil {
			external := domain.ExternalIdentity{
				Source:   "ldap",
				Subject:  identity.Subject,
				Username: identity.Username,
				Email:    identity.Email,
			}
			if t, u, externalErr := h.auth.LoginExternal(external); externalErr == nil {
				c.JSON(http.StatusOK, LoginResponse{Token: t, User: toAPIUser(u)})
				return
			}
		}
	}
	// 本地与目录都未通过：对外表现与纯本地失败完全一致（不泄露账号存在于哪一侧）。
	writeDomainErr(c, err)
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
