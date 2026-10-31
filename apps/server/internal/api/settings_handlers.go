package api

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 设置端点的边界：秒级配置允许范围与缺省展示值。
const (
	minConfigSecs          = 1
	maxConfigSecs          = 3600
	defaultUpstreamTimeout = 30
	// 两项 proxy/group 调优的缺省展示值，必须与 config 层同名默认值同值
	// （config.defaultGroupMemberTimeout / config.defaultNegativeCacheTTL）。
	// 正常路径下启动播种已把初值写进 setting 表，这两个常量只兜「播种失败」与
	// 「从旧版本升级上来但尚未播种」两种情况，让界面不至于显示 0 秒/关闭这种假象。
	defaultGroupMemberTimeoutSecs = 3
	defaultNegativeCacheTTLSecs   = 60
	// 运行期策略 13 项的缺省展示值，逐项必须与 config 层同名默认值同值
	// （config.defaultBlobGCInterval 等，见 internal/config/config.go 的默认值块）。
	// 与上面两项同理：正常路径下启动播种已把初值写进 setting 表，这些常量只兜
	// 「播种失败」与「从旧版本升级上来但尚未播种」两种情况，让界面不至于把
	// 「每天一次」显示成「关闭」这种假象。
	defaultBlobGCIntervalSecs              = 86400   // 24 小时
	defaultStorageCleanupIntervalSecs      = 86400   // 24 小时
	defaultBackupOrphanCleanupIntervalSecs = 86400   // 24 小时
	defaultTrashGCIntervalSecs             = 86400   // 24 小时
	defaultUpstreamProbeIntervalSecs       = 60      // 60 秒（与分钟桶对齐）
	defaultBackupOrphanGraceHours          = 1       // 1 小时
	defaultStorageMetadataRetentionDays    = 7       // 7 天
	defaultStorageTempMaxAgeHours          = 24      // 24 小时
	defaultTrashRetentionDays              = 7       // 7 天
	defaultTrashMaxBytes                   = 2 << 30 // 2 GiB
	defaultAutoBlockBaseSecs               = 40      // 40 秒
	defaultSearchTimeoutSecs               = 10      // 10 秒
	defaultCapacityCacheTTLSecs            = 30      // 30 秒
)

// 运行期策略的取值上限。
//
// # 为什么不统一用 maxConfigSecs
//
// 规格原稿要求「上限统一复用 maxConfigSecs（3600 秒）」，但**核实 config.go 后不成立**：
// blob-gc / storage-cleanup / backup-orphan-cleanup / trash-gc 的默认间隔都是 86400 秒
// （24 小时）。把上界定在 3600 秒，界面把 86400 原样回存时就会被夹到 3600——
// 一个原本每天跑一次的清理作业静默变成每小时一次，在 11GB 级实例上这是持续 IO。
// 上限的作用是挡数量级笔误，不能把**默认值本身**挡在外面。
//
// 因此按量纲分组，每组取「已在该文件里存在的同类上限」或同一数量级：
//   - 秒级请求预算（搜索超时 / 断路器退避 / 容量缓存）：沿用 maxConfigSecs=3600，
//     与 group_member_timeout、negative_cache_ttl 完全一致——这些值以秒计的合理范围
//     本来就在一小时以内；
//   - 作业间隔：沿用 maxBackupAutoIntervalSecs（30 天），它已经是本文件对「间隔」这一
//     量纲定的上界（"防手滑"而非业务规则）；
//   - 小时 / 天：按同一数量级取一年（8760 小时 / 365 天），天的上界与既有
//     maxBackupRetentionDays（3650）不必强行对齐——保留期与作业频率是两回事，
//     这里更看重「一年以上基本等于永不清理」这一直觉。
const (
	// maxConfigHours 是小时量纲的上界（一年）。
	maxConfigHours = 8760
	// maxConfigDays 是天数量纲的上界（一年）。
	maxConfigDays = 365
	// maxTrashMaxBytes 是回收站字节上限的上界（1 TiB）。
	// 设上界而不是不设：这个字段以字节输入，多敲三个 0 是现实存在的笔误
	// （2 GiB → 2 TiB），而上限只用来挡这种数量级错误。
	maxTrashMaxBytes = 1 << 40 // 1 TiB
)

// SettingsResponse 是设置端点的响应体：基础配置的当前生效值。
type SettingsResponse struct {
	AnonymousAccess    bool     `json:"anonymousAccess"`
	PublicURL          string   `json:"publicUrl"`
	UpstreamTimeout    int      `json:"upstreamTimeout"`
	AllowedHosts       []string `json:"allowedHosts"`
	OriginTokenEnabled bool     `json:"originTokenEnabled"`
	OriginTokenHeader  string   `json:"originTokenHeader"`
	OriginTokenValue   string   `json:"originTokenValue"`
	// 自动备份与保留策略（运行时可改，保存后由作业下一轮读取生效）。
	BackupAutoInterval  int `json:"backupAutoInterval"`
	BackupKeepLast      int `json:"backupKeepLast"`
	BackupRetentionDays int `json:"backupRetentionDays"`
	// 间隔的人类表达与拆解结果，供界面直接回填"数值 + 单位"控件，
	// 避免前端重复实现一遍拆分规则（两处实现迟早不一致）。
	BackupAutoIntervalExpr  string `json:"backupAutoIntervalExpr"`
	BackupAutoIntervalValue int64  `json:"backupAutoIntervalValue"`
	BackupAutoIntervalUnit  string `json:"backupAutoIntervalUnit"`
	// 最近一次备份包的实际大小，供界面估算容量占用。
	//
	// 备份是**全量**包，每份约等于数据目录大小，因此"份数 × 单份大小"就是最坏情况
	// 下的磁盘占用。没有这个值，用户无法判断"保留 7 份"会不会把自己的盘写满——
	// 实测存在 20GB 数据 / 40GB 磁盘的配置，7 份需要 140GB，远超可用空间。
	BackupLatestPackageBytes int64 `json:"backupLatestPackageBytes"`
	// proxy/group 读路径调优两项的当前生效值（秒）。回填的是**真的在用的值**：
	// 负缓存 TTL 的 0 表示"关闭"，必须照实返回，否则界面无法把开关拨回关闭态。
	GroupMemberTimeout int `json:"groupMemberTimeout"`
	NegativeCacheTTL   int `json:"negativeCacheTTL"`
	// 运行期策略 13 项（分组与设置页「运行期策略」分区一致）。
	//
	// 回填的是**真的在用的值**：0 在这些项上都有明确语义（作业禁用 / 不自动清理 /
	// 不限制 / 禁用缓存），必须照实返回，否则界面无法把开关拨回关闭态。
	//
	// 生效时机（界面文案必须如实标注，不能一律写"立即生效"）：
	//   - 5 个作业间隔：保存后经调度器 SetInterval 热改，**从下一轮作业起生效**；
	//   - 4 个保留策略（宽限期 / 元数据保留 / 临时文件存活 / 回收站保留）：
	//     由作业在**每轮开始时**读取，下一轮作业生效；
	//   - 回收站上限：下一次 cleanup / trash-gc 的前置检查时生效；
	//   - 搜索预算 / 断路器退避 / 容量缓存 TTL：请求路径按需读取，**下一次请求即生效**。
	BlobGCInterval               int   `json:"blobGcInterval"`
	StorageCleanupInterval       int   `json:"storageCleanupInterval"`
	BackupOrphanCleanupInterval  int   `json:"backupOrphanCleanupInterval"`
	TrashGCInterval              int   `json:"trashGcInterval"`
	UpstreamProbeInterval        int   `json:"upstreamProbeInterval"`
	BackupOrphanGraceHours       int   `json:"backupOrphanGraceHours"`
	StorageMetadataRetentionDays int   `json:"storageMetadataRetentionDays"`
	StorageTempMaxAgeHours       int   `json:"storageTempMaxAgeHours"`
	TrashRetentionDays           int   `json:"trashRetentionDays"`
	TrashMaxBytes                int64 `json:"trashMaxBytes"`
	AutoBlockBaseSeconds         int   `json:"autoBlockBaseSeconds"`
	SearchTimeoutSeconds         int   `json:"searchTimeoutSeconds"`
	CapacityCacheTTLSeconds      int   `json:"capacityCacheTtlSeconds"`
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
	AllowedHosts       *[]string `json:"allowedHosts,omitempty"`
	OriginTokenEnabled *bool     `json:"originTokenEnabled,omitempty"`
	OriginTokenHeader  *string   `json:"originTokenHeader,omitempty"`
	OriginTokenValue   *string   `json:"originTokenValue,omitempty"`
	// 自动备份三项（0 分别表示关闭 / 不限制，是合法取值）。
	BackupAutoInterval  *int `json:"backupAutoInterval,omitempty"`
	BackupKeepLast      *int `json:"backupKeepLast,omitempty"`
	BackupRetentionDays *int `json:"backupRetentionDays,omitempty"`
	// 间隔的人类表达（如 "30"、"5m"、"2h"、"1d"、"1mo"、"1y"）。
	//
	// 与 BackupAutoInterval（秒）二选一，本字段优先：它就是为了让用户不必心算
	// "每天一次等于多少秒"。两者都给时以本字段为准并在响应里回显换算结果，
	// 避免"我填了 1d 但存进去的是 86400"这种不透明的落差。
	BackupAutoIntervalExpr *string `json:"backupAutoIntervalExpr,omitempty"`
	// proxy/group 读路径调优两项（秒）。负缓存 TTL 的 0 是"关闭"的合法取值。
	GroupMemberTimeout *int `json:"groupMemberTimeout,omitempty"`
	NegativeCacheTTL   *int `json:"negativeCacheTTL,omitempty"`
	// 运行期策略 13 项。用指针是为了区分「本次未提供」与「显式传 0」——
	// 其中 10 项的 0 是合法且有意义的取值（作业禁用 / 不自动清理 / 不限制 / 禁用缓存），
	// 若用值类型，界面就永远无法把开关拨回关闭。
	BlobGCInterval               *int `json:"blobGcInterval,omitempty"`
	StorageCleanupInterval       *int `json:"storageCleanupInterval,omitempty"`
	BackupOrphanCleanupInterval  *int `json:"backupOrphanCleanupInterval,omitempty"`
	TrashGCInterval              *int `json:"trashGcInterval,omitempty"`
	UpstreamProbeInterval        *int `json:"upstreamProbeInterval,omitempty"`
	BackupOrphanGraceHours       *int `json:"backupOrphanGraceHours,omitempty"`
	StorageMetadataRetentionDays *int `json:"storageMetadataRetentionDays,omitempty"`
	StorageTempMaxAgeHours       *int `json:"storageTempMaxAgeHours,omitempty"`
	TrashRetentionDays           *int `json:"trashRetentionDays,omitempty"`
	// 回收站上限以**字节**为单位，故是 int64：2 GiB 已经超出 32 位 int，
	// 而字节数是最容易被写成 10 位以上数字的一个字段。
	TrashMaxBytes           *int64 `json:"trashMaxBytes,omitempty"`
	AutoBlockBaseSeconds    *int   `json:"autoBlockBaseSeconds,omitempty"`
	SearchTimeoutSeconds    *int   `json:"searchTimeoutSeconds,omitempty"`
	CapacityCacheTTLSeconds *int   `json:"capacityCacheTtlSeconds,omitempty"`
}

// settingsSnapshot 读取基础配置的当前生效值（未配置的间隔 / 超时回退默认值）。
func (h *Handlers) settingsSnapshot() SettingsResponse {
	anonymous := true
	publicURL := ""
	timeoutSecs := defaultUpstreamTimeout
	groupMemberSecs := defaultGroupMemberTimeoutSecs
	negCacheTTLSecs := defaultNegativeCacheTTLSecs
	var allowedHosts []string
	var tokenEnabled bool
	var tokenHeader, tokenValue string
	if h.settings != nil {
		if v, err := h.settings.AnonymousAccessEnabled(); err == nil {
			anonymous = v
		}
		publicURL = h.settings.PublicURL()
		if v := h.settings.UpstreamTimeoutSecs(); v > 0 {
			timeoutSecs = v
		}
		// 成员确认预算只在 >0 时采用（本项无禁用语义，0/缺失回退默认）。
		if v := h.settings.GroupMemberTimeoutSecs(); v > 0 {
			groupMemberSecs = v
		}
		// 负缓存 TTL 必须用 (secs, ok) 判定：ok=false 才是"未配置/值非法"，
		// 而 ok=true 且 secs==0 是用户显式关闭，界面要照实显示 0。
		if v, ok := h.settings.NegativeCacheTTLSecs(); ok {
			negCacheTTLSecs = v
		}
		allowedHosts = h.settings.AllowedHostsList()
		tokenEnabled, tokenHeader, tokenValue = h.settings.OriginTokenGuard()
	}
	// 间隔拆成"数值 + 单位"：界面据此回填两个控件，不需要自己猜该显示成天还是秒。
	intervalValue, intervalUnit := domain.DurationToParts(int64(h.settingServiceOrZero().BackupAutoIntervalSecs()))
	// 运行期策略 13 项的统一读取入口；nil 安全的零值服务，避免逐项判空。
	svc := h.settingServiceOrZero()
	return SettingsResponse{
		AnonymousAccess:    anonymous,
		PublicURL:          publicURL,
		UpstreamTimeout:    timeoutSecs,
		AllowedHosts:       allowedHosts,
		OriginTokenEnabled: tokenEnabled,
		OriginTokenHeader:  tokenHeader,
		OriginTokenValue:   tokenValue,
		// 三项备份配置：0 是有意义的取值（关闭 / 不限制），照实返回不做回退，
		// 否则界面无法区分"关闭"与"未配置"，也就无法把开关拨回关闭。
		BackupAutoInterval:          h.settingServiceOrZero().BackupAutoIntervalSecs(),
		BackupKeepLast:              h.settingServiceOrZero().BackupKeepLast(),
		BackupRetentionDays:         h.settingServiceOrZero().BackupRetentionDays(),
		BackupAutoIntervalExpr:      domain.FormatDurationHuman(int64(h.settingServiceOrZero().BackupAutoIntervalSecs())),
		BackupAutoIntervalValue:     intervalValue,
		BackupAutoIntervalUnit:      string(intervalUnit),
		BackupLatestPackageBytes:    h.latestBackupPackageBytes(),
		GroupMemberTimeout:          groupMemberSecs,
		NegativeCacheTTL:            negCacheTTLSecs,
		BlobGCInterval:              policyIntOr(svc.BlobGCIntervalSecs, defaultBlobGCIntervalSecs),
		StorageCleanupInterval:      policyIntOr(svc.StorageCleanupIntervalSecs, defaultStorageCleanupIntervalSecs),
		BackupOrphanCleanupInterval: policyIntOr(svc.BackupOrphanCleanupIntervalSecs, defaultBackupOrphanCleanupIntervalSecs),
		TrashGCInterval:             policyIntOr(svc.TrashGCIntervalSecs, defaultTrashGCIntervalSecs),
		UpstreamProbeInterval:       policyIntOr(svc.UpstreamProbeIntervalSecs, defaultUpstreamProbeIntervalSecs),
		// 宽限期不允许 0：读取函数用普通 int（0 = 未配置/非法），只在 > 0 时采用，
		// 否则回填默认。若直接赋值，键缺失时界面会显示「0 小时宽限期」——
		// 那正是最危险的显示（看起来像"零宽限已生效"，实际用的是默认 1 小时）。
		BackupOrphanGraceHours:       positiveIntOr(svc.BackupOrphanGraceHours(), defaultBackupOrphanGraceHours),
		StorageMetadataRetentionDays: policyIntOr(svc.StorageMetadataRetentionDays, defaultStorageMetadataRetentionDays),
		StorageTempMaxAgeHours:       policyIntOr(svc.StorageTempMaxAgeHours, defaultStorageTempMaxAgeHours),
		TrashRetentionDays:           policyIntOr(svc.TrashRetentionDays, defaultTrashRetentionDays),
		TrashMaxBytes:                policyInt64Or(svc.TrashMaxBytes, defaultTrashMaxBytes),
		// 断路器退避与搜索预算同属「0 无意义」：只在 > 0 时采用，否则回填默认。
		AutoBlockBaseSeconds:    positiveIntOr(svc.AutoBlockBaseSeconds(), defaultAutoBlockBaseSecs),
		SearchTimeoutSeconds:    positiveIntOr(svc.SearchTimeoutSeconds(), defaultSearchTimeoutSecs),
		CapacityCacheTTLSeconds: policyIntOr(svc.CapacityCacheTTLSeconds, defaultCapacityCacheTTLSecs),
	}
}

// positiveIntOr 返回「0 无意义」的项该回显的值：>0 才采用，否则回退默认。
//
// 与 policyIntOr 的分工是那两个口径本身的分工：本函数用于禁用语义缺失的项
// （0 小时宽限 = 误删在写的包；0 秒搜索预算 = 放开无界查询），
// 这些值显示成 0 会让运维以为某个危险状态已经生效，而实际用的是默认值。
func positiveIntOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// policyIntOr 返回「0 有语义」的项该回显的值：只有 ok=false（键缺失或值非法）才回退默认值。
//
// 反过来做的后果是具体的：0 = 禁用作业 / 不自动清理 / 禁用缓存，若把 0 当成
// 「未配置」而回显默认值，界面会显示成「每天一次」，运维看得见的值与真实行为不一致。
func policyIntOr(get func() (int, bool), def int) int {
	if v, ok := get(); ok {
		return v
	}
	return def
}

// policyInt64Or 是 policyIntOr 的 int64 版本（回收站上限以字节计）。
func policyInt64Or(get func() (int64, bool), def int64) int64 {
	if v, ok := get(); ok {
		return v
	}
	return def
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
// looksLikeDisplayedInterval 判断该值是否是「回显格式」而不是「用户输入」。
//
// 为什么需要这个判断：GET 响应里的 backupAutoIntervalExpr 是**给人看的中文串**
// （如 "30 秒"、"2 天"、"关闭"），而 PUT 的同名字段是**输入**。二者共用字段名，
// 于是「把 GET 结果原样 PUT 回去」这种最自然的读-改-写用法必然 400
// （未知的时间单位后缀 "秒"）——脚本化/第三方客户端一定会踩到。
//
// 判据刻意收窄，只认 FormatDurationHuman 会产出的那几种形态，不猜其他：
//   - "关闭"（0 秒的回显）
//   - "<数字> <中文单位>"（秒/分钟/小时/天/周/个月/年）
//
// 命中即视为「未提供本字段」，交由秒字段决定；这样既修好读-改-写，又不改变
// 任何合法英文表达式（"30"、"5m"、"1d"）的解析。
func looksLikeDisplayedInterval(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if s == "关闭" {
		return true
	}
	for _, unit := range []string{"秒钟", "秒", "分钟", "小时", "天", "周", "个月", "年"} {
		if strings.HasSuffix(s, unit) {
			// 数字部分必须能解析，避免把 "abc天" 这种真正的错误输入也吞掉。
			numPart := strings.TrimSpace(strings.TrimSuffix(s, unit))
			if numPart == "" {
				return false
			}
			if _, err := strconv.ParseInt(numPart, 10, 64); err != nil {
				return false
			}
			return true
		}
	}
	return false
}

// effectiveIntervalExpr 返回真正该当作输入处理的表达式（回显值视为未提供）。
func effectiveIntervalExpr(req SettingsRequest) *string {
	if req.BackupAutoIntervalExpr == nil {
		return nil
	}
	if looksLikeDisplayedInterval(*req.BackupAutoIntervalExpr) {
		return nil
	}
	return req.BackupAutoIntervalExpr
}

func validateBackupSettings(req SettingsRequest) error {
	if expr := effectiveIntervalExpr(req); expr != nil {
		if _, err := domain.ParseDurationExpression(*expr, maxBackupAutoIntervalSecs); err != nil {
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
	if expr := effectiveIntervalExpr(req); expr != nil {
		secs, err := domain.ParseDurationExpression(*expr, maxBackupAutoIntervalSecs)
		if err != nil {
			return nil, err
		}
		v := int(secs)
		return &v, nil
	}
	return req.BackupAutoInterval, nil
}

// clampGroupMemberTimeoutSecs 把 group 成员确认阶段预算夹到设置页的合法区间（1–3600 秒）。
//
// 为什么是"夹取 + 警告"而不是像 validateSecs 那样直接拒绝 400：本项的口径由 config 层
// 定调（JIAN_GROUP_MEMBER_TIMEOUT 超上限就是夹取并打中文警告，见 config.groupMemberTimeout），
// 而设置页只是同一配置的第二个入口。两个入口对同一个输入给出不同结论（环境变量接受并夹取、
// 界面报 400）会让"到底哪个才是真源"变成一次无谓的争论。
//
// 本项**无禁用语义**：0 秒预算等于每个成员都立刻超时，group 读会 100% 退化成不可判定失败，
// 只因一处手误就会让整个 group 仓库不可用，因此 0 与负数一律回退默认 3 秒并留痕——
// 口径同 config 层的 envPositiveDurationOr。
func clampGroupMemberTimeoutSecs(v int) int {
	if v <= 0 {
		log.Printf("警告：group 成员确认预算 %d 秒无效（0 秒等于每个成员都立刻超时，本项无关闭语义），已回退默认 %d 秒。",
			v, defaultGroupMemberTimeoutSecs)
		return defaultGroupMemberTimeoutSecs
	}
	if v > maxConfigSecs {
		log.Printf("警告：group 成员确认预算 %d 秒超过上限 %d 秒，已按上限 %d 秒使用（调大会直接拖慢缺失路径的失败返回）。",
			v, maxConfigSecs, maxConfigSecs)
		return maxConfigSecs
	}
	return v
}

// clampNegativeCacheTTLSecs 把负缓存 TTL 夹到 0–3600 秒。
//
// 与上一项的差别是这里**0 是合法且有意义的值**（关闭负缓存，每个请求都真实回源），
// 必须原样保留，不能被当成"未配置"而回退默认——那正是界面关不掉负缓存的经典缺陷。
//
// 负数不解读为关闭：关闭应当是明确的运维意图，用显式的 0 表达。把 -600 这类笔误当成
// 关闭，等于一次手误就静默放大上游压力（每个缺失路径都要探测全部 group 成员），
// 恰好落在最不该静默的地方；因此负数回退默认 60 秒并留痕，口径同 config.negativeCacheTTL。
func clampNegativeCacheTTLSecs(v int) int {
	if v == 0 {
		return 0
	}
	if v < 0 {
		log.Printf("警告：负缓存 TTL %d 秒无效（负数不等于关闭，关闭请显式填 0），已回退默认 %d 秒。",
			v, defaultNegativeCacheTTLSecs)
		return defaultNegativeCacheTTLSecs
	}
	if v > maxConfigSecs {
		log.Printf("警告：负缓存 TTL %d 秒超过上限 %d 秒，已按上限 %d 秒使用（负缓存过久会让新发布的制品长期 404 且不自愈）。",
			v, maxConfigSecs, maxConfigSecs)
		return maxConfigSecs
	}
	return v
}

// policyClamp 描述一项运行期策略的取值口径，把「0 是否合法」「回落默认是多少」
// 「上限是多少」集中表达，避免为 13 个字段各写一份几乎雷同的夹取函数
// （13 份雷同实现里迟早有一份把「0 合法」写成「0 回退」——而那正是本批要消灭的缺陷）。
//
// 它不是第三种校验范式：zeroAllowed=true 时行为与 clampNegativeCacheTTLSecs 逐字一致
// （0 原样保留、负数回落默认、超上限夹取），false 时与 clampGroupMemberTimeoutSecs 一致
// （非正数回落默认、超上限夹取）。
type policyClamp struct {
	// name 是中文字段名，只用于警告文案。
	name string
	// unit 是量纲单位（秒 / 小时 / 天 / 字节），只用于警告文案。
	unit string
	// def 是回落默认值，必须与 config 层同名默认值同值。
	def int64
	// max 是上限，超限夹取到本值。
	max int64
	// zeroAllowed 报告 0 是否是该字段的合法取值（禁用 / 不限制 / 不自动清理）。
	zeroAllowed bool
	// zeroMeaning 说明 0 的含义，写进负数回落时的警告——运维看到「-600 已回退」时
	// 必须同时知道「想关闭应该填 0」，否则下一次他还会填负数。
	zeroMeaning string
	// overLimitHint 说明「这个值调太大会有什么后果」。夹取必须留痕，而留痕要知道后果。
	overLimitHint string
}

// clampPolicy 按 policyClamp 的口径夹取一个取值，并在改动时打中文警告。
//
// 为什么是「夹取 + 警告」而不是像 validateSecs 那样直接 400：这些项的取值口径由 config 层
// 定调（环境变量超上限就是夹取并警告），而设置页只是同一配置的第二个入口。两个入口对同一
// 输入给出不同结论，会让「到底哪个才是真源」变成一次无谓的争论；且配置瑕疵不该让保存失败，
// 但必须留痕，否则运维会以为改生效了。
func clampPolicy(p policyClamp, v int64) int64 {
	if v < 0 {
		log.Printf("警告：%s %d %s无效（%s），已回退默认 %d %s。",
			p.name, v, p.unit, p.zeroMeaning, p.def, p.unit)
		return p.def
	}
	if v == 0 {
		if p.zeroAllowed {
			return 0
		}
		log.Printf("警告：%s不能为 0（%s），已回退默认 %d %s。",
			p.name, p.zeroMeaning, p.def, p.unit)
		return p.def
	}
	if v > p.max {
		log.Printf("警告：%s %d %s超过上限 %d %s，已按上限使用（%s）。",
			p.name, v, p.unit, p.max, p.unit, p.overLimitHint)
		return p.max
	}
	return v
}

// 13 项运行期策略的夹取口径。逐条在这里声明，而不是散落在 PutSettings 里：
// 一项配置的「0 能不能填、填多大算过头」应当能一眼对照着看。
var (
	blobGCIntervalClamp = policyClamp{
		name: "blob 回收间隔", unit: "秒", def: defaultBlobGCIntervalSecs, max: maxBackupAutoIntervalSecs,
		zeroAllowed: true, zeroMeaning: "0 表示禁用该作业",
		overLimitHint: "间隔过小会让磁盘扫描持续占用 IO",
	}
	storageCleanupIntervalClamp = policyClamp{
		name: "存储治理清理间隔", unit: "秒", def: defaultStorageCleanupIntervalSecs, max: maxBackupAutoIntervalSecs,
		zeroAllowed: true, zeroMeaning: "0 表示禁用该作业",
		overLimitHint: "间隔过小会让目录遍历持续占用 IO",
	}
	backupOrphanCleanupIntervalClamp = policyClamp{
		name: "孤儿备份包回收间隔", unit: "秒", def: defaultBackupOrphanCleanupIntervalSecs, max: maxBackupAutoIntervalSecs,
		zeroAllowed: true, zeroMeaning: "0 表示禁用该作业",
		overLimitHint: "间隔过小会让备份目录被反复全量扫描",
	}
	trashGCIntervalClamp = policyClamp{
		name: "回收站回收间隔", unit: "秒", def: defaultTrashGCIntervalSecs, max: maxBackupAutoIntervalSecs,
		zeroAllowed: true, zeroMeaning: "0 表示禁用该作业",
		overLimitHint: "间隔过小会让回收站被反复遍历",
	}
	upstreamProbeIntervalClamp = policyClamp{
		name: "上游健康探测间隔", unit: "秒", def: defaultUpstreamProbeIntervalSecs, max: maxBackupAutoIntervalSecs,
		zeroAllowed: true, zeroMeaning: "0 表示关闭定时探测（真实请求采样照常积累）",
		overLimitHint: "每次探测都会对本节点每个 proxy 上游发一次 HEAD，间隔过小等于持续压上游",
	}
	// 宽限期是唯一**不允许 0** 的一项：零宽限会让「刚拷进 backups/、还没在界面导入」
	// 的包在下一轮就被判为孤儿删掉，而这正是宽限期存在的唯一目的。
	backupOrphanGraceClamp = policyClamp{
		name: "孤儿包回收宽限期", unit: "小时", def: defaultBackupOrphanGraceHours, max: maxConfigHours,
		zeroAllowed:   false,
		zeroMeaning:   "零宽限会把正在拷入、尚未导入的备份包误删",
		overLimitHint: "宽限期过长只会让孤儿包多留一会儿，不影响正确性",
	}
	storageMetadataRetentionClamp = policyClamp{
		name: "元数据保留天数", unit: "天", def: defaultStorageMetadataRetentionDays, max: maxConfigDays,
		zeroAllowed: true, zeroMeaning: "0 表示不裁剪终态元数据",
		overLimitHint: "保留过久会让元数据表持续膨胀",
	}
	storageTempMaxAgeClamp = policyClamp{
		name: "临时文件最长存活", unit: "小时", def: defaultStorageTempMaxAgeHours, max: maxConfigHours,
		zeroAllowed: true, zeroMeaning: "0 表示不清理过期上传临时文件",
		overLimitHint: "过长会让中断的上传长期占盘",
	}
	trashRetentionClamp = policyClamp{
		name: "回收站保留天数", unit: "天", def: defaultTrashRetentionDays, max: maxConfigDays,
		zeroAllowed: true, zeroMeaning: "0 表示不自动清理（仍可手动彻底删除）",
		overLimitHint: "保留过久会让回收站持续占盘",
	}
	trashMaxBytesClamp = policyClamp{
		name: "回收站容量上限", unit: "字节", def: defaultTrashMaxBytes, max: maxTrashMaxBytes,
		zeroAllowed: true, zeroMeaning: "0 表示不限制",
		overLimitHint: "上限设得比可用磁盘还大等于没有护栏",
	}
	// 断路器退避与搜索预算都**没有禁用语义**：0 窗口等于「失败不阻止」，
	// 0 预算等于放开无界查询，故非正数一律回落默认（口径同 config.envPositiveDurationOr）。
	autoBlockBaseClamp = policyClamp{
		name: "断路器退避起始", unit: "秒", def: defaultAutoBlockBaseSecs, max: maxConfigSecs,
		zeroAllowed:   false,
		zeroMeaning:   "起始退避为 0 等于失败后不做任何阻止",
		overLimitHint: "退避起始过长会让上游短暂抖动被放大成长时间不可用",
	}
	searchTimeoutClamp = policyClamp{
		name: "搜索总预算", unit: "秒", def: defaultSearchTimeoutSecs, max: maxConfigSecs,
		zeroAllowed:   false,
		zeroMeaning:   "关掉搜索超时等于开放一个可长期占住数据库连接的入口",
		overLimitHint: "预算过长会让慢查询长时间占住连接",
	}
	capacityCacheTTLClamp = policyClamp{
		name: "容量缓存有效期", unit: "秒", def: defaultCapacityCacheTTLSecs, max: maxConfigSecs,
		zeroAllowed: true, zeroMeaning: "0 表示禁用缓存（每次请求实时聚合，代价是多跑一次全表扫描）",
		overLimitHint: "有效期过长只会让仪表盘数字滞后（进程重启即自愈）",
	}
)

// clampRuntimePolicy 把 13 项运行期策略的请求值逐项夹到合法区间，返回可直接写库的更新。
//
// 在写入前夹取（而不是存进去再在读取时夹）是刻意的：存进去的就是**实际会生效的值**，
// 避免出现「表里是 99999、运行时被夹到 3600」这种界面与行为对不上的状态
// ——与既有 clampGroupMemberTimeoutSecs / clampNegativeCacheTTLSecs 的处理点一致。
func clampRuntimePolicy(req SettingsRequest) domain.SettingsUpdate {
	clampInt := func(v *int, p policyClamp) *int {
		if v == nil {
			return nil
		}
		n := int(clampPolicy(p, int64(*v)))
		return &n
	}
	update := domain.SettingsUpdate{
		BlobGCInterval:               clampInt(req.BlobGCInterval, blobGCIntervalClamp),
		StorageCleanupInterval:       clampInt(req.StorageCleanupInterval, storageCleanupIntervalClamp),
		BackupOrphanCleanupInterval:  clampInt(req.BackupOrphanCleanupInterval, backupOrphanCleanupIntervalClamp),
		TrashGCInterval:              clampInt(req.TrashGCInterval, trashGCIntervalClamp),
		UpstreamProbeInterval:        clampInt(req.UpstreamProbeInterval, upstreamProbeIntervalClamp),
		BackupOrphanGraceHours:       clampInt(req.BackupOrphanGraceHours, backupOrphanGraceClamp),
		StorageMetadataRetentionDays: clampInt(req.StorageMetadataRetentionDays, storageMetadataRetentionClamp),
		StorageTempMaxAgeHours:       clampInt(req.StorageTempMaxAgeHours, storageTempMaxAgeClamp),
		TrashRetentionDays:           clampInt(req.TrashRetentionDays, trashRetentionClamp),
		AutoBlockBaseSeconds:         clampInt(req.AutoBlockBaseSeconds, autoBlockBaseClamp),
		SearchTimeoutSeconds:         clampInt(req.SearchTimeoutSeconds, searchTimeoutClamp),
		CapacityCacheTTLSeconds:      clampInt(req.CapacityCacheTTLSeconds, capacityCacheTTLClamp),
	}
	if req.TrashMaxBytes != nil {
		n := clampPolicy(trashMaxBytesClamp, *req.TrashMaxBytes)
		update.TrashMaxBytes = &n
	}
	return update
}

// GetSettings 返回基础配置四项，仅管理员。非契约端点，经 WithProtocolRoutes 注册。
func (h *Handlers) GetSettings(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	c.JSON(http.StatusOK, h.settingsSnapshot())
}

// settingKeyParamMaxLen 限制历史查询的设置键长度，避免超长参数打满审计表扫描。
const settingKeyParamMaxLen = 64

// GetSettingHistory 查询某个设置键的变更历史（仅管理员）。
//
// 按设置键精确查询而不是复用审计列表的关键字模糊匹配：设置历史要稳定地回答
// 「这个键都被谁改过」，模糊匹配会把 public_url 与 allowed_hosts 之类的改动混在一起。
func (h *Handlers) GetSettingHistory(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	if h.auditLogs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "审计日志未启用"})
		return
	}
	key := strings.TrimSpace(c.Query("key"))
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 key 参数"})
		return
	}
	if len(key) > settingKeyParamMaxLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key 参数过长"})
		return
	}
	limit := 20
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit 必须为正整数"})
			return
		}
		limit = parsed
	}
	entries, err := h.auditLogs.ListSettingHistory(key, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取设置历史失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"key": key, "entries": entries})
}

// PutSettings 部分写入基础配置，仅管理员。写后运行时生效：
// 匿名开关 / 对外 URL 由读取方动态读 setting；同步间隔由调度器下一轮应用；
// 回源超时经 OnUpstreamTimeoutChange 回调立即同步到回源客户端；
// proxy/group 调优两项经 OnGroupMemberTimeoutChange / OnNegativeCacheTTLChange
// 回调立即同步到 AssetService（成员确认预算与负缓存 TTL），不需重启。
//
// 运行期策略 13 项按生效方式分两条路：
//   - 时长类 8 项（5 个作业间隔 + 搜索预算 + 断路器退避 + 容量缓存 TTL）经
//     OnRuntimePolicyChange 交给装配层，由它决定推给调度器还是对应服务；
//   - 回收站容量上限经 OnTrashCapacityChange 推给定向清理服务的容量护栏。
//
// 剩下 4 个保留策略（宽限期 / 元数据保留 / 临时文件存活 / 回收站保留）**不需要回调**：
// 它们的读取方是作业，而作业在每一轮开始时现读 setting 表，写库即已生效。
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
	// 两项调优在写入前先夹到合法区间：存进去的就是**实际会生效的值**，
	// 避免出现"表里是 99999、运行时被夹到 3600"这种界面与行为对不上的状态。
	groupMemberSecs := req.GroupMemberTimeout
	if groupMemberSecs != nil {
		v := clampGroupMemberTimeoutSecs(*groupMemberSecs)
		groupMemberSecs = &v
	}
	negCacheSecs := req.NegativeCacheTTL
	if negCacheSecs != nil {
		v := clampNegativeCacheTTLSecs(*negCacheSecs)
		negCacheSecs = &v
	}
	// 运行期策略 13 项同口径：写入前夹取，表中存的就是实际会生效的值。
	policy := clampRuntimePolicy(req)
	// 写入前取一份生效快照：审计要记「从什么改成什么」，而 UpdateSettings 之后就
	// 只剩新值了。快照必须取在写入之前，且只用于算前后差异。
	beforeSnapshot := h.settingsSnapshot()
	if err := h.settings.UpdateSettings(domain.SettingsUpdate{
		AnonymousAccess:     req.AnonymousAccess,
		PublicURL:           req.PublicURL,
		UpstreamTimeout:     req.UpstreamTimeout,
		AllowedHosts:        req.AllowedHosts,
		OriginTokenEnabled:  req.OriginTokenEnabled,
		OriginTokenHeader:   req.OriginTokenHeader,
		OriginTokenValue:    req.OriginTokenValue,
		BackupAutoInterval:  intervalSecs,
		BackupKeepLast:      req.BackupKeepLast,
		BackupRetentionDays: req.BackupRetentionDays,
		GroupMemberTimeout:  groupMemberSecs,
		NegativeCacheTTL:    negCacheSecs,

		BlobGCInterval:               policy.BlobGCInterval,
		StorageCleanupInterval:       policy.StorageCleanupInterval,
		BackupOrphanCleanupInterval:  policy.BackupOrphanCleanupInterval,
		TrashGCInterval:              policy.TrashGCInterval,
		UpstreamProbeInterval:        policy.UpstreamProbeInterval,
		BackupOrphanGraceHours:       policy.BackupOrphanGraceHours,
		StorageMetadataRetentionDays: policy.StorageMetadataRetentionDays,
		StorageTempMaxAgeHours:       policy.StorageTempMaxAgeHours,
		TrashRetentionDays:           policy.TrashRetentionDays,
		TrashMaxBytes:                policy.TrashMaxBytes,
		AutoBlockBaseSeconds:         policy.AutoBlockBaseSeconds,
		SearchTimeoutSeconds:         policy.SearchTimeoutSeconds,
		CapacityCacheTTLSeconds:      policy.CapacityCacheTTLSeconds,
	}); err != nil {
		writeDomainErr(c, err)
		return
	}
	if req.UpstreamTimeout != nil && h.onUpstreamTimeoutChange != nil {
		h.onUpstreamTimeoutChange(time.Duration(*req.UpstreamTimeout) * time.Second)
	}
	// 热生效：写入成功后立即把生效值推给持有运行时状态的持有者，不等重启。
	if groupMemberSecs != nil && h.onGroupMemberTimeoutChange != nil {
		h.onGroupMemberTimeoutChange(time.Duration(*groupMemberSecs) * time.Second)
	}
	if negCacheSecs != nil && h.onNegativeCacheTTLChange != nil {
		h.onNegativeCacheTTLChange(time.Duration(*negCacheSecs) * time.Second)
	}
	h.applyRuntimePolicy(policy)
	// 逐键落审计：设置页要展示「这一项谁在什么时候改成了什么」，
	// 只写一条「基础设置更新」摘要无法支撑按 key 查询（历史上也确实查不到）。
	// 只记录本次请求真的提交过、且值确实发生变化的键——未提交的键不该出现在历史里。
	h.auditSettingChanges(c, beforeSnapshot, h.settingsSnapshot())
	c.JSON(http.StatusOK, h.settingsSnapshot())
}

// auditSettingChanges 为本次请求中真正发生变化的设置键各写一条审计。
//
// before/after 都是完整的生效快照，逐键比对即可覆盖所有设置项（含运行期策略 13 项），
// 无需在每个字段上重复判断「这次传没传」。
func (h *Handlers) auditSettingChanges(c *gin.Context, before, after SettingsResponse) {
	beforeValues := settingSnapshotValues(before)
	afterValues := settingSnapshotValues(after)
	for key, newVal := range afterValues {
		oldVal := beforeValues[key]
		if oldVal == newVal {
			continue
		}
		h.AuditLog(
			c, repository.SettingAuditAction, "setting", key, "",
			repository.EncodeSettingHistoryDetail(oldVal, newVal), "ok",
		)
	}
}

// settingSnapshotValues 把设置快照摊平成「设置键 → 字符串值」。
//
// 键名与 setting 表的键名一致（而非 API 的 JSON 字段名）：历史查询按设置键过滤，
// 两者不一致会让「按 key 查历史」查不到任何记录。
func settingSnapshotValues(s SettingsResponse) map[string]string {
	return map[string]string{
		"public_url":                      s.PublicURL,
		"upstream_timeout":                strconv.Itoa(s.UpstreamTimeout),
		"group_member_timeout":            strconv.Itoa(s.GroupMemberTimeout),
		"negative_cache_ttl":              strconv.Itoa(s.NegativeCacheTTL),
		"blob_gc_interval":                strconv.Itoa(s.BlobGCInterval),
		"storage_cleanup_interval":        strconv.Itoa(s.StorageCleanupInterval),
		"backup_orphan_cleanup_interval":  strconv.Itoa(s.BackupOrphanCleanupInterval),
		"trash_gc_interval":               strconv.Itoa(s.TrashGCInterval),
		"upstream_probe_interval":         strconv.Itoa(s.UpstreamProbeInterval),
		"backup_orphan_grace_hours":       strconv.Itoa(s.BackupOrphanGraceHours),
		"storage_metadata_retention_days": strconv.Itoa(s.StorageMetadataRetentionDays),
		"storage_temp_max_age_hours":      strconv.Itoa(s.StorageTempMaxAgeHours),
		"trash_retention_days":            strconv.Itoa(s.TrashRetentionDays),
		"trash_max_bytes":                 strconv.FormatInt(s.TrashMaxBytes, 10),
		"auto_block_base_seconds":         strconv.Itoa(s.AutoBlockBaseSeconds),
		"search_timeout_seconds":          strconv.Itoa(s.SearchTimeoutSeconds),
		"capacity_cache_ttl_seconds":      strconv.Itoa(s.CapacityCacheTTLSeconds),
		"backup_auto_interval":            strconv.Itoa(s.BackupAutoInterval),
		"backup_keep_last":                strconv.Itoa(s.BackupKeepLast),
		"backup_retention_days":           strconv.Itoa(s.BackupRetentionDays),
	}
}

// applyRuntimePolicy 把已写入的 13 项运行期策略推给持有运行时状态的一方。
//
// 只推**本次请求真的提交了的**字段（夹取后的值，nil 表示没提交）：
// 「传哪个改哪个」是本节点的既有语义，把未提交的项也推一遍会让装配层误以为
// 运维刚改过它。
//
// 说明为什么保留策略四项不在这里：它们的读取方是作业，作业每轮现读 setting 表
// （见 wiring.go 的 registerStorageCleanupJob / registerTrashGCJob 等），
// 写库即已生效，再按 key 推一次反而是第二份真源。
func (h *Handlers) applyRuntimePolicy(update domain.SettingsUpdate) {
	// 时长类 8 项：装配层按 key 决定推给调度器还是对应服务。
	durations := []struct {
		key  string
		secs *int
	}{
		{domain.SettingKeyBlobGCInterval, update.BlobGCInterval},
		{domain.SettingKeyStorageCleanupInterval, update.StorageCleanupInterval},
		{domain.SettingKeyBackupOrphanCleanupInterval, update.BackupOrphanCleanupInterval},
		{domain.SettingKeyTrashGCInterval, update.TrashGCInterval},
		{domain.SettingKeyUpstreamProbeInterval, update.UpstreamProbeInterval},
		{domain.SettingKeySearchTimeoutSeconds, update.SearchTimeoutSeconds},
		{domain.SettingKeyAutoBlockBaseSeconds, update.AutoBlockBaseSeconds},
		{domain.SettingKeyCapacityCacheTTLSeconds, update.CapacityCacheTTLSeconds},
	}
	for _, item := range durations {
		if item.secs == nil || h.onRuntimePolicyChange == nil {
			continue
		}
		h.onRuntimePolicyChange(item.key, time.Duration(*item.secs)*time.Second)
	}
	if update.TrashMaxBytes != nil && h.onTrashCapacityChange != nil {
		h.onTrashCapacityChange(*update.TrashMaxBytes)
	}
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
