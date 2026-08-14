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

// TestReplicationCmdStatusAndToggle replication CLI：status/start/stop 无错误，start 后开关生效（FR-86）。
func TestReplicationCmdStatusAndToggle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "replication-test-secret-key-32byte!!")
	t.Setenv(config.EnvSyncPeerURL, "http://peer.example")
	t.Setenv(config.EnvSyncToken, "secret-token")

	// status 无错误（未同步过 → 水位未同步提示）。
	if err := replicationCmd([]string{"status"}); err != nil {
		t.Fatalf("replication status：%v", err)
	}

	// stop / start 均无错误。
	if err := replicationCmd([]string{"stop"}); err != nil {
		t.Fatalf("replication stop：%v", err)
	}
	if err := replicationCmd([]string{"start"}); err != nil {
		t.Fatalf("replication start：%v", err)
	}
	// 未知子命令应报错。
	if err := replicationCmd([]string{"bogus"}); err == nil {
		t.Error("未知子命令应报错")
	}
}

// TestReplicationCmdNoPeer 未配置对端时 replication 子命令应正常提示（FR-88：对端配置与同步解耦）。
func TestReplicationCmdNoPeer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "replication-test-secret-key-32byte!!")
	// 不设置 JIAN_SYNC_PEER_URL。
	if err := replicationCmd([]string{"status"}); err != nil {
		t.Errorf("未配置对端时 status 应正常提示而非报错：%v", err)
	}
}
