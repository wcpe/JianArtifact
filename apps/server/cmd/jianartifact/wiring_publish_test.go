package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestOpenServicesPublishUsesEnvelopeWithoutNilNodeID 是装配级回归测试：
// 复制退役后发布恒走原子信封路径，若 wiring 漏接 assetSvc.SetNodeIdentity，
// nodeID 为 nil 函数，任何发布都会 500。本测试直接走真实 openServices 发布。
func TestOpenServicesPublishUsesEnvelopeWithoutNilNodeID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "wiring-publish-test-secret-key-32bytes")
	t.Setenv(config.EnvEnabledFormats, "raw")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })

	if _, err := svc.repoSvc.Create("wiring-publish", "raw", "hosted", "public", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.assetSvc.Put("wiring-publish", "a.bin", bytes.NewReader([]byte("payload")), "application/octet-stream"); err != nil {
		t.Fatalf("发布制品（信封路径不得因 nodeID 未接线而失败）：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs")); err != nil {
		t.Fatalf("blob 目录应存在：%v", err)
	}
}
