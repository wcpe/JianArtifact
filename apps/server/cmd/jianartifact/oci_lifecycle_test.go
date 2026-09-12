package main

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

func TestOpenServicesPrimaryStartupLeavesUnreferencedBlobForScheduledGC(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "oci-startup-cleanup-test-secret-key")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	first, err := openServices(cfg)
	if err != nil {
		t.Fatalf("首次装配服务：%v", err)
	}
	if err := first.db.Close(); err != nil {
		t.Fatal(err)
	}
	store := blobstore.NewStore(cfg.BlobDir)
	orphan, _, _, _, err := store.Put(bytes.NewReader([]byte("oci-uncommitted-orphan")))
	if err != nil {
		t.Fatalf("创建孤立 blob：%v", err)
	}
	upload, err := store.CreateOCIUploadTemp()
	if err != nil {
		t.Fatalf("创建 OCI 上传暂存：%v", err)
	}
	uploadPath := upload.Name()
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openServices(cfg)
	if err != nil {
		t.Fatalf("重启装配服务：%v", err)
	}
	t.Cleanup(func() { _ = second.db.Close() })
	if !store.Exists(orphan) {
		t.Fatal("主节点启动不得立即扫描并清理无引用 blob")
	}
	if _, err := os.Stat(uploadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("主节点启动必须清理 OCI 上传会话文件，err=%v", err)
	}
}
