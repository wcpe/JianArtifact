package upstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTailBufferKeepsOnlyTail 验证 stderr 缓冲不会无限增长。
// 用 bytes.Buffer 承接子进程 stderr 时，一个反复告警的进程能让它涨到 OOM，
// 因此必须限长并只保留尾部（启动失败的诊断信息总在最后几行）。
func TestTailBufferKeepsOnlyTail(t *testing.T) {
	buf := newTailBuffer(64)
	for i := 0; i < 200; i++ {
		if _, err := buf.Write([]byte("0123456789")); err != nil {
			t.Fatalf("写入失败：%v", err)
		}
	}
	got := buf.String()
	if len(got) != 64 {
		t.Fatalf("缓冲应被限制为 64 字节，实际 %d", len(got))
	}
	// 尾部内容应是最新写入的数据。
	if !strings.HasSuffix(got, "89") {
		t.Fatalf("应保留最新写入的尾部数据，得 %q", got)
	}
}

// TestTailBufferConcurrentAccess 覆盖「拷贝 goroutine 写入 vs 读取」的并发场景：
// 原实现用无锁 bytes.Buffer，在 stopInstance 等待超时后读 stderr 会与之竞态。
func TestTailBufferConcurrentAccess(t *testing.T) {
	buf := newTailBuffer(128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			_, _ = buf.Write([]byte("line\n"))
		}
	}()
	// 并发读取：不加锁的缓冲会在这里被 -race 检出。
	for i := 0; i < 100; i++ {
		_ = buf.String()
	}
	<-done
	if buf.String() == "" {
		t.Fatal("并发写入后不应为空")
	}
}

// TestSanitizeMihomoStderrFlattensLines 验证子进程 stderr 被压成单行且裁短：
// 该文本会进入 API 响应，必须去掉换行（防日志注入）并限制长度。
func TestSanitizeMihomoStderrFlattensLines(t *testing.T) {
	raw := "level=error msg=\"parse config failed\"\r\n  proxies[0].password: hunter2\nnext-line\n"
	got := sanitizeMihomoStderr(raw)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("结果不应含换行，得 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("连续空白应被压缩，得 %q", got)
	}
	long := strings.Repeat("x", 2000)
	if len(sanitizeMihomoStderr(long)) > 240 {
		t.Fatal("超长 stderr 应被裁短到 240 字节以内")
	}
	if sanitizeMihomoStderr("   \n  ") != "" {
		t.Fatal("纯空白应归为空串")
	}
}

// TestMinimalMihomoEnvDoesNotLeakSecrets 是 M-4 回归测试：
// 默认继承 os.Environ() 会把本进程密钥交给第三方（GPL）子进程。
func TestMinimalMihomoEnvDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("JIAN_JWT_SECRET", "super-secret-jwt")
	t.Setenv("JIAN_OUTBOUND_PROXY_KEY", strings.Repeat("a", 64))
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_MAVEN", "user:pass")

	workDir := t.TempDir()
	env := minimalMihomoEnv(workDir)
	for _, entry := range env {
		for _, secret := range []string{"super-secret-jwt", strings.Repeat("a", 64), "user:pass"} {
			if strings.Contains(entry, secret) {
				t.Fatalf("mihomo 环境变量泄露了本进程密钥：%s", entry)
			}
		}
	}
	// 必须带上工作目录相关的两项，否则 mihomo 会把缓存写到数据目录之外。
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "HOME="+workDir) || !strings.Contains(joined, "TMPDIR="+workDir) {
		t.Fatalf("应把 HOME/TMPDIR 指向工作目录，得 %v", env)
	}
}

// TestValidMihomoServerHostRejectsControlChars 验证 server 字段不接受控制字符：
// 该字段会进入 config.yaml 并出现在审计日志里的 endpointPreview 中。
func TestValidMihomoServerHostRejectsControlChars(t *testing.T) {
	good := []string{"hk.example.com", "1.2.3.4", "2001:db8::1", "[2001:db8::1]", "node-01_a.example"}
	for _, s := range good {
		if !validMihomoServerHost(s) {
			t.Fatalf("合法 server 被拒绝：%q", s)
		}
	}
	bad := []string{"", "host with space", "host\x00evil", "host\x1b[31m", "host/path", "user@host", strings.Repeat("a", 256)}
	for _, s := range bad {
		if validMihomoServerHost(s) {
			t.Fatalf("非法 server 应被拒绝：%q", s)
		}
	}
}

// TestValidateAndNormalizeMihomoNodeDropsUnknownKeys 是 m-3 回归测试：
// proxies[0] 会原样进入 mihomo 运行配置，未知键（dialer-proxy / interface-name 等）
// 能改变子进程的网络行为，必须按白名单丢弃。
func TestValidateAndNormalizeMihomoNodeDropsUnknownKeys(t *testing.T) {
	node, preview, err := validateAndNormalizeMihomoNode(map[string]any{
		"type":           "vless",
		"server":         "hk.example.com",
		"port":           443,
		"uuid":           "11111111-1111-1111-1111-111111111111",
		"dialer-proxy":   "another-group",
		"interface-name": "eth9",
		"routing-mark":   99,
		"tun":            map[string]any{"enable": true},
	})
	if err != nil {
		t.Fatalf("合法节点构造失败：%v", err)
	}
	for _, forbidden := range []string{"dialer-proxy", "interface-name", "routing-mark", "tun"} {
		if _, exists := node[forbidden]; exists {
			t.Fatalf("未知键 %q 不应进入节点配置", forbidden)
		}
	}
	if node["uuid"] != "11111111-1111-1111-1111-111111111111" {
		t.Fatal("白名单内的字段应保留")
	}
	if strings.Contains(preview, "11111111") {
		t.Fatalf("预览不得泄露 uuid：%q", preview)
	}
}

// TestMihomoManagerCleanupStaleWorkDirs 验证启动期清扫残留的明文配置目录。
func TestMihomoManagerCleanupStaleWorkDirs(t *testing.T) {
	dataDir := t.TempDir()
	base := filepath.Join(dataDir, "tmp", "mihomo")
	stale := filepath.Join(base, "proxy-1")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("准备残留目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "config.yaml"), []byte("password: leaked"), 0o600); err != nil {
		t.Fatalf("写入残留配置失败：%v", err)
	}
	// 非 proxy-* 目录不应被误删。
	keep := filepath.Join(base, "keepme")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatalf("准备保留目录失败：%v", err)
	}

	mgr := NewMihomoManager("", dataDir)
	if removed := mgr.CleanupStaleWorkDirs(); removed != 1 {
		t.Fatalf("应清理 1 个遗留目录，实际 %d", removed)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("残留的 proxy-* 目录应被删除")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("非 proxy-* 目录不应被删除")
	}
}

// TestMihomoManagerBackoffPreventsCrashLoop 是 M-3 回归测试：
// 坏节点配置会让 mihomo 秒退；没有退避时**每个回源请求**都会 fork 一次进程。
func TestMihomoManagerBackoffPreventsCrashLoop(t *testing.T) {
	mgr := NewMihomoManager("", t.TempDir())
	// 模拟连续启动失败。
	mgr.recordStartupFailure(1)
	mgr.recordStartupFailure(1)
	mgr.recordStartupFailure(1)

	err := mgr.checkBackoff(1)
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("冷却期内应快速失败并返回 ErrProxyUnavailable，得 %v", err)
	}
	// 未失败的代理不受影响。
	if err := mgr.checkBackoff(2); err != nil {
		t.Fatalf("未失败的代理不应被冷却，得 %v", err)
	}
	// 成功后清除退避。
	mgr.clearBackoff(1)
	if err := mgr.checkBackoff(1); err != nil {
		t.Fatalf("清除退避后应可重试，得 %v", err)
	}
}

// TestMihomoManagerBackoffGrowsExponentially 验证退避时长按失败次数递增且有上限。
func TestMihomoManagerBackoffGrowsExponentially(t *testing.T) {
	mgr := NewMihomoManager("", t.TempDir())
	var waits []time.Duration
	for i := 0; i < 10; i++ {
		mgr.recordStartupFailure(1)
		mgr.mu.Lock()
		waits = append(waits, time.Until(mgr.backoff[1].until))
		mgr.mu.Unlock()
	}
	if waits[0] >= waits[2] {
		t.Fatalf("退避应递增：%v", waits[:3])
	}
	last := waits[len(waits)-1]
	if last > mihomoStartupBackoffMax+time.Second {
		t.Fatalf("退避不应超过上限 %v，实际 %v", mihomoStartupBackoffMax, last)
	}
}

// TestMihomoManagerCapabilitiesReportsUnavailableForBrokenBinary 是 m-2 回归测试：
// 找到文件不等于能执行（架构不符、缺动态库），此时不能报 Available=true。
func TestMihomoManagerCapabilitiesReportsUnavailableForBrokenBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本模拟不可执行的二进制，仅适用于类 Unix")
	}
	dir := t.TempDir()
	broken := filepath.Join(dir, "mihomo")
	// 无执行位的普通文件：Stat 成功但无法 exec。
	if err := os.WriteFile(broken, []byte("not a binary"), 0o600); err != nil {
		t.Fatalf("准备假二进制失败：%v", err)
	}
	mgr := NewMihomoManager(broken, dir)
	caps := mgr.Capabilities(context.Background())
	if caps.Available {
		t.Fatal("不可执行的二进制不应报 Available=true")
	}
}
