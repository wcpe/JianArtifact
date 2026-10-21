package domain

import (
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 实例级基础配置的设置键（：web 可读可改，运行时生效）。同步间隔复用复制命名空间键。
const (
	settingKeyAnonymousAccess   = "anonymous_access_enabled"
	SettingKeyPublicURL         = "public_url"
	SettingKeyUpstreamTimeout   = "upstream_timeout"
	SettingKeyAllowedHosts      = "allowed_hosts"        // 允许访问的域名白名单（逗号分隔；节点本地）
	SettingKeyOriginTokenEnable = "origin_token_enabled" // 回源 Token 校验开关（；节点本地）
	SettingKeyOriginTokenHeader = "origin_token_header"  // 回源 Token 请求头名（；节点本地）
	SettingKeyOriginTokenValue  = "origin_token_value"   // 回源 Token 值（；节点本地）
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
	SyncInterval       *int
	AllowedHosts       *[]string
	OriginTokenEnabled *bool
	OriginTokenHeader  *string
	OriginTokenValue   *string
	//  追加：自动备份与保留策略（运行时可改，写后由作业下一轮读取生效）。
	BackupAutoInterval  *int
	BackupKeepLast      *int
	BackupRetentionDays *int
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
	v, err := s.settings.Get(settingKeyAnonymousAccess)
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

// PublicURL 返回对外基础 URL（，读 setting）；未配置返回空串（回退请求 Host 推断）。
func (s *SettingService) PublicURL() string {
	v, err := s.settings.Get(SettingKeyPublicURL)
	if err != nil {
		return ""
	}
	return v
}

// SetPublicURL 写入对外基础 URL（空串表示未配置，回退请求推断）。
func (s *SettingService) SetPublicURL(v string) error {
	return s.UpdateSettings(SettingsUpdate{PublicURL: &v})
}

// SyncIntervalSecs 返回同步轮询间隔（秒，）；未配置或非法返回 0（调度器回退构造值）。
// SettingKeyReplSyncInterval 是设置页「同步间隔」的存储键（，用户可见配置；
// 名字里的 repl: 是历史遗留，复制退役后由该键继续承载此配置项）。
const SettingKeyReplSyncInterval = "repl:sync_interval"

// SettingKeyClusterPrefix 是「节点本地设置」的前缀判定键（历史遗留，保留）。
const SettingKeyClusterPrefix = "repl:"

func (s *SettingService) SyncIntervalSecs() int {
	return s.secsSetting(SettingKeyReplSyncInterval)
}

// SetSyncInterval 写入同步轮询间隔（秒）。
func (s *SettingService) SetSyncInterval(secs int) error {
	return s.UpdateSettings(SettingsUpdate{SyncInterval: &secs})
}

// UpstreamTimeoutSecs 返回回源整体超时（秒，）；未配置或非法返回 0。
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
	v, err := s.settings.Get(key)
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
		if _, err := s.settings.Get(key); err == nil {
			return true
		}
	}
	return false
}

// AllowedHostsList 返回允许访问的域名白名单；未配置或空值返回空切片（不限制）。
// 条目经逗号分隔、按首尾空白归一化，空项忽略。节点本地配置，不参与复制。
func (s *SettingService) AllowedHostsList() []string {
	v, err := s.settings.Get(SettingKeyAllowedHosts)
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
	if v, err := s.settings.Get(SettingKeyOriginTokenEnable); err == nil {
		enabled = v == "true"
	}
	header, _ = s.settings.Get(SettingKeyOriginTokenHeader)
	value, _ = s.settings.Get(SettingKeyOriginTokenValue)
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
func (s *SettingService) secsSetting(key string) int {
	v, err := s.settings.Get(key)
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
		SettingKeyBackupAutoInterval, SettingKeyBackupKeepLast, SettingKeyBackupRetentionDays:
		return true
	}
	return strings.HasPrefix(key, SettingKeyClusterPrefix)
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
	if update.SyncInterval != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyReplSyncInterval, Value: strconv.Itoa(*update.SyncInterval)})
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
	//  追加：自动备份三项。0 是合法值（关闭 / 不限制），照写不跳过。
	if update.BackupAutoInterval != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyBackupAutoInterval, Value: strconv.Itoa(*update.BackupAutoInterval)})
	}
	if update.BackupKeepLast != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyBackupKeepLast, Value: strconv.Itoa(*update.BackupKeepLast)})
	}
	if update.BackupRetentionDays != nil {
		values = append(values, repository.SettingValue{Key: SettingKeyBackupRetentionDays, Value: strconv.Itoa(*update.BackupRetentionDays)})
	}
	return values
}
