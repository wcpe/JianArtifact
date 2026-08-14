package domain

import (
	"errors"
	"log"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// settingKeyAnonymousAccess 是匿名访问全局开关的设置键（FR-66）。
const settingKeyAnonymousAccess = "anonymous_access_enabled"

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
