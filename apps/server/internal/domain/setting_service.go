package domain

import (
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 实例级基础配置的设置键（web 可读可改，运行时生效）。同步间隔复用复制命名空间键。
const (
	settingKeyAnonymousAccess = "anonymous_access_enabled"
	SettingKeyPublicURL       = "public_url"
	SettingKeyUpstreamTimeout = "upstream_timeout"
	SettingKeyAllowedHosts    = "allowed_hosts" // 允许访问的域名白名单（逗号分隔；节点本地）
	// 回源 Token 三项都是节点本地配置：只影响本节点的回源校验。
	SettingKeyOriginTokenEnable = "origin_token_enabled" // 回源 Token 校验开关（节点本地）
	SettingKeyOriginTokenHeader = "origin_token_header"  // 回源 Token 请求头名（节点本地）
	SettingKeyOriginTokenValue  = "origin_token_value"   // 回源 Token 值（节点本地，不打印）
)

// SettingService 处理实例级全局设置。无缓存：SQLite 直查，写后即生效。
type SettingService struct {
	settings  *repository.SettingRepo
	recorder  ChangeRecorder
	writeGate BusinessWriteGate
}

// SettingsUpdate 表示一次原子基础设置更新；nil 字段保持不变。
type SettingsUpdate struct {
	AnonymousAccess    *bool
	PublicURL          *string
	UpstreamTimeout    *int
	AllowedHosts       *[]string
	OriginTokenEnabled *bool
	OriginTokenHeader  *string
	OriginTokenValue   *string
	// 自动备份与保留策略三项（运行时可改，写后由作业下一轮读取生效）。
	BackupAutoInterval  *int
	BackupKeepLast      *int
	BackupRetentionDays *int
	// proxy/group 读路径调优两项（运行时可改，保存后经装配层回调热生效，无需重启）。
	GroupMemberTimeout *int
	NegativeCacheTTL   *int
	// 运行期策略 13 项（作业间隔 / 保留策略 / 容量与阈值）。生效方式各不相同，
	// 逐项说明见各键定义处的注释：作业间隔靠调度器 SetInterval 热改，
	// 保留策略由作业每轮现读，容量与阈值经装配层回调注入对应服务。
	// 这里用指针是必需的：0 在多数项上是合法取值（禁用 / 不限制），
	// 用值类型无法区分「本次不修改」与「改成 0」。
	BlobGCInterval               *int
	StorageCleanupInterval       *int
	BackupOrphanCleanupInterval  *int
	TrashGCInterval              *int
	UpstreamProbeInterval        *int
	BackupOrphanGraceHours       *int
	StorageMetadataRetentionDays *int
	StorageTempMaxAgeHours       *int
	TrashRetentionDays           *int
	TrashMaxBytes                *int64
	AutoBlockBaseSeconds         *int
	SearchTimeoutSeconds         *int
	CapacityCacheTTLSeconds      *int
}

// NewSettingService 构造 SettingService。
func NewSettingService(settings *repository.SettingRepo) *SettingService {
	return &SettingService{settings: settings}
}

// SetChangeRecorder 注入复制变更日志记录器；nil 表示不记录。
func (s *SettingService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

// SetBusinessWriteGate 注入备用节点本地业务写栅栏；nil 保持兼容行为。
func (s *SettingService) SetBusinessWriteGate(gate BusinessWriteGate) { s.writeGate = gate }

// recordChange 记录复制变更日志；记录失败不阻断业务写（对账兜底，见 ADR-0013）。
func (s *SettingService) recordChange(entityType, entityKey, op string, data any) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Record(entityType, entityKey, op, data); err != nil {
		log.Printf("复制变更日志记录失败 entity=%s key=%s op=%s：%v", entityType, entityKey, op, err)
	}
}

// AnonymousAccessEnabled 返回匿名访问全局开关；键缺失视为默认开启。
func (s *SettingService) AnonymousAccessEnabled() (bool, error) {
	v, err := s.getRaw(settingKeyAnonymousAccess)
	if errors.Is(err, repository.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}

// SetAnonymousAccessEnabled 写入匿名访问全局开关。
func (s *SettingService) SetAnonymousAccessEnabled(enabled bool) error {
	return s.UpdateSettings(SettingsUpdate{AnonymousAccess: &enabled})
}

// PublicURL 返回对外基础 URL（读 setting）；未配置返回空串（回退请求 Host 推断）。
func (s *SettingService) PublicURL() string {
	v, err := s.getRaw(SettingKeyPublicURL)
	if err != nil {
		return ""
	}
	return v
}

// SetPublicURL 写入对外基础 URL（空串表示未配置，回退请求推断）。
func (s *SettingService) SetPublicURL(v string) error {
	return s.UpdateSettings(SettingsUpdate{PublicURL: &v})
}

// UpstreamTimeoutSecs 返回回源整体超时（秒）；未配置或非法返回 0。
func (s *SettingService) UpstreamTimeoutSecs() int {
	return s.secsSetting(SettingKeyUpstreamTimeout)
}

// SetUpstreamTimeout 写入回源整体超时（秒）。
func (s *SettingService) SetUpstreamTimeout(secs int) error {
	return s.UpdateSettings(SettingsUpdate{UpstreamTimeout: &secs})
}

// 自动备份与保留策略的设置键（追加：原先只能经环境变量配置，现放开为运行时可改）。
//
// 三项均为**节点本地**（见 isNodeLocalSetting）：备份包落在本节点磁盘上，
// 保留策略是"这台机器怎么用自己的盘"的决策，跨节点复制该设置会让一个节点的
// 磁盘策略被另一个节点改写。
//
// 环境变量的角色随之变化：从"唯一真源"降级为**首次播种的初值**——启动时若
// setting 表没有对应键，用环境变量值写入；此后以 setting 表为准。这样既有部署
// 的环境变量配置继续有效，同时可被界面覆盖（见 cmd/jianartifact/wiring.go）。
const (
	SettingKeyBackupAutoInterval  = "backup_auto_interval"  // 自动备份间隔（秒），0 = 关闭
	SettingKeyBackupKeepLast      = "backup_keep_last"      // 保留最近 N 份自动备份，0 = 不按份数限制
	SettingKeyBackupRetentionDays = "backup_retention_days" // 保留最近 N 天的自动备份，0 = 不限天数
)

// BackupAutoIntervalSecs 返回自动备份间隔（秒）；未配置或非法返回 0（= 关闭）。
//
// 与 secsSetting 不同，本方法**允许 0**：对自动备份来说 0 是有意义的配置值
// （关闭），必须与"未配置"区分开来，否则界面无法把开关拨回关闭状态。
func (s *SettingService) BackupAutoIntervalSecs() int {
	return s.nonNegativeIntSetting(SettingKeyBackupAutoInterval)
}

// BackupKeepLast 返回保留份数；未配置或非法返回 0（= 不按份数限制）。
func (s *SettingService) BackupKeepLast() int {
	return s.nonNegativeIntSetting(SettingKeyBackupKeepLast)
}

// BackupRetentionDays 返回保留天数；未配置或非法返回 0（= 不限天数）。
func (s *SettingService) BackupRetentionDays() int {
	return s.nonNegativeIntSetting(SettingKeyBackupRetentionDays)
}

// nonNegativeIntSetting 读取非负整数设置；未配置或非法返回 0。
// 与 secsSetting 的差别是接受 0（0 在备份配置里是"关闭 / 不限制"的合法取值）。
func (s *SettingService) nonNegativeIntSetting(key string) int {
	v, err := s.getRaw(key)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// SaveBackupSettings 写入自动备份三项配置（并发安全：单次事务写入，写后即生效）。
func (s *SettingService) SaveBackupSettings(autoInterval, keepLast, retentionDays int) error {
	return s.UpdateSettings(SettingsUpdate{
		BackupAutoInterval:  &autoInterval,
		BackupKeepLast:      &keepLast,
		BackupRetentionDays: &retentionDays,
	})
}

// HasBackupSettings 报告 setting 表里是否已存在三项备份配置中的任意一项。
//
// 供启动播种逻辑判断"是否已有界面配置"：表里没有才把环境变量当作初值写入；
// 已有则一律不动，避免每次重启都把运维在界面上改过的值覆盖回环境变量值。
func (s *SettingService) HasBackupSettings() bool {
	for _, key := range []string{
		SettingKeyBackupAutoInterval,
		SettingKeyBackupKeepLast,
		SettingKeyBackupRetentionDays,
	} {
		if _, err := s.getRaw(key); err == nil {
			return true
		}
	}
	return false
}

// proxy/group 读路径调优两项的设置键（运行时可改，保存后经装配层回调热生效）。
//
// 两项都是**节点本地**（见 isNodeLocalSetting）：
//   - group_member_timeout 是「本节点到上游的网络条件能给出多少确认预算」——同一 group
//     里不同节点到同一上游的时延本就不同，把一个节点的预算复制给另一个节点没有意义；
//   - negative_cache_ttl 是「本进程内存态负缓存活多久」——负缓存本身就是进程内 map，
//     另一个节点根本没有这份缓存，复制该值只会把本节点策略强加给它。
//
// 环境变量的角色与备份三项逐字相同：从「唯一真源」降级为**首次播种的初值**——启动时
// 若 setting 表没有对应键，用环境变量值写入；此后以 setting 表为准（见 wiring.go）。
const (
	// SettingKeyGroupMemberTimeout 是 group 成员确认阶段预算（秒）。本项**无禁用语义**：
	// 0 秒预算等于每个成员都立刻超时，整条 group 读会退化成不可判定失败，故非正数回退默认。
	SettingKeyGroupMemberTimeout = "group_member_timeout"
	// SettingKeyNegativeCacheTTL 是 404 负缓存 TTL（秒）。**0 = 关闭负缓存**，是合法取值。
	SettingKeyNegativeCacheTTL = "negative_cache_ttl"
)

// GroupMemberTimeoutSecs 返回 group 成员确认阶段预算（秒）；未配置或非法返回 0。
//
// 复用 secsSetting 是**正确**的：本项确实没有禁用语义，0 与「未配置」都应当表示
// 「沿用环境变量/默认值」，调用方据此只在 secs > 0 时应用。口径同 UpstreamTimeoutSecs。
func (s *SettingService) GroupMemberTimeoutSecs() int {
	return s.secsSetting(SettingKeyGroupMemberTimeout)
}

// NegativeCacheTTLSecs 返回负缓存 TTL（秒）与「键是否已配置」。
//
// 为什么不能复用 secsSetting / nonNegativeIntSetting：
//   - secsSetting 把 0 归零成「未配置」，与「0 = 关闭负缓存」直接冲突，关不掉；
//   - nonNegativeIntSetting 接受 0，但**缺失时也返回 0**，于是一个被删掉的键（或一次
//     播种失败）会静默变成「关闭负缓存」——那会让每个缺失路径都真实回源、放大上游压力。
//
// 因此这里用 ok 把「显式关闭」与「未配置 / 值非法」彻底分开：
// 只有 ok=true 且 secs==0 才是关闭；ok=false 时调用方必须回退环境变量/默认值。
func (s *SettingService) NegativeCacheTTLSecs() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyNegativeCacheTTL)
}

// 运行期策略 13 项的设置键：作业多久跑一次、保留多久、容量与阈值多大。
//
// # 为什么这 13 项要搬进 setting 表
//
// 它们原本只由环境变量决定，于是「把 storage-cleanup 从每天改成每 6 小时」这种
// 纯运行期调整也要重启一个承载数十 GB 数据的实例。而这些值本身没有任何启动期约束
// （不是监听地址、不是数据目录、不是密钥），重启对它们没有技术必要性。
//
// # 环境变量的角色
//
// 与备份三项、proxy/group 调优两项逐字相同：从「唯一真源」降级为**首次播种的初值**——
// 启动时若 setting 表没有对应键，用环境变量值写入；此后以 setting 表为准
// （见 cmd/jianartifact/wiring.go 的 seedRuntimePolicySettings）。
//
// # 全部节点本地
//
// 13 项都进 isNodeLocalSetting 白名单，理由见那里的注释。
//
// # 0 的语义分三类，读取函数的返回形态据此不同
//
//   - **0 = 禁用 / 不限制**（5 个作业间隔、元数据保留天数、临时文件最长存活、
//     回收站保留天数、回收站上限、容量缓存 TTL）：0 与「键缺失」必须分开，
//     否则界面关掉的功能会被下一次读取悄悄按默认值打开。这类用 (值, ok) 返回。
//   - **不允许 0**（孤儿包宽限期）：零宽限会在写入途中把包删掉，0 与负数一律回落默认。
//   - **0 = 回落默认**（断路器退避、搜索预算）：取 0 没有意义（见各自 config 注释），
//     用普通 int 返回，调用方只在 > 0 时应用。
const (
	// 五个周期作业的间隔（秒）。0 = 禁用该作业（不注册 / 不触发），是本批最重要的一条语义：
	// 运维需要能在界面上关掉一个持续占用 IO 的清理作业，而不是只能改环境变量重启。
	SettingKeyBlobGCInterval              = "blob_gc_interval"               // 孤儿 blob 回收
	SettingKeyStorageCleanupInterval      = "storage_cleanup_interval"       // 存储治理清理
	SettingKeyBackupOrphanCleanupInterval = "backup_orphan_cleanup_interval" // 孤儿备份包回收
	SettingKeyTrashGCInterval             = "trash_gc_interval"              // 回收站保留期回收
	SettingKeyUpstreamProbeInterval       = "upstream_probe_interval"        // 上游健康定时探测

	// SettingKeyBackupOrphanGraceHours 是孤儿备份包的回收宽限期（小时）。
	// **不允许 0**：零宽限会让「刚拷进 backups/、还没来得及在界面导入」的包在下一轮
	// 就被判为孤儿删掉，而这正是宽限期存在的唯一目的。故 0 与负数一律回落默认。
	SettingKeyBackupOrphanGraceHours = "backup_orphan_grace_hours"

	// 存储治理清理的两个阈值。0 均为「禁用该子步骤」（不是「立刻清理」）：
	// 0 天保留等于把所有终态元数据判超期，一次笔误就会清空审计与操作历史。
	SettingKeyStorageMetadataRetentionDays = "storage_metadata_retention_days" // 终态元数据保留天数
	SettingKeyStorageTempMaxAgeHours       = "storage_temp_max_age_hours"      // 过期上传临时文件最长存活（小时）

	// SettingKeyTrashRetentionDays 是回收站条目保留天数。0 = 不自动清理（只保留手动彻底删除），
	// 不是「立刻全删」：后者会让一次笔误抹掉整个回收站。
	SettingKeyTrashRetentionDays = "trash_retention_days"
	// SettingKeyTrashMaxBytes 是回收站字节上限。0 = 不限制；超限时拒绝**新的清理**，
	// 而不是淘汰旧条目——静默删除等于绕过用户确认。
	SettingKeyTrashMaxBytes = "trash_max_bytes"

	// SettingKeyAutoBlockBaseSeconds 是 proxy 上游首次失败的阻止窗口（秒），之后每档翻倍。
	// 0 = 回落默认（「失败不阻止」不是有意义的配置）。
	SettingKeyAutoBlockBaseSeconds = "auto_block_base_seconds"
	// SettingKeySearchTimeoutSeconds 是搜索链路总预算（秒）。0 = 回落默认：
	// 搜索是无界查询，关掉超时等于开放一个可长期占住数据库连接的入口。
	SettingKeySearchTimeoutSeconds = "search_timeout_seconds"
	// SettingKeyCapacityCacheTTLSeconds 是仪表盘容量聚合缓存的有效期（秒）。0 = 禁用缓存
	// （每次请求实时聚合），是合法取值：批量导入期间运维需要仪表盘立刻反映真实占用。
	SettingKeyCapacityCacheTTLSeconds = "capacity_cache_ttl_seconds"
)

// BlobGCIntervalSecs 返回孤儿 blob 回收作业的间隔（秒）与「键是否已配置」。
// ok=true 且 secs==0 表示**显式禁用**，调用方不得回退默认值。
func (s *SettingService) BlobGCIntervalSecs() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyBlobGCInterval)
}

// StorageCleanupIntervalSecs 返回存储治理清理的间隔（秒）与是否已配置；0 = 禁用。
func (s *SettingService) StorageCleanupIntervalSecs() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyStorageCleanupInterval)
}

// BackupOrphanCleanupIntervalSecs 返回孤儿备份包回收的间隔（秒）与是否已配置；0 = 禁用。
func (s *SettingService) BackupOrphanCleanupIntervalSecs() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyBackupOrphanCleanupInterval)
}

// TrashGCIntervalSecs 返回回收站保留期回收的间隔（秒）与是否已配置；0 = 禁用。
func (s *SettingService) TrashGCIntervalSecs() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyTrashGCInterval)
}

// UpstreamProbeIntervalSecs 返回上游健康定时探测的间隔（秒）与是否已配置；0 = 关闭探测。
func (s *SettingService) UpstreamProbeIntervalSecs() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyUpstreamProbeInterval)
}

// BackupOrphanGraceHours 返回孤儿备份包的回收宽限期（小时）；未配置或非法返回 0。
//
// 用普通 int 返回（不带 ok）是刻意的：本项**没有禁用语义**，0 小时宽限等于
// 「下一轮就把正在拷入的包当孤儿删掉」，因此 0 与「未配置」在调用方必须走同一条分支
// ——只在 > 0 时应用，否则保留当前的（默认）宽限期。口径同 GroupMemberTimeoutSecs。
func (s *SettingService) BackupOrphanGraceHours() int {
	return s.nonNegativeIntSetting(SettingKeyBackupOrphanGraceHours)
}

// StorageMetadataRetentionDays 返回终态元数据保留天数与是否已配置；0 = 禁用裁剪。
func (s *SettingService) StorageMetadataRetentionDays() (days int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyStorageMetadataRetentionDays)
}

// StorageTempMaxAgeHours 返回过期上传临时文件的最长存活时长（小时）与是否已配置；0 = 禁用。
func (s *SettingService) StorageTempMaxAgeHours() (hours int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyStorageTempMaxAgeHours)
}

// TrashRetentionDays 返回回收站保留天数与是否已配置；0 = 不自动清理。
func (s *SettingService) TrashRetentionDays() (days int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyTrashRetentionDays)
}

// TrashMaxBytes 返回回收站字节上限与是否已配置；0 = 不限制。
// 用 int64 是因为 2 GiB 已经超出 32 位 int 的范围，而字节数是最容易写到 10 位以上的量。
func (s *SettingService) TrashMaxBytes() (n int64, ok bool) {
	v, err := s.getRaw(SettingKeyTrashMaxBytes)
	if err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

// AutoBlockBaseSeconds 返回断路器退避起始时长（秒）；未配置或非法返回 0（= 沿用当前值）。
// 本项无禁用语义（0 窗口等于「失败不阻止」），口径同 GroupMemberTimeoutSecs。
func (s *SettingService) AutoBlockBaseSeconds() int {
	return s.nonNegativeIntSetting(SettingKeyAutoBlockBaseSeconds)
}

// SearchTimeoutSeconds 返回搜索链路总预算（秒）；未配置或非法返回 0（= 沿用当前值）。
// 本项无禁用语义（关闭超时等于放开无界查询），口径同 GroupMemberTimeoutSecs。
func (s *SettingService) SearchTimeoutSeconds() int {
	return s.nonNegativeIntSetting(SettingKeySearchTimeoutSeconds)
}

// CapacityCacheTTLSeconds 返回仪表盘容量缓存有效期（秒）与是否已配置；0 = 禁用缓存。
func (s *SettingService) CapacityCacheTTLSeconds() (secs int, ok bool) {
	return s.nonNegativeIntSettingOK(SettingKeyCapacityCacheTTLSeconds)
}

// nonNegativeIntSettingOK 读取非负整数设置，并用 ok 区分「键缺失 / 值非法」与「显式 0」。
//
// 为什么需要它（与另外两个读取函数的分工）：
//   - secsSetting 把 0 归零成「未配置」，凡是 0 有语义的项都不能用它；
//   - nonNegativeIntSetting 接受 0，但**缺失时也返回 0**，调用方无法把「运维显式关掉」
//     与「一次播种失败」区分开——前者要照实生效，后者必须回退默认值。
//
// 因此「0 有语义」的项一律走本函数：只有 ok=true 且值为 0 才是关闭。
func (s *SettingService) nonNegativeIntSettingOK(key string) (n int, ok bool) {
	v, err := s.getRaw(key)
	if err != nil {
		return 0, false
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed, true
}

// AllowedHostsList 返回允许访问的域名白名单；未配置或空值返回空切片（不限制）。
// 条目经逗号分隔、按首尾空白归一化，空项忽略。节点本地配置，不参与复制。
func (s *SettingService) AllowedHostsList() []string {
	v, err := s.getRaw(SettingKeyAllowedHosts)
	if err != nil {
		return nil
	}
	raw := strings.Split(v, ",")
	hosts := make([]string, 0, len(raw))
	for _, item := range raw {
		if host := strings.TrimSpace(item); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// SetAllowedHosts 写入允许访问的域名白名单（空切片表示不限制）。
func (s *SettingService) SetAllowedHosts(hosts []string) error {
	return s.UpdateSettings(SettingsUpdate{AllowedHosts: &hosts})
}

// OriginTokenGuard 返回回源 Token 校验配置：开关、请求头名与 Token 值；未配置返回零值（不启用）。
// 节点本地，不参与复制；重新生成 Token 后旧值立即失效。
func (s *SettingService) OriginTokenGuard() (enabled bool, header, value string) {
	if v, err := s.getRaw(SettingKeyOriginTokenEnable); err == nil {
		enabled = v == "true"
	}
	header, _ = s.getRaw(SettingKeyOriginTokenHeader)
	value, _ = s.getRaw(SettingKeyOriginTokenValue)
	return enabled, strings.TrimSpace(header), strings.TrimSpace(value)
}

// SetOriginTokenGuard 写入回源 Token 校验配置；空 value 表示清空（关闭后重新开启需重新生成）。
func (s *SettingService) SetOriginTokenGuard(enabled bool, header, value string) error {
	return s.UpdateSettings(SettingsUpdate{
		OriginTokenEnabled: &enabled,
		OriginTokenHeader:  &header,
		OriginTokenValue:   &value,
	})
}

// secsSetting 读取整数秒设置；缺失 / 非数字 / 非正数返回 0。
// getRaw 是 SettingService 读取 setting 的**唯一入口**，负责 nil 仓储守卫。
//
// 为什么必须统一：Handlers 在拿不到 SettingService 时会用 settingServiceOrZero()
// 造一个零值服务（settings 字段为 nil），随后 settingsSnapshot 仍会逐项读取回填。
// 任何一处直接调 s.settings.Get 都会在未装配场景下 panic，把 GET /settings 变成 500。
// 集中到一处，新增读取项时就不会再漏。
func (s *SettingService) getRaw(key string) (string, error) {
	if s.settings == nil {
		return "", repository.ErrNotFound
	}
	return s.settings.Get(key)
}

func (s *SettingService) secsSetting(key string) int {
	v, err := s.getRaw(key)
	if err != nil {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return secs
}

// UpdateSettings 在同一数据库事务内写入全部基础设置，提交后再记录复制变更。
func (s *SettingService) UpdateSettings(update SettingsUpdate) error {
	if err := requireBusinessWrite(s.writeGate); err != nil {
		return err
	}
	values := settingValues(update)
	if err := s.settings.SetMany(values); err != nil {
		return err
	}
	for _, value := range values {
		if isNodeLocalSetting(value.Key) {
			continue
		}
		s.recordChange(EntitySetting, SettingKey(value.Key), OpPut, SettingChangeData{
			Key: value.Key, Value: value.Value,
		})
	}
	return nil
}

// isNodeLocalSetting 返回不应跨节点复制的设置键。
func isNodeLocalSetting(key string) bool {
	switch key {
	case SettingKeyPublicURL, SettingKeyAllowedHosts,
		SettingKeyOriginTokenEnable, SettingKeyOriginTokenHeader, SettingKeyOriginTokenValue,
		// 备份配置是"本节点怎么用自己的磁盘"的决策，不跨节点复制。
		SettingKeyBackupAutoInterval, SettingKeyBackupKeepLast, SettingKeyBackupRetentionDays,
		// proxy/group 调优两项同属节点本地：成员确认预算取决于本节点到上游的时延，
		// 负缓存 TTL 约束的是本进程内存态的存活期（理由见键定义处的注释）。
		SettingKeyGroupMemberTimeout, SettingKeyNegativeCacheTTL,
		// 运行期策略 13 项全部是节点本地，理由与 group_member_timeout 同源：
		// 它们约束的是「本节点怎么用自己的磁盘、多久跑一次自己的作业、本进程缓存活多久」，
		// 而磁盘容量、IO 余量、上游时延在节点之间本来就不同。
		// 把一个节点的「回收站上限 2 GiB」或「blob-gc 每天一次」复制给另一个节点，
		// 等于让 A 机器的磁盘策略去改写 B 机器，且 B 上的作业会在无人知情时改频率。
		SettingKeyBlobGCInterval, SettingKeyStorageCleanupInterval,
		SettingKeyBackupOrphanCleanupInterval, SettingKeyTrashGCInterval,
		SettingKeyUpstreamProbeInterval,
		SettingKeyBackupOrphanGraceHours, SettingKeyStorageMetadataRetentionDays,
		SettingKeyStorageTempMaxAgeHours, SettingKeyTrashRetentionDays,
		SettingKeyTrashMaxBytes,
		SettingKeyAutoBlockBaseSeconds, SettingKeySearchTimeoutSeconds,
		SettingKeyCapacityCacheTTLSeconds:
		return true
	}
	// 原先这里还有一条「repl: 前缀一律算节点本地」的兜底。集群复制退役后，
	// repl: 键不再被写入也不再被读取（sync_interval 已随复制一起删除），
	// 保留这条前缀判断只会让「这个键算不算节点本地」有一个无人使用的旁路。
	return false
}

// settingValues 将可选更新转换为稳定顺序的持久化键值列表。
func settingValues(update SettingsUpdate) []repository.SettingValue {
	values := make([]repository.SettingValue, 0, 8)
	if update.AnonymousAccess != nil {
		values = append(values, repository.SettingValue{Key: settingKeyAnonymousAccess, Value: strconv.FormatBool(*update.AnonymousAccess)})
	}
	if update.PublicURL != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyPublicURL, Value: *update.PublicURL})
	}
	if update.UpstreamTimeout != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyUpstreamTimeout, Value: strconv.Itoa(*update.UpstreamTimeout)})
	}
	if update.AllowedHosts != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyAllowedHosts, Value: strings.Join(*update.AllowedHosts, ",")})
	}
	if update.OriginTokenEnabled != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyOriginTokenEnable, Value: strconv.FormatBool(*update.OriginTokenEnabled)})
	}
	if update.OriginTokenHeader != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyOriginTokenHeader, Value: *update.OriginTokenHeader})
	}
	if update.OriginTokenValue != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyOriginTokenValue, Value: *update.OriginTokenValue})
	}
	// 自动备份三项。0 是合法值（关闭 / 不限制），照写不跳过。
	if update.BackupAutoInterval != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyBackupAutoInterval, Value: strconv.Itoa(*update.BackupAutoInterval)})
	}
	if update.BackupKeepLast != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyBackupKeepLast, Value: strconv.Itoa(*update.BackupKeepLast)})
	}
	if update.BackupRetentionDays != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyBackupRetentionDays, Value: strconv.Itoa(*update.BackupRetentionDays)})
	}
	// proxy/group 调优两项。0 在负缓存 TTL 里是「关闭」的合法取值，照写不跳过
	// （跳过等于把「关闭」存成「未配置」，下一轮读取又会退回默认 60s）。
	if update.GroupMemberTimeout != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyGroupMemberTimeout, Value: strconv.Itoa(*update.GroupMemberTimeout)})
	}
	if update.NegativeCacheTTL != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyNegativeCacheTTL, Value: strconv.Itoa(*update.NegativeCacheTTL)})
	}
	// 运行期策略 13 项。逐项照写、不做「非零才写」的过滤：其中 10 项的 0 是合法取值
	// （禁用 / 不限制），跳过等于把「显式关闭」存成「未配置」，下一次读取又会退回默认值。
	for _, item := range []struct {
		key   string
		value *int
	}{
		{SettingKeyBlobGCInterval, update.BlobGCInterval},
		{SettingKeyStorageCleanupInterval, update.StorageCleanupInterval},
		{SettingKeyBackupOrphanCleanupInterval, update.BackupOrphanCleanupInterval},
		{SettingKeyTrashGCInterval, update.TrashGCInterval},
		{SettingKeyUpstreamProbeInterval, update.UpstreamProbeInterval},
		{SettingKeyBackupOrphanGraceHours, update.BackupOrphanGraceHours},
		{SettingKeyStorageMetadataRetentionDays, update.StorageMetadataRetentionDays},
		{SettingKeyStorageTempMaxAgeHours, update.StorageTempMaxAgeHours},
		{SettingKeyTrashRetentionDays, update.TrashRetentionDays},
		{SettingKeyAutoBlockBaseSeconds, update.AutoBlockBaseSeconds},
		{SettingKeySearchTimeoutSeconds, update.SearchTimeoutSeconds},
		{SettingKeyCapacityCacheTTLSeconds, update.CapacityCacheTTLSeconds},
	} {
		if item.value != nil {
			values = append(values, repository.SettingValue{Key: item.key, Value: strconv.Itoa(*item.value)})
		}
	}
	// 回收站上限单独处理：它是 int64（字节数，2 GiB 已超出 32 位 int），
	// 与其余 12 项的 int 不同，无法塞进上面那张表。
	if update.TrashMaxBytes != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyTrashMaxBytes, Value: strconv.FormatInt(*update.TrashMaxBytes, 10)})
	}
	return values
}
