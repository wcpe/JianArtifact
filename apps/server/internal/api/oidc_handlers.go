package api

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// oidcFlowCookiePath 把流程 Cookie 限定在 OIDC 端点路径下，随回调一并发送、不扩散到其它接口。
const oidcFlowCookiePath = "/api/v1/auth/oidc"

// OIDCLogin 是 OIDC 协议侧能力（由 auth.OIDCVerifier 实现；测试可替换）。
type OIDCLogin interface {
	NewFlow() auth.OIDCFlow
	AuthCodeURL(ctx context.Context, flow auth.OIDCFlow) (string, error)
	Exchange(ctx context.Context, code string, flow auth.OIDCFlow) (auth.ExternalUser, error)
}

// OIDCDeps 汇集 OIDC 登录端点所需依赖（FR-34）；为 nil 表示未启用 OIDC 登录。
type OIDCDeps struct {
	Verifier       OIDCLogin            // 协议侧校验器
	FlowSigner     *auth.OIDCFlowSigner // 流程状态签名器（启动密钥派生）
	AllowedDomains []string             // 允许自动建号的邮箱域名；空 = 不限制
	RedirectAfter  string               // 回前端的基础路径（如 /login）
}

// StartOidcLogin 起跳授权码流程（FR-34）：生成短时流程状态（签名 Cookie 承载，无服务端状态），
// 再重定向到 IdP。未启用 OIDC 时返回 404。
func (h *Handlers) StartOidcLogin(c *gin.Context) {
	if h.oidc == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	flow := h.oidc.Verifier.NewFlow()
	encoded, err := h.oidc.FlowSigner.Encode(flow)
	if err != nil {
		auth.WriteError(c, http.StatusInternalServerError, "internal_error", "生成登录流程状态失败")
		return
	}
	h.setOIDCFlowCookie(c, encoded, int(auth.OIDCFlowLifetime.Seconds()))
	target, err := h.oidc.Verifier.AuthCodeURL(c.Request.Context(), flow)
	if err != nil {
		// IdP 不可达：详细原因进日志，对外只回笼统错误码。
		log.Printf("OIDC 登录起跳失败：%v", err)
		h.oidcFail(c, "provider_unavailable")
		return
	}
	c.Redirect(http.StatusFound, target)
}

// CompleteOidcLogin 完成授权码流程（FR-34）：校验流程状态 → 兑换并校验身份 → 绑定或建号 →
// 携会话令牌重定向前端。令牌经 URL 片段下发（片段不进入服务端访问日志）。
func (h *Handlers) CompleteOidcLogin(c *gin.Context, params CompleteOidcLoginParams) {
	if h.oidc == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	// 流程状态只消费一次：无论成败都先清除 Cookie。
	h.setOIDCFlowCookie(c, "", -1)

	if providerErr := derefString(params.Error); providerErr != "" {
		log.Printf("OIDC 提供方返回错误：%s", providerErr)
		h.oidcFail(c, "provider_error")
		return
	}
	value, err := c.Cookie(auth.OIDCFlowCookieName)
	if err != nil {
		h.oidcFail(c, "flow_expired")
		return
	}
	flow, err := h.oidc.FlowSigner.Decode(value, time.Now())
	if err != nil {
		log.Printf("OIDC 流程状态不可用：%v", err)
		h.oidcFail(c, "flow_expired")
		return
	}
	if derefString(params.State) != flow.State {
		log.Print("OIDC 回调 state 与流程状态不符，疑似跨站重放")
		h.oidcFail(c, "state_mismatch")
		return
	}
	code := derefString(params.Code)
	if code == "" {
		h.oidcFail(c, "missing_code")
		return
	}
	claims, err := h.oidc.Verifier.Exchange(c.Request.Context(), code, flow)
	if err != nil {
		log.Printf("OIDC 身份校验失败：%v", err)
		h.oidcFail(c, "verify_failed")
		return
	}
	if !h.oidcEmailAllowed(claims.Email) {
		log.Printf("OIDC 登录被域名白名单拒绝：%s", claims.Email)
		h.oidcFail(c, "not_allowed")
		return
	}
	token, _, err := h.auth.LoginExternal(domain.ExternalIdentity{
		Source:   "oidc",
		Subject:  claims.Subject,
		Username: claims.Username,
		Email:    claims.Email,
	})
	if err != nil {
		log.Printf("OIDC 登录被拒绝（%s）：%v", claims.Username, err)
		h.oidcFail(c, "login_rejected")
		return
	}
	c.Redirect(http.StatusFound, h.oidc.RedirectAfter+"#token="+url.QueryEscape(token))
}

// oidcFail 以片段错误码重定向前端登录页；详细原因只进服务端日志。
func (h *Handlers) oidcFail(c *gin.Context, code string) {
	c.Redirect(http.StatusFound, h.oidc.RedirectAfter+"#error="+url.QueryEscape(code))
}

// oidcEmailAllowed 判定邮箱域名白名单（空名单表示不限制；配了名单则必须命中）。
func (h *Handlers) oidcEmailAllowed(email string) bool {
	if len(h.oidc.AllowedDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, allowed := range h.oidc.AllowedDomains {
		if domain == allowed {
			return true
		}
	}
	return false
}

// setOIDCFlowCookie 写入/清除流程 Cookie（HttpOnly，限定 OIDC 路径，SameSite=Lax）。
func (h *Handlers) setOIDCFlowCookie(c *gin.Context, value string, maxAge int) {
	c.SetSameSite(http.SameSiteLaxMode)
	secure := c.Request.TLS != nil || strings.HasPrefix(h.publicURL, "https://")
	c.SetCookie(auth.OIDCFlowCookieName, value, maxAge, oidcFlowCookiePath, "", secure, true)
}
