package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCreatesIndependentMigrationCredentialKey(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(EnvDataDir, dataDir)
	t.Setenv(EnvJWTSecret, "jwt-secret-must-not-be-reused")
	t.Setenv(EnvMigrationCredentialKey, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	if len(cfg.MigrationCredentialKey) != 32 {
		t.Fatalf("迁移凭据密钥长度 = %d，期望 32", len(cfg.MigrationCredentialKey))
	}
	if bytes.Equal(cfg.MigrationCredentialKey, cfg.JWTSecret) {
		t.Fatal("迁移凭据密钥不得复用 JWT 密钥")
	}
	if _, err := os.Stat(filepath.Join(dataDir, migrationCredentialKeyFileName)); err != nil {
		t.Fatalf("迁移凭据密钥文件未创建：%v", err)
	}
}

func TestLoadMigrationCredentialKeyPrefersEnvironment(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv(EnvJWTSecret, "jwt-secret-must-not-be-reused")
	t.Setenv(EnvMigrationCredentialKey, base64.StdEncoding.EncodeToString(key))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	if !bytes.Equal(cfg.MigrationCredentialKey, key) {
		t.Fatal("环境变量迁移凭据密钥未生效")
	}
}

func TestEnabledFormatsDefaultAndExplicitEmpty(t *testing.T) {
	t.Setenv(EnvEnabledFormats, "")
	got, err := enabledFormats()
	if err != nil || got.Any() {
		t.Fatalf("显式空配置应关闭全部格式：%#v %v", got.List(), err)
	}
	if err := os.Unsetenv(EnvEnabledFormats); err != nil {
		t.Fatal(err)
	}
	got, err = enabledFormats()
	if err != nil || !got.Has("raw") || !got.Has("maven") || !got.Has("npm") {
		t.Fatalf("缺失配置应兼容默认三格式：%#v %v", got.List(), err)
	}
}

func TestEnabledFormatsRejectsUnknown(t *testing.T) {
	t.Setenv(EnvEnabledFormats, "raw,unknown")
	if _, err := enabledFormats(); err == nil {
		t.Fatal("未知格式应导致启动配置失败")
	}
}

func TestLoadTLSConfig(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv(EnvJWTSecret, "jwt-secret-must-not-be-reused")
	t.Setenv(EnvMigrationCredentialKey, "")

	t.Setenv(EnvTLSAddr, ":8443")
	t.Setenv(EnvTLSCert, "/etc/jianartifact/tls/cert.pem")
	t.Setenv(EnvTLSKey, "/etc/jianartifact/tls/key.pem")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	if cfg.TLSAddr != ":8443" || cfg.TLSCert != "/etc/jianartifact/tls/cert.pem" || cfg.TLSKey != "/etc/jianartifact/tls/key.pem" {
		t.Fatalf("TLS 配置解析不符：%+v", cfg)
	}

	t.Setenv(EnvTLSAddr, "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	if cfg.TLSAddr != "" {
		t.Fatal("未配置 JIAN_TLS_ADDR 时 TLSAddr 应为空")
	}
}
