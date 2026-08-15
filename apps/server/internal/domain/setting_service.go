package domain

import (
	"errors"
	"log"
	"strconv"

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
	v := "false"
	if enabled {
		v = "true"
	}
	if err := s.settings.Set(settingKeyAnonymousAccess, v); err != nil {
		return err
	}
	s.recordChange(EntitySetting, SettingKey(settingKeyAnonymousAccess), OpPut, SettingChangeData{
		Key: settingKeyAnonymousAccess, Value: v,
	})
	return nil
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
	if err := s.settings.Set(SettingKeyPublicURL, v); err != nil {
		return err
	}
	s.recordChange(EntitySetting, SettingKey(SettingKeyPublicURL), OpPut, SettingChangeData{
		Key: SettingKeyPublicURL, Value: v,
	})
	return nil
}

// SyncIntervalSecs 返回同步轮询间隔（秒，FR-89）；未配置或非法返回 0（调度器回退构造值）。
func (s *SettingService) SyncIntervalSecs() int {
	return s.secsSetting(SettingKeyReplSyncInterval)
}

// SetSyncInterval 写入同步轮询间隔（秒）。
func (s *SettingService) SetSyncInterval(secs int) error {
	return s.setIntSetting(SettingKeyReplSyncInterval, secs)
}

// UpstreamTimeoutSecs 返回回源整体超时（秒，FR-89）；未配置或非法返回 0。
func (s *SettingService) UpstreamTimeoutSecs() int {
	return s.secsSetting(SettingKeyUpstreamTimeout)
}

// SetUpstreamTimeout 写入回源整体超时（秒）。
func (s *SettingService) SetUpstreamTimeout(secs int) error {
	return s.setIntSetting(SettingKeyUpstreamTimeout, secs)
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

// setIntSetting 写入整数秒设置并记录复制变更日志。
func (s *SettingService) setIntSetting(key string, secs int) error {
	v := strconv.Itoa(secs)
	if err := s.settings.Set(key, v); err != nil {
		return err
	}
	s.recordChange(EntitySetting, SettingKey(key), OpPut, SettingChangeData{Key: key, Value: v})
	return nil
}
