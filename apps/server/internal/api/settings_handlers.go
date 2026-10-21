package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
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
	// FR-40 追加：自动备份与保留策略（运行时可改，保存后由作业下一轮读取生效）。
	BackupAutoInterval  int `json:"backupAutoInterval"`
	BackupKeepLast      int `json:"backupKeepLast"`
	BackupRetentionDays int `json:"backupRetentionDays"`
	// FR-40 追加九：间隔的人类表达与拆解结果，供界面直接回填"数值 + 单位"控件，
	// 避免前端重复实现一遍拆分规则（两处实现迟早不一致）。
	BackupAutoIntervalExpr  string `json:"backupAutoIntervalExpr"`
	BackupAutoIntervalValue int64  `json:"backupAutoIntervalValue"`
	BackupAutoIntervalUnit  string `json:"backupAutoIntervalUnit"`
	// FR-40 追加十二：最近一次备份包的实际大小，供界面估算容量占用。
	//
	// 备份是**全量**包，每份约等于数据目录大小，因此"份数 × 单份大小"就是最坏情况
	// 下的磁盘占用。没有这个值，用户无法判断"保留 7 份"会不会把自己的盘写满——
	// 实测存在 20GB 数据 / 40GB 磁盘的配置，7 份需要 140GB，远超可用空间。
	BackupLatestPackageBytes int64 `json:"backupLatestPackageBytes"`
}

// 自动备份配置的取值上限。不是"业务规则"，而是防手滑：把间隔输成 99999999
// 秒（三年）或份数输成 10 万份，都不会报错但会让配置静默失去意义。
// 有了上限，超范围输入会在保存时被明确拒绝，而不是留下一个看似生效的坏配置。
const (
	maxBackupAutoIntervalSecs = 30 * 24 * 3600 // 30 天
	maxBackupKeepLast         = 1000           // 1000 份
	maxBackupRetentionDays    = 3650           // 10 年
)

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
	// FR-40 追加：自动备份三项（0 分别表示关闭 / 不限制，是合法取值）。
	BackupAutoInterval  *int `json:"backupAutoInterval,omitempty"`
	BackupKeepLast      *int `json:"backupKeepLast,omitempty"`
	BackupRetentionDays *int `json:"backupRetentionDays,omitempty"`
	// FR-40 追加九：间隔的人类表达（如 "30"、"5m"、"2h"、"1d"、"1mo"、"1y"）。
	//
	// 与 BackupAutoInterval（秒）二选一，本字段优先：它就是为了让用户不必心算
	// "每天一次等于多少秒"。两者都给时以本字段为准并在响应里回显换算结果，
	// 避免"我填了 1d 但存进去的是 86400"这种不透明的落差。
	BackupAutoIntervalExpr *string `json:"backupAutoIntervalExpr,omitempty"`
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
	// 间隔拆成"数值 + 单位"：界面据此回填两个控件，不需要自己猜该显示成天还是秒。
	intervalValue, intervalUnit := domain.DurationToParts(int64(h.settingServiceOrZero().BackupAutoIntervalSecs()))
	return SettingsResponse{
		AnonymousAccess:    anonymous,
		PublicURL:          publicURL,
		UpstreamTimeout:    timeoutSecs,
		SyncInterval:       syncSecs,
		AllowedHosts:       allowedHosts,
		OriginTokenEnabled: tokenEnabled,
		OriginTokenHeader:  tokenHeader,
		OriginTokenValue:   tokenValue,
		// 三项备份配置：0 是有意义的取值（关闭 / 不限制），照实返回不做回退，
		// 否则界面无法区分"关闭"与"未配置"，也就无法把开关拨回关闭。
		BackupAutoInterval:       h.settingServiceOrZero().BackupAutoIntervalSecs(),
		BackupKeepLast:           h.settingServiceOrZero().BackupKeepLast(),
		BackupRetentionDays:      h.settingServiceOrZero().BackupRetentionDays(),
		BackupAutoIntervalExpr:   domain.FormatDurationHuman(int64(h.settingServiceOrZero().BackupAutoIntervalSecs())),
		BackupAutoIntervalValue:  intervalValue,
		BackupAutoIntervalUnit:   string(intervalUnit),
		BackupLatestPackageBytes: h.latestBackupPackageBytes(),
	}
}

// settingServiceOrZero 在设置服务缺失时返回零值服务，使快照读取无需层层判空。
func (h *Handlers) settingServiceOrZero() *domain.SettingService {
	if h.settings == nil {
		return &domain.SettingService{}
	}
	return h.settings
}

// validateBackupSettings 校验自动备份三项配置的取值。
//
// 三项均**允许 0**（关闭 / 不限制），这不是"未配置"：0 是界面把开关拨回关闭的方式，
// 必须能存下来。负数与超上限一律拒绝——它们不会报错但会让配置静默失去意义
// （如间隔填成负数等于永久关闭，天数填成 10 万等于永不淘汰）。
//
// 间隔若以表达式（backupAutoIntervalExpr）提交，则只校验表达式本身、跳过秒字段的
// 上限校验——两者语义重叠，对同一个值报两次错只会让用户困惑。
func validateBackupSettings(req SettingsRequest) error {
	if req.BackupAutoIntervalExpr != nil {
		if _, err := domain.ParseDurationExpression(*req.BackupAutoIntervalExpr, maxBackupAutoIntervalSecs); err != nil {
			return fmt.Errorf("自动备份间隔：%w", err)
		}
	} else if req.BackupAutoInterval != nil {
		if *req.BackupAutoInterval < 0 {
			return fmt.Errorf("自动备份间隔不能为负数（0 表示关闭）")
		}
		if *req.BackupAutoInterval > maxBackupAutoIntervalSecs {
			return fmt.Errorf("自动备份间隔过大（上限 %s）",
				domain.FormatDurationHuman(int64(maxBackupAutoIntervalSecs)))
		}
	}
	checks := []struct {
		name string
		val  *int
		max  int
		unit string
	}{
		{"保留份数", req.BackupKeepLast, maxBackupKeepLast, "份"},
		{"保留天数", req.BackupRetentionDays, maxBackupRetentionDays, "天"},
	}
	for _, c := range checks {
		if c.val == nil {
			continue
		}
		if *c.val < 0 {
			return fmt.Errorf("%s不能为负数（0 表示关闭 / 不限制）", c.name)
		}
		if *c.val > c.max {
			return fmt.Errorf("%s过大（上限 %d %s）", c.name, c.max, c.unit)
		}
	}
	return nil
}

// resolveBackupAutoInterval 决定本次请求最终要写入的秒数（nil = 本次不改这一项）。
//
// 优先级：表达式 > 秒字段。表达式是给人用的入口，秒字段只为兼容既有调用方保留。
func resolveBackupAutoInterval(req SettingsRequest) (*int, error) {
	if req.BackupAutoIntervalExpr != nil {
		secs, err := domain.ParseDurationExpression(*req.BackupAutoIntervalExpr, maxBackupAutoIntervalSecs)
		if err != nil {
			return nil, err
		}
		v := int(secs)
		return &v, nil
	}
	return req.BackupAutoInterval, nil
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
	// 域名白名单先归一化再校验：容忍粘贴完整 URL（剥离协议/路径/端口），
	// 避免 https://repo.example.com 这类输入被误判为「含非法字符」。
	if req.AllowedHosts != nil {
		hosts := normalizeHostList(*req.AllowedHosts)
		req.AllowedHosts = &hosts
	}
	// 回源 Token 校验需要当前生效值兜底：请求未携带 header/value 时沿用存量配置，
	// 否则"未开启 Token 的实例保存任意字段"会被空头名误判（header 默认空）。
	curEnabled, curHeader, curValue := h.settings.OriginTokenGuard()
	if err := validateSettingsRequest(req, curEnabled, curHeader, curValue); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := validateBackupSettings(req); err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// 表达式优先：把 "5m"/"1d"/"1mo" 换算成秒后写入，存储口径不变。
	intervalSecs, err := resolveBackupAutoInterval(req)
	if err != nil {
		auth.WriteError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := h.settings.UpdateSettings(domain.SettingsUpdate{
		AnonymousAccess:     req.AnonymousAccess,
		PublicURL:           req.PublicURL,
		UpstreamTimeout:     req.UpstreamTimeout,
		SyncInterval:        req.SyncInterval,
		AllowedHosts:        req.AllowedHosts,
		OriginTokenEnabled:  req.OriginTokenEnabled,
		OriginTokenHeader:   req.OriginTokenHeader,
		OriginTokenValue:    req.OriginTokenValue,
		BackupAutoInterval:  intervalSecs,
		BackupKeepLast:      req.BackupKeepLast,
		BackupRetentionDays: req.BackupRetentionDays,
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

// normalizeHostList 归一化域名白名单（保留条目数量，空项交由校验阶段报错）。
func normalizeHostList(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, normalizeHostEntry(item))
	}
	return out
}

// normalizeHostEntry 归一化单条白名单：支持粘贴 https://repo.example.com/path 这类完整地址，
// 只保留主机名（域名或 IP）并统一小写。端口一并剥离——请求侧 hostAllowed 比较的是去掉
// 端口后的 Host，存了端口的条目永远命中不了，剥离后「填什么就是什么」。
func normalizeHostEntry(item string) string {
	host := strings.TrimSpace(item)
	if host == "" {
		return ""
	}
	if idx := strings.Index(host, "://"); idx >= 0 {
		// 带协议：只取 authority 段（容忍 user@ 前缀与路径/查询）。
		rest := host[idx+3:]
		if cut := strings.IndexAny(rest, "/?#"); cut >= 0 {
			rest = rest[:cut]
		}
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			// 只有协议没有主机名（如 "https://"）：归一化为空项交由校验阶段报错。
			return ""
		}
		host = rest
	} else if cut := strings.IndexAny(host, "/?#"); cut >= 0 {
		host = host[:cut]
	}
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port != "" {
			host = h
		}
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return strings.ToLower(host)
}

// validateHostList 校验允许访问的域名白名单：空列表合法（不限制）；
// 每项须为域名或 IP 字面量（协议前缀 / 路径 / 端口已在归一化阶段剥离）。
func validateHostList(hosts []string) error {
	for _, item := range hosts {
		host := strings.TrimSpace(item)
		if host == "" {
			return errors.New("允许访问域名不能为空项（只允许域名或 IP）")
		}
		if _, err := netip.ParseAddr(host); err == nil {
			continue
		}
		if err := validateHostname(host); err != nil {
			return err
		}
	}
	return nil
}

// validateHostname 校验域名字面量：只允许字母数字与 - _ .；
// 不得为空标签、不得以点或连字符开头结尾，纯数字加点（写错的 IP）也拒绝。
func validateHostname(host string) error {
	if strings.ContainsAny(host, " :/\\\t?#@[]%") {
		return fmt.Errorf("允许访问域名 %q 含非法字符（只允许域名或 IP）", host)
	}
	if strings.Contains(host, "..") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return fmt.Errorf("允许访问域名 %q 非法", host)
	}
	if strings.Trim(host, "0123456789.") == "" {
		return fmt.Errorf("允许访问域名 %q 不是合法 IP", host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("允许访问域名 %q 非法", host)
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

// latestBackupPackageBytes 返回最近一次**成功**备份包的实际大小（字节）。
//
// 用途是让界面估算"保留 N 份大约占多少磁盘"。取"最近一次"而不是平均值：
// 数据量通常是增长的，用旧的平均值会低估当前占用；而最近一份最接近现状。
//
// 任何读不到的情况都返回 0（界面据此不显示估算），不让它影响设置读取本身。
func (h *Handlers) latestBackupPackageBytes() int64 {
	if h.backups == nil {
		return 0
	}
	items, err := h.backups.List(20, 0)
	if err != nil {
		return 0
	}
	for _, it := range items {
		// 只认已完成的包：生成中的包体积不完整，会严重低估。
		if it.Status == repository.BackupStatusDone && it.SizeBytes > 0 {
			return it.SizeBytes
		}
	}
	return 0
}
