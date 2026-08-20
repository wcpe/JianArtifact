package domain

import (
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 实例级基础配置的设置键（FR-89：web 可读可改，运行时生效）。同步间隔复用复制命名空间键。
const (
	settingKeyAnonymousAccess = "anonymous_access_enabled"
	SettingKeyPublicURL       = "public_url"
	SettingKeyUpstreamTimeout = "upstream_timeout"
)

// SettingService 处理实例级全局设置。无缓存：SQLite 直查，写后即生效。
type SettingService struct {
	settings *repository.SettingRepo
	recorder ChangeRecorder
}

// SettingsUpdate 表示一次原子基础设置更新；nil 字段保持不变。
type SettingsUpdate struct {
	AnonymousAccess *bool
	PublicURL       *string
	UpstreamTimeout *int
	SyncInterval    *int
}

// NewSettingService 构造 SettingService。
func NewSettingService(settings *repository.SettingRepo) *SettingService {
	return &SettingService{settings: settings}
}

// SetChangeRecorder 注入复制变更日志记录器（FR-83）；nil 表示不记录。
func (s *SettingService) SetChangeRecorder(r ChangeRecorder) { s.recorder = r }

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

// PublicURL 返回对外基础 URL（FR-89，读 setting）；未配置返回空串（回退请求 Host 推断）。
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

// SyncIntervalSecs 返回同步轮询间隔（秒，FR-89）；未配置或非法返回 0（调度器回退构造值）。
func (s *SettingService) SyncIntervalSecs() int {
	return s.secsSetting(SettingKeyReplSyncInterval)
}

// SetSyncInterval 写入同步轮询间隔（秒）。
func (s *SettingService) SetSyncInterval(secs int) error {
	return s.UpdateSettings(SettingsUpdate{SyncInterval: &secs})
}

// UpstreamTimeoutSecs 返回回源整体超时（秒，FR-89）；未配置或非法返回 0。
func (s *SettingService) UpstreamTimeoutSecs() int {
	return s.secsSetting(SettingKeyUpstreamTimeout)
}

// SetUpstreamTimeout 写入回源整体超时（秒）。
func (s *SettingService) SetUpstreamTimeout(secs int) error {
	return s.UpdateSettings(SettingsUpdate{UpstreamTimeout: &secs})
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
	return key == SettingKeyPublicURL || strings.HasPrefix(key, SettingKeyClusterPrefix)
}

// settingValues 将可选更新转换为稳定顺序的持久化键值列表。
func settingValues(update SettingsUpdate) []repository.SettingValue {
	values := make([]repository.SettingValue, 0, 4)
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
	return values
}
