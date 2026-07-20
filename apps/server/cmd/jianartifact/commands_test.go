package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// TestAdminResetCreatesThenResets 覆盖 admin reset 两条路径：
// 无该管理员则创建；已存在则改密。改密后旧口令失效、新口令可登录。
func TestAdminResetCreatesThenResets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "admin-reset-test-secret-key-32byte!!")

	// 首次：无管理员 → 创建。
	if err := adminReset([]string{"--username", "admin", "--password", "first-pass-123"}); err != nil {
		t.Fatalf("首次 reset（创建）：%v", err)
	}
	if !canLogin(t, "admin", "first-pass-123") {
		t.Fatal("创建后应能以初始口令登录")
	}

	// 再次：已存在 → 改密。
	if err := adminReset([]string{"--username", "admin", "--password", "second-pass-456"}); err != nil {
		t.Fatalf("二次 reset（改密）：%v", err)
	}
	if canLogin(t, "admin", "first-pass-123") {
		t.Error("改密后旧口令不应再登录")
	}
	if !canLogin(t, "admin", "second-pass-456") {
		t.Error("改密后应能以新口令登录")
	}
}

// TestAdminResetRejectsEmptyPassword 非交互环境下缺省口令应报错（不静默创建空口令账号）。
func TestAdminResetRejectsEmptyPassword(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "admin-reset-test-secret-key-32byte!!")

	// 未提供 --password 且 stdin 非终端：readPasswordTwice 返回错误。
	if err := adminReset([]string{"--username", "admin"}); err == nil {
		t.Fatal("非交互式且无 --password 时应报错")
	}
}

// canLogin 用离线服务校验给定口令能否登录。
func canLogin(t *testing.T, username, password string) bool {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	defer func() { _ = svc.db.Close() }()
	_, _, err = svc.authSvc.Login(username, password)
	return err == nil
}

// TestUsageListsCommands usage 输出应列出全部子命令，供无参数 / help / 未知命令时展示。
func TestUsageListsCommands(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	out := buf.String()
	for _, want := range []string{"run", "status", "admin reset", "healthcheck", "help", "JIAN_HTTP_ADDR"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage 输出应包含 %q，实际：\n%s", want, out)
		}
	}
}
