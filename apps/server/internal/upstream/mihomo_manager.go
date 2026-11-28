package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrMihomoNotInstalled 表示宿主机未找到可用的 mihomo / clash-meta 可执行文件。
var ErrMihomoNotInstalled = errors.New("upstream: 未检测到 mihomo 可执行文件，请安装 mihomo 或设置 JIAN_MIHOMO_BIN")

// MihomoCapabilities 描述当前宿主机上受管 mihomo 运行时的可用状态。
type MihomoCapabilities struct {
	Available  bool
	BinaryPath string
	Version    string
}

type managedMihomo struct {
	configHash string
	port       int
	workDir    string
	cmd        *exec.Cmd
	done       chan struct{}
	stderr     *tailBuffer
	transport  *http.Transport
}

// mihomoStderrTailBytes 是保留的子进程 stderr 尾部字节数。
//
// 必须限长：os/exec 会为 Stderr 起一个拷贝 goroutine 并在**整个子进程生命周期内**
// 持续写入，用 bytes.Buffer 时一个反复告警/崩溃重启的 mihomo 可以让它无限增长直到 OOM。
// 只保留尾部是因为启动失败的诊断信息（解析错误、端口占用）总在最后几行。
const mihomoStderrTailBytes = 4096

// mihomoStartupBackoffBase / Max 控制「启动失败后的冷却窗口」。
//
// 没有退避时，一个必然秒退的坏节点（口令错、协议不支持、证书问题）会让**每个回源请求**
// 都 fork 一次进程：分配端口 → 写含密钥的配置 → Start → 等最多 5s。这既是资源浪费，
// 也会把单节点故障放大成整站尾延迟。退避期内直接快速失败，让调用方按普通上游不可用处理。
const (
	mihomoStartupBackoffBase = time.Second
	mihomoStartupBackoffMax  = 60 * time.Second
)

// tailBuffer 是带锁、限长的字节缓冲：并发写入只保留最后 max 字节，读取安全。
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		// 从尾部保留，避免无限增长；拷贝一份防止底层数组一直持有旧数据。
		trimmed := make([]byte, t.max)
		copy(trimmed, t.buf[len(t.buf)-t.max:])
		t.buf = trimmed
	}
	return len(p), nil
}

// String 返回当前尾部内容（并发安全）。
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// MihomoManager 负责按需拉起、健康托管与销毁本地受管 mihomo 子进程。
//
// 进程隔离设计：
//   - 每个启用的 Mihomo 代理节点独立生成最小化配置并绑定 127.0.0.1:<随机空闲端口> + 随机认证口令；
//   - 不将 GPL-3.0 的 mihomo 源码静态链接进 MIT 许可的 JianArtifact 单二进制；
//   - 代理配置变更或进程退出时自动终止旧子进程并清除含密钥的临时配置文件。
type MihomoManager struct {
	mu            sync.Mutex
	configuredBin string
	dataDir       string
	instances     map[int64]*managedMihomo
	// backoff 记录每个代理 ID 的启动失败冷却状态（见 mihomoStartupBackoffBase）。
	backoff map[int64]*mihomoBackoff
	// startMu 按代理 ID 串行化启动过程：启动含进程派生与最长 5s 端口等待，
	// 不能放在 mu 里做——那会让任一代理启动期间所有代理的回源全部阻塞。
	startMu sync.Mutex
}

type mihomoBackoff struct {
	failures int
	until    time.Time
}

// NewMihomoManager 构造 Mihomo 子进程管理器。
func NewMihomoManager(configuredBin, dataDir string) *MihomoManager {
	return &MihomoManager{
		configuredBin: strings.TrimSpace(configuredBin),
		dataDir:       strings.TrimSpace(dataDir),
		instances:     make(map[int64]*managedMihomo),
		backoff:       make(map[int64]*mihomoBackoff),
	}
}

// Capabilities 探测本机可用的 mihomo 二进制与版本信息。
func (m *MihomoManager) Capabilities(ctx context.Context) MihomoCapabilities {
	bin, err := m.resolveBinary()
	if err != nil {
		return MihomoCapabilities{Available: false}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, bin, "-v").CombinedOutput()
	if err != nil {
		// 找到文件不等于能跑：架构不符、缺动态库、无执行位都会在这一步失败。
		// 此时若仍报 Available=true，前端会把「可用」显示给运维，而实际每次回源都会失败。
		return MihomoCapabilities{Available: false, BinaryPath: bin}
	}
	version := ""
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 {
		version = strings.TrimSpace(lines[0])
	}
	return MihomoCapabilities{
		Available:  true,
		BinaryPath: bin,
		Version:    version,
	}
}

func (m *MihomoManager) resolveBinary() (string, error) {
	if m.configuredBin != "" {
		if st, err := os.Stat(m.configuredBin); err == nil && !st.IsDir() {
			return m.configuredBin, nil
		}
		if resolved, err := exec.LookPath(m.configuredBin); err == nil {
			return resolved, nil
		}
		return "", ErrMihomoNotInstalled
	}
	candidates := []string{}
	if m.dataDir != "" {
		names := []string{"mihomo", "clash-meta"}
		if runtime.GOOS == "windows" {
			names = []string{"mihomo.exe", "clash-meta.exe"}
		}
		for _, n := range names {
			candidates = append(candidates, filepath.Join(m.dataDir, "bin", n))
		}
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	for _, name := range []string{"mihomo", "clash-meta"} {
		if resolved, err := exec.LookPath(name); err == nil {
			return resolved, nil
		}
	}
	return "", ErrMihomoNotInstalled
}

// TransportFor 返回 proxyID 对应的受管 mihomo 本地 SOCKS5 传输层；
// 若尚未启动、子进程已退出或 configHash 发生变更，则自动重启并等待本地端口就绪。
//
// 锁边界：mu 只保护 instances / backoff 两个 map 的读写，**进程派生与端口等待都在锁外**
// （startMu 按代理串行化同一代理的并发启动）。把启动放在 mu 里会让任一代理首次使用或
// 崩溃重启期间，所有其它代理的回源一起阻塞最长 7s，形成整站尾延迟。
func (m *MihomoManager) TransportFor(ctx context.Context, proxyID int64, configHash string, node map[string]any) (*http.Transport, error) {
	// 快路径：已就绪且配置未变，直接复用（只在 mu 内做一次 map 查找）。
	if tr, ok := m.reuseTransport(proxyID, configHash); ok {
		return tr, nil
	}
	if err := m.checkBackoff(proxyID); err != nil {
		return nil, err
	}

	// 同一代理的启动串行化：不同代理互不阻塞。
	m.startMu.Lock()
	defer m.startMu.Unlock()

	// 二次检查：等锁期间可能已被别的请求启动完成（或配置又被改回去）。
	if tr, ok := m.reuseTransport(proxyID, configHash); ok {
		return tr, nil
	}
	if err := m.checkBackoff(proxyID); err != nil {
		return nil, err
	}

	tr, err := m.start(ctx, proxyID, configHash, node)
	if err != nil {
		m.recordStartupFailure(proxyID)
		return nil, err
	}
	m.clearBackoff(proxyID)
	return tr, nil
}

// reuseTransport 在实例已就绪且配置未变时返回其传输层；否则清理掉过期实例。
func (m *MihomoManager) reuseTransport(proxyID int64, configHash string) (*http.Transport, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.instances[proxyID]
	if !ok {
		return nil, false
	}
	select {
	case <-existing.done:
		// 子进程已异常退出：清理后由调用方重建。
		m.cleanupLocked(proxyID)
		return nil, false
	default:
		if existing.configHash == configHash {
			return existing.transport, true
		}
		m.cleanupLocked(proxyID)
		return nil, false
	}
}

// checkBackoff 在冷却窗口内快速失败，避免坏节点让每个请求都 fork 一次进程。
func (m *MihomoManager) checkBackoff(proxyID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.backoff[proxyID]
	if !ok || b.failures == 0 {
		return nil
	}
	if time.Now().Before(b.until) {
		return fmt.Errorf("%w: mihomo 启动连续失败 %d 次，冷却中", ErrProxyUnavailable, b.failures)
	}
	return nil
}

func (m *MihomoManager) recordStartupFailure(proxyID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.backoff[proxyID]
	if !ok {
		b = &mihomoBackoff{}
		m.backoff[proxyID] = b
	}
	b.failures++
	wait := mihomoStartupBackoffBase
	for i := 1; i < b.failures && wait < mihomoStartupBackoffMax; i++ {
		wait *= 2
	}
	if wait > mihomoStartupBackoffMax {
		wait = mihomoStartupBackoffMax
	}
	b.until = time.Now().Add(wait)
}

func (m *MihomoManager) clearBackoff(proxyID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.backoff, proxyID)
}

// start 实际派生 mihomo 子进程。调用方必须已持有 startMu（保证同一代理只有一个启动在跑）。
func (m *MihomoManager) start(ctx context.Context, proxyID int64, configHash string, node map[string]any) (*http.Transport, error) {
	bin, err := m.resolveBinary()
	if err != nil {
		return nil, err
	}
	port, err := pickFreeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("upstream: 分配 Mihomo 本地端口失败：%w", err)
	}
	authUser := "jian"
	authPass, err := randomHexSecret(16)
	if err != nil {
		return nil, fmt.Errorf("upstream: 生成 Mihomo 本地认证口令失败：%w", err)
	}
	yamlBytes, err := RenderMihomoRuntimeYAML(node, port, authUser, authPass)
	if err != nil {
		return nil, err
	}
	baseTmp := filepath.Join(os.TempDir(), "jianartifact-mihomo")
	if m.dataDir != "" {
		baseTmp = filepath.Join(m.dataDir, "tmp", "mihomo")
	}
	workDir := filepath.Join(baseTmp, fmt.Sprintf("proxy-%d", proxyID))
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("upstream: 创建 Mihomo 工作目录失败：%w", err)
	}
	cfgPath := filepath.Join(workDir, "config.yaml")
	if err := os.WriteFile(cfgPath, yamlBytes, 0o600); err != nil {
		return nil, fmt.Errorf("upstream: 写入 Mihomo 配置失败：%w", err)
	}

	stderr := newTailBuffer(mihomoStderrTailBytes)
	cmd := exec.Command(bin, "-d", workDir, "-f", cfgPath)
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	// 最小化环境变量：默认继承 os.Environ() 会把 JIAN_JWT_SECRET、
	// JIAN_OUTBOUND_PROXY_KEY、JIAN_UPSTREAM_CREDENTIAL_* 等本进程密钥一并交给
	// 一个第三方（GPL）二进制。这里只给 mihomo 运行必需的几项。
	cmd.Env = minimalMihomoEnv(workDir)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("upstream: 启动 mihomo 进程失败：%w", err)
	}
	inst := &managedMihomo{
		configHash: configHash,
		port:       port,
		workDir:    workDir,
		cmd:        cmd,
		done:       make(chan struct{}),
		stderr:     stderr,
	}
	go func() {
		_ = cmd.Wait()
		close(inst.done)
	}()

	if err := waitPortReady(ctx, port, inst.done); err != nil {
		detail := sanitizeMihomoStderr(stderr.String())
		m.stopInstance(inst)
		if detail != "" {
			return nil, fmt.Errorf("upstream: mihomo 本地代理未能就绪（%s）：%w", detail, err)
		}
		return nil, fmt.Errorf("upstream: mihomo 本地代理未能就绪：%w", err)
	}

	socksURL := (&url.URL{
		Scheme: "socks5h",
		User:   url.UserPassword(authUser, authPass),
		Host:   fmt.Sprintf("127.0.0.1:%d", port),
	}).String()
	tr, err := NewProxyTransport("socks5", socksURL)
	if err != nil {
		m.stopInstance(inst)
		return nil, err
	}
	inst.transport = tr
	m.mu.Lock()
	// 覆盖前先回收旧实例（并发下可能已被其它路径启动）。
	if old, ok := m.instances[proxyID]; ok {
		m.stopInstance(old)
	}
	m.instances[proxyID] = inst
	m.mu.Unlock()
	return tr, nil
}

// minimalMihomoEnv 构造交给 mihomo 的最小环境变量集合。
//
// 刻意不透传 os.Environ()：本进程的密钥（JWT、出站代理主密钥、上游凭据）不该出现在
// 第三方子进程的环境里（子进程可被 /proc/<pid>/environ 读取，核心转储也会带出）。
// HOME/TMPDIR 指向本次工作目录，避免 mihomo 把缓存写到数据目录之外或复用他人状态。
func minimalMihomoEnv(workDir string) []string {
	env := []string{
		"HOME=" + workDir,
		"TMPDIR=" + workDir,
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	if os.Getenv("SystemRoot") != "" {
		// Windows 下 Go 运行时与部分库依赖 SystemRoot。
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	return env
}

// sanitizeMihomoStderr 把子进程 stderr 压成单行诊断片段并裁短。
//
// 两个目的：① mihomo 的启动期解析错误常回显配置片段（如 proxies[0].password: ...），
// 这段文本最终会进入 API 响应，必须裁剪到固定长度且保留可读性；
// ② 去掉换行避免日志注入（攻击者可控的节点名会把伪造的日志行塞进服务端日志）。
// 凭据本身已经在 RenderMihomoRuntimeYAML 里以随机口令生成，不会出现管理员的原值，
// 但节点名等字段仍来自用户输入，仍按不可信处理。
func sanitizeMihomoStderr(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	// 折叠所有换行与制表为单空格，并压掉连续空白。
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if len(cleaned) > 240 {
		cleaned = cleaned[:240]
	}
	return cleaned
}

// Invalidate 停止指定代理 ID 的受管 mihomo 子进程并删除其临时配置目录。
func (m *MihomoManager) Invalidate(proxyID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(proxyID)
}

// Close 停止全部受管 mihomo 子进程并清理临时配置目录（优雅停机调用）。
func (m *MihomoManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.instances {
		m.cleanupLocked(id)
	}
}

// CleanupStaleWorkDirs 删除上次运行遗留的 mihomo 工作目录（含明文节点配置）。
//
// 为什么需要：正常路径由 stopInstance 删除工作目录，但 SIGKILL、panic、断电，
// 以及旧版本在一键重启时不回收子进程，都会留下 config.yaml（0600，含 UUID/口令/公钥）。
// 这些文件不在 storage-cleanup 的扫描范围内，会一直累积。启动期扫描是安全的窗口：
// 此时本进程尚未启动任何子进程（serving 前调用），目录里的一切都不属于任何存活实例。
// 返回删除的目录数，便于启动日志说明。
func (m *MihomoManager) CleanupStaleWorkDirs() int {
	if m == nil {
		return 0
	}
	baseTmp := filepath.Join(os.TempDir(), "jianartifact-mihomo")
	if m.dataDir != "" {
		baseTmp = filepath.Join(m.dataDir, "tmp", "mihomo")
	}
	entries, err := os.ReadDir(baseTmp)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "proxy-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(baseTmp, entry.Name())); err == nil {
			removed++
		}
	}
	return removed
}

func (m *MihomoManager) cleanupLocked(proxyID int64) {
	inst, ok := m.instances[proxyID]
	if !ok {
		return
	}
	delete(m.instances, proxyID)
	m.stopInstance(inst)
}

func (m *MihomoManager) stopInstance(inst *managedMihomo) {
	if inst.transport != nil {
		inst.transport.CloseIdleConnections()
	}
	if inst.cmd != nil && inst.cmd.Process != nil {
		_ = inst.cmd.Process.Kill()
		select {
		case <-inst.done:
		case <-time.After(2 * time.Second):
		}
	}
	if inst.workDir != "" {
		_ = os.RemoveAll(inst.workDir)
	}
}

func pickFreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return 0, errors.New("无法获取分配的本地端口")
	}
	return addr.Port, nil
}

func randomHexSecret(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func waitPortReady(ctx context.Context, port int, done <-chan struct{}) error {
	deadline := time.Now().Add(5 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return errors.New("mihomo 进程提前退出")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("tcp4", addr, 150*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-done:
			return errors.New("mihomo 进程提前退出")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("等待本地监听端口超时")
}
