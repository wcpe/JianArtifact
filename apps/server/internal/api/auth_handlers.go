package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
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

// Login 用户名 + 口令换取会话 JWT。
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
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, LoginResponse{Token: token, User: toAPIUser(user)})
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
