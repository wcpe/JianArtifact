package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// 设置端点的边界（FR-89）：秒级配置允许范围与缺省展示值。
const (
	minConfigSecs           = 1
	maxConfigSecs           = 3600
	defaultSyncIntervalSecs = 5
	defaultUpstreamTimeout  = 30
)

// SettingsResponse 是设置端点（FR-89）的响应体：基础配置四项的当前生效值。
type SettingsResponse struct {
	AnonymousAccess bool   `json:"anonymousAccess"`
	PublicURL       string `json:"publicUrl"`
	UpstreamTimeout int    `json:"upstreamTimeout"`
	SyncInterval    int    `json:"syncInterval"`
}

// SettingsRequest 是设置端点的请求体：可选字段，传哪个改哪个（对齐 cluster 端点风格）。
type SettingsRequest struct {
	AnonymousAccess *bool   `json:"anonymousAccess,omitempty"`
	PublicURL       *string `json:"publicUrl,omitempty"`
	UpstreamTimeout *int    `json:"upstreamTimeout,omitempty"`
	SyncInterval    *int    `json:"syncInterval,omitempty"`
}

// settingsSnapshot 读取基础配置四项的当前生效值（未配置的间隔 / 超时回退默认值）。
func (h *Handlers) settingsSnapshot() SettingsResponse {
	anonymous := true
	publicURL := ""
	syncSecs := defaultSyncIntervalSecs
	timeoutSecs := defaultUpstreamTimeout
	if h.settings != nil {
		if v, err := h.settings.AnonymousAccessEnabled(); err == nil {
			anonymous = v
		}
		publicURL = h.settings.PublicURL()
		if v := h.settings.SyncIntervalSecs(); v > 0 {
			syncSecs = v
		}
		if v := h.settings.UpstreamTimeoutSecs(); v > 0 {
			timeoutSecs = v
		}
	}
	return SettingsResponse{
		AnonymousAccess: anonymous,
		PublicURL:       publicURL,
		UpstreamTimeout: timeoutSecs,
		SyncInterval:    syncSecs,
	}
}

// GetSettings 返回基础配置四项（FR-89），仅管理员。非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetSettings(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	c.JSON(http.StatusOK, h.settingsSnapshot())
}

// PutSettings 部分写入基础配置（FR-89），仅管理员。写后运行时生效：
// 匿名开关 / 对外 URL 由读取方动态读 setting；同步间隔由调度器下一轮应用；
// 回源超时经 OnUpstreamTimeoutChange 回调立即同步到回源客户端。
func (h *Handlers) PutSettings(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.settings == nil {
		auth.WriteError(c, http.StatusConflict, "conflict", "设置服务未就绪")
		return
	}
	var req SettingsRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := validateSettingsRequest(req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := h.settings.UpdateSettings(domain.SettingsUpdate{
		AnonymousAccess: req.AnonymousAccess,
		PublicURL:       req.PublicURL,
		UpstreamTimeout: req.UpstreamTimeout,
		SyncInterval:    req.SyncInterval,
	}); err != nil {
		writeDomainErr(c, err)
		return
	}
	if req.UpstreamTimeout != nil && h.onUpstreamTimeoutChange != nil {
		h.onUpstreamTimeoutChange(time.Duration(*req.UpstreamTimeout) * time.Second)
	}
	h.AuditLog(c, "setting.set", "setting", "settings", "", "基础设置更新", "ok")
	c.JSON(http.StatusOK, h.settingsSnapshot())
}

// validateSettingsRequest 在写入前完成全部字段校验，避免非法请求产生部分更新。
func validateSettingsRequest(req SettingsRequest) error {
	if req.PublicURL != nil {
		if err := validatePublicURL(*req.PublicURL); err != nil {
			return err
		}
	}
	if req.UpstreamTimeout != nil {
		if err := validateSecs(*req.UpstreamTimeout); err != nil {
			return err
		}
	}
	if req.SyncInterval != nil {
		return validateSecs(*req.SyncInterval)
	}
	return nil
}

// validateSecs 校验秒级配置取值范围（1–3600）。
func validateSecs(v int) error {
	if v < minConfigSecs || v > maxConfigSecs {
		return fmt.Errorf("取值须在 %d–%d 之间", minConfigSecs, maxConfigSecs)
	}
	return nil
}

// validatePublicURL 校验对外基础 URL：空串合法（未配置，回退请求推断）；
// 非空必须为 http/https 绝对 URL。
func validatePublicURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("对外 URL 须为 http/https 绝对地址或留空")
	}
	return nil
}
