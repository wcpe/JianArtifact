package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// AnonymousAccessSetting 是匿名访问全局开关端点的请求 / 响应体（FR-66）。
// 非契约端点，与 tree/search/cleanup 同先例，后续统一收编进 OpenAPI 契约。
type AnonymousAccessSetting struct {
	Enabled *bool `json:"enabled"`
}

// GetAnonymousAccessSetting 读取匿名访问全局开关，仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetAnonymousAccessSetting(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	enabled, err := h.settings.AnonymousAccessEnabled()
	if err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

// PutAnonymousAccessSetting 写入匿名访问全局开关，仅管理员。
// 非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) PutAnonymousAccessSetting(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	var req AnonymousAccessSetting
	if !bindJSON(c, &req) {
		return
	}
	if req.Enabled == nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", "缺少 enabled 字段")
		return
	}
	if err := h.settings.SetAnonymousAccessEnabled(*req.Enabled); err != nil {
		writeDomainErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": *req.Enabled})
}
