package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// ListTokens 返回当前用户名下未吊销的 API Token（不含明文）。
func (h *Handlers) ListTokens(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	rows, err := h.tokens.List(p.UserID)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	items := make([]Token, 0, len(rows))
	for _, t := range rows {
		items = append(items, toAPIToken(t))
	}
	c.JSON(http.StatusOK, TokenList{Items: items})
}

// CreateToken 为当前用户签发 API Token，明文仅此次返回一次。
func (h *Handlers) CreateToken(c *gin.Context) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	var req CreateTokenRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Name == "" {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "令牌名称不能为空")
		return
	}
	plaintext, tok, err := h.tokens.Create(p.UserID, req.Name)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, TokenCreated{
		Id:        tok.ID,
		Name:      tok.Name,
		Token:     plaintext,
		CreatedAt: tok.CreatedAt,
	})
}

// DeleteToken 吊销当前用户名下的 API Token。
func (h *Handlers) DeleteToken(c *gin.Context, id TokenIdParam) {
	p, ok := requirePrincipal(c)
	if !ok {
		return
	}
	if err := h.tokens.Delete(id, p.UserID); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
