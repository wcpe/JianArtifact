package api

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
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
	var names map[int64]string
	seen := map[int64]bool{}
	var ids []int64
	for _, t := range rows {
		if scopes, err := auth.ParseMachineScopes(t.ScopesJSON); err == nil {
			for _, sc := range scopes {
				if !seen[sc.RepositoryID] {
					seen[sc.RepositoryID] = true
					ids = append(ids, sc.RepositoryID)
				}
			}
		}
	}
	if len(ids) > 0 && h.repos != nil {
		names = h.repos.RepositoryNames(ids)
	}
	items := make([]Token, 0, len(rows))
	for _, t := range rows {
		items = append(items, toAPIToken(t, names))
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
	in := domain.CreateTokenInput{Name: req.Name}
	if req.Purpose != nil {
		in.Purpose = string(*req.Purpose)
	}
	if req.ExpiresDays != nil {
		in.ExpiresDays = *req.ExpiresDays
	}
	if req.Scopes != nil {
		in.Scopes = make([]domain.MachineTokenScopeInput, 0, len(*req.Scopes))
		for _, sc := range *req.Scopes {
			item := domain.MachineTokenScopeInput{Repository: sc.Repository}
			if sc.PathPrefix != nil {
				item.PathPrefix = *sc.PathPrefix
			}
			for _, a := range sc.Actions {
				item.Actions = append(item.Actions, string(a))
			}
			in.Scopes = append(in.Scopes, item)
		}
	}
	plaintext, tok, err := h.tokens.CreateScopedForPrincipal(c.Request.Context(), p, in)
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	// 签发未限用途旧式令牌时给出弃用警告：标准 Warning 头（HTTP 规范的弃用告示通道）
	// + 服务端日志。时间盒见 auth.LegacyTokenSunsetDate。
	if tok.Purpose == auth.TokenPurposeLegacy {
		c.Header("Warning", `299 - "jat_ 未限用途令牌已弃用，`+auth.LegacyTokenSunsetDate+` 起停止签发；请改用 purpose=artifact_read/artifact_publish 的受限机器凭据"`)
		log.Printf("签发已弃用的未限用途令牌 user=%d name=%q：%s 起停止签发", p.UserID, tok.Name, auth.LegacyTokenSunsetDate)
	}
	h.AuditLog(c, "token.create", "token", tok.Name, "", "tokenId="+strconv.FormatInt(tok.ID, 10), "ok")
	created := TokenCreated{
		Id:        tok.ID,
		Name:      tok.Name,
		Token:     plaintext,
		CreatedAt: tok.CreatedAt,
	}
	if tok.Purpose != "" {
		purpose := TokenCreatedPurpose(tok.Purpose)
		created.Purpose = &purpose
	}
	if tok.ExpiresAt != nil {
		expiresAt := time.Unix(*tok.ExpiresAt, 0).UTC().Format(time.RFC3339)
		created.ExpiresAt = &expiresAt
	}
	c.JSON(http.StatusCreated, created)
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
	h.AuditLog(c, "token.revoke", "token", strconv.FormatInt(id, 10), "", "", "ok")
	c.Status(http.StatusNoContent)
}
