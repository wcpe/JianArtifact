package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

// SettingsResponse 是设置端点（FR-89）的响应体：基础配置的当前生效值。
type SettingsResponse struct {
	AnonymousAccess    bool     `json:"anonymousAccess"`
	PublicURL          string   `json:"publicUrl"`
	UpstreamTimeout    int      `json:"upstreamTimeout"`
	SyncInterval       int      `json:"syncInterval"`
	AllowedHosts       []string `json:"allowedHosts"`
	OriginTokenEnabled bool     `json:"originTokenEnabled"`
	OriginTokenHeader  string   `json:"originTokenHeader"`
	OriginTokenValue   string   `json:"originTokenValue"`
}

// SettingsRequest 是设置端点的请求体：可选字段，传哪个改哪个（对齐 cluster 端点风格）。
type SettingsRequest struct {
	AnonymousAccess    *bool     `json:"anonymousAccess,omitempty"`
	PublicURL          *string   `json:"publicUrl,omitempty"`
	UpstreamTimeout    *int      `json:"upstreamTimeout,omitempty"`
	SyncInterval       *int      `json:"syncInterval,omitempty"`
	AllowedHosts       *[]string `json:"allowedHosts,omitempty"`
	OriginTokenEnabled *bool     `json:"originTokenEnabled,omitempty"`
	OriginTokenHeader  *string   `json:"originTokenHeader,omitempty"`
	OriginTokenValue   *string   `json:"originTokenValue,omitempty"`
}

// settingsSnapshot 读取基础配置的当前生效值（未配置的间隔 / 超时回退默认值）。
func (h *Handlers) settingsSnapshot() SettingsResponse {
	anonymous := true
	publicURL := ""
	syncSecs := defaultSyncIntervalSecs
	timeoutSecs := defaultUpstreamTimeout
	var allowedHosts []string
	var tokenEnabled bool
	var tokenHeader, tokenValue string
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
		allowedHosts = h.settings.AllowedHostsList()
		tokenEnabled, tokenHeader, tokenValue = h.settings.OriginTokenGuard()
	}
	return SettingsResponse{
		AnonymousAccess:    anonymous,
		PublicURL:          publicURL,
		UpstreamTimeout:    timeoutSecs,
		SyncInterval:       syncSecs,
		AllowedHosts:       allowedHosts,
		OriginTokenEnabled: tokenEnabled,
		OriginTokenHeader:  tokenHeader,
		OriginTokenValue:   tokenValue,
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
	// 回源 Token 校验需要当前生效值兜底：请求未携带 header/value 时沿用存量配置，
	// 否则"未开启 Token 的实例保存任意字段"会被空头名误判（header 默认空）。
	curEnabled, curHeader, curValue := h.settings.OriginTokenGuard()
	if err := validateSettingsRequest(req, curEnabled, curHeader, curValue); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := h.settings.UpdateSettings(domain.SettingsUpdate{
		AnonymousAccess:    req.AnonymousAccess,
		PublicURL:          req.PublicURL,
		UpstreamTimeout:    req.UpstreamTimeout,
		SyncInterval:       req.SyncInterval,
		AllowedHosts:       req.AllowedHosts,
		OriginTokenEnabled: req.OriginTokenEnabled,
		OriginTokenHeader:  req.OriginTokenHeader,
		OriginTokenValue:   req.OriginTokenValue,
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
// 回源 Token 按"请求应用后的生效状态"校验：开启时头名与值必须齐备，关闭时允许
// 留空（清除）；但显式给出的非空值仍须格式合法。cur* 为当前生效值，用于补齐
// 请求未携带的字段（可选字段语义：传哪个改哪个）。
func validateSettingsRequest(req SettingsRequest, curEnabled bool, curHeader, curValue string) error {
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
		if err := validateSecs(*req.SyncInterval); err != nil {
			return err
		}
	}
	if req.AllowedHosts != nil {
		if err := validateHostList(*req.AllowedHosts); err != nil {
			return err
		}
	}

	enabled := curEnabled
	if req.OriginTokenEnabled != nil {
		enabled = *req.OriginTokenEnabled
	}
	header := curHeader
	if req.OriginTokenHeader != nil {
		header = *req.OriginTokenHeader
		if trimmed := strings.TrimSpace(header); trimmed != "" {
			if err := validateHTTPHeaderName(header); err != nil {
				return err
			}
		}
	}
	value := curValue
	if req.OriginTokenValue != nil {
		value = *req.OriginTokenValue
		if v := strings.TrimSpace(value); v != "" && len(v) < 16 {
			return errors.New("回源 Token 值至少 16 个字符")
		}
	}
	if enabled {
		if strings.TrimSpace(header) == "" {
			return errors.New("开启回源 Token 校验必须提供请求头名")
		}
		if strings.TrimSpace(value) == "" {
			return errors.New("开启回源 Token 校验必须提供 Token 值")
		}
	}
	return nil
}

// validateHTTPHeaderName 校验合法的 HTTP 头名（token 除外）：不含空白、冒号、控制字符。
func validateHTTPHeaderName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("请求头名不能为空")
	}
	if strings.ContainsAny(name, " :\t\r\n") {
		return fmt.Errorf("请求头名 %q 含非法字符（不允许空格/冒号/换行）", name)
	}
	return nil
}

// validateHostList 校验允许访问的域名白名单：空列表合法（不限制）；
// 每项必须是合法 Host（域名或 IP，可带端口），禁止空格、路径或 scheme。
func validateHostList(hosts []string) error {
	for _, item := range hosts {
		host := strings.TrimSpace(item)
		if host == "" {
			return errors.New("允许访问域名不能包含空项")
		}
		if strings.ContainsAny(host, " /\\\t") {
			return fmt.Errorf("允许访问域名 %q 含非法字符", host)
		}
		if u, err := url.Parse("http://" + host); err != nil || u.Host == "" || strings.Contains(u.Host, "..") {
			return fmt.Errorf("允许访问域名 %q 非法", host)
		}
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
			return fmt.Errorf("允许访问域名 %q 不应包含协议前缀", host)
		}
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
