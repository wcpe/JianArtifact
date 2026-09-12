package domain_test

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestOriginTokenGuardRoundTrip(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewSettingService(repository.NewSettingRepo(db))

	enabled, header, value := svc.OriginTokenGuard()
	if enabled || header != "" || value != "" {
		t.Fatalf("初始应为空：%v/%q/%q", enabled, header, value)
	}

	if err := svc.SetOriginTokenGuard(true, "X-Jian-Origin-Token", "tok-0123456789abcdef"); err != nil {
		t.Fatalf("写入：%v", err)
	}
	enabled, header, value = svc.OriginTokenGuard()
	if !enabled || header != "X-Jian-Origin-Token" || value != "tok-0123456789abcdef" {
		t.Fatalf("回读不符：%v/%q/%q", enabled, header, value)
	}

	if err := svc.SetOriginTokenGuard(false, "", ""); err != nil {
		t.Fatalf("关闭：%v", err)
	}
	enabled, _, _ = svc.OriginTokenGuard()
	if enabled {
		t.Fatal("关闭后应为 false")
	}
}
