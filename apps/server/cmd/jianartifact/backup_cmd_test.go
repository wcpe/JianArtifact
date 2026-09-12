package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// newBackupCLIEnv 准备一个隔离的数据目录并写入一条资产，返回数据目录。
func newBackupCLIEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.EnvDataDir, dir)
	t.Setenv(config.EnvJWTSecret, "backup-cli-test-secret-key-32-bytes!")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	defer func() { _ = svc.db.Close() }()

	store := blobstore.NewStore(filepath.Join(dir, "blobs"))
	hash, _, _, size, err := store.Put(strings.NewReader("cli-payload"))
	if err != nil {
		t.Fatalf("写入 blob：%v", err)
	}
	res, err := svc.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`)
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	if _, err := svc.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size) VALUES (?, 'a.bin', ?, ?)`,
		repoID, hash, size); err != nil {
		t.Fatalf("插入资产：%v", err)
	}
	return dir
}

// runBackupCmd 捕获输出执行一条备份子命令。
// 复用 commands_test.go 的 captureStdout（其函数参数无返回值），故用闭包回传错误。
func runBackupCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var callErr error
	out := captureStdout(t, func() { callErr = backupCmd(args) })
	return out, callErr
}

func TestBackupCmdRejectsBadUsage(t *testing.T) {
	newBackupCLIEnv(t)
	cases := []struct {
		name string
		args []string
	}{
		{"无子命令", nil},
		{"未知子命令", []string{"nope"}},
		{"未知模式", []string{"create", "--mode", "weird"}},
		{"verify 缺目标", []string{"verify"}},
		{"link 缺目标", []string{"link", "--base", "https://example.test"}},
		{"delete 缺目标", []string{"delete"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runBackupCmd(t, tc.args...); err == nil {
				t.Fatalf("应返回错误：%v", tc.args)
			}
		})
	}
}

func TestBackupCmdVerifyRejectsUnknownTarget(t *testing.T) {
	dir := newBackupCLIEnv(t)
	if _, err := runBackupCmd(t, "verify", filepath.Join(dir, "不存在.tar.gz")); err == nil {
		t.Fatal("不存在的路径应报错")
	}
}

func TestBackupCmdLinkRejectsUnknownPackage(t *testing.T) {
	newBackupCLIEnv(t)
	if _, err := runBackupCmd(t, "link", "bk-missing", "--base", "https://example.test"); err == nil {
		t.Fatal("不存在的包标识应报错")
	}
}

// TestBackupCmdLifecycle 覆盖 create → list → verify → link → delete 全链路。
func TestBackupCmdLifecycle(t *testing.T) {
	newBackupCLIEnv(t)

	out, err := runBackupCmd(t, "create", "--label", "cli")
	if err != nil {
		t.Fatalf("create：%v（输出 %s）", err, out)
	}
	packageID := extractPackageID(t, out)

	out, err = runBackupCmd(t, "list")
	if err != nil {
		t.Fatalf("list：%v", err)
	}
	if !strings.Contains(out, packageID) {
		t.Fatalf("列表未包含新包 %s：%s", packageID, out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("列表应显示 done 状态：%s", out)
	}

	if _, err := runBackupCmd(t, "verify", packageID, "--deep"); err != nil {
		t.Fatalf("verify：%v", err)
	}

	out, err = runBackupCmd(t, "link", packageID, "--base", "https://repo.example.test", "--ttl", "10m")
	if err != nil {
		t.Fatalf("link：%v", err)
	}
	link := strings.TrimSpace(out)
	if !strings.HasPrefix(link, "https://repo.example.test/api/v1/backups/"+packageID+"/download?") {
		t.Fatalf("链接格式有误：%s", link)
	}
	if !strings.Contains(link, "token=") || !strings.Contains(link, "exp=") {
		t.Fatalf("链接缺少签名参数：%s", link)
	}

	if _, err := runBackupCmd(t, "delete", packageID); err != nil {
		t.Fatalf("delete：%v", err)
	}
	if _, err := runBackupCmd(t, "delete", packageID); err == nil {
		t.Fatal("重复删除应报错")
	}
}

// TestBackupCmdLinkRequiresBase 验证缺对外基址时明确报错，而不是签出一个相对链接。
func TestBackupCmdLinkRequiresBase(t *testing.T) {
	newBackupCLIEnv(t)
	t.Setenv(config.EnvPublicURL, "")

	out, err := runBackupCmd(t, "create")
	if err != nil {
		t.Fatalf("create：%v", err)
	}
	packageID := extractPackageID(t, out)

	if _, err := runBackupCmd(t, "link", packageID); err == nil {
		t.Fatal("缺少对外基址时应报错")
	}
}

func TestPositionalArgSkipsOptionValues(t *testing.T) {
	if got := positionalArg([]string{"--ttl", "30m", "bk-1"}); got != "bk-1" {
		t.Fatalf("positionalArg = %q，期望 bk-1", got)
	}
	if got := positionalArg([]string{"--deep"}); got != "" {
		t.Fatalf("仅开关时 positionalArg = %q，期望空", got)
	}
}

// TestBackupCmdCreateWithBase 覆盖 create --base 生成增量差包：输出展示基线 id，
// 且 list 也展示差包的基线。
func TestBackupCmdCreateWithBase(t *testing.T) {
	newBackupCLIEnv(t)

	out, err := runBackupCmd(t, "create", "--label", "base")
	if err != nil {
		t.Fatalf("create 基线：%v（输出 %s）", err, out)
	}
	baseID := extractPackageID(t, out)

	out, err = runBackupCmd(t, "create", "--base", baseID, "--label", "delta")
	if err != nil {
		t.Fatalf("create 差包：%v（输出 %s）", err, out)
	}
	deltaID := extractPackageID(t, out)
	if deltaID == baseID {
		t.Fatal("差包标识应与基线不同")
	}
	if !strings.Contains(out, "增量差包") || !strings.Contains(out, baseID) {
		t.Fatalf("差包输出应展示基线 id：%s", out)
	}

	out, err = runBackupCmd(t, "list")
	if err != nil {
		t.Fatalf("list：%v", err)
	}
	if !strings.Contains(out, "基线="+baseID) {
		t.Fatalf("列表应展示差包基线：%s", out)
	}
}

// TestBackupCmdCreateRejectsUnknownBase 覆盖 create --base 指向不存在的基线 → 报错。
func TestBackupCmdCreateRejectsUnknownBase(t *testing.T) {
	newBackupCLIEnv(t)
	if _, err := runBackupCmd(t, "create", "--base", "bk-missing"); err == nil {
		t.Fatal("不存在的基线应报错")
	}
}

// extractPackageID 从 create 输出里取出包标识。
func extractPackageID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "已生成备份包 ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "已生成备份包 "))
		}
	}
	t.Fatalf("未能从输出中解析包标识：%s", out)
	return ""
}
