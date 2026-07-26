package domain

import (
	"errors"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// settingKeyAnonymousAccess 是匿名访问全局开关的设置键（FR-66）。
const settingKeyAnonymousAccess = "anonymous_access_enabled"

// SettingService 处理实例级全局设置。无缓存：SQLite 直查，写后即生效。
type SettingService struct {
	settings *repository.SettingRepo
}

// NewSettingService 构造 SettingService。
func NewSettingService(settings *repository.SettingRepo) *SettingService {
	return &SettingService{settings: settings}
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
	return s.settings.Set(settingKeyAnonymousAccess, v)
}
