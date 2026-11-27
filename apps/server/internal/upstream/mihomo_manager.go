package upstream

import (
	"bytes"
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
	exitErr    error
	transport  *http.Transport
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
}

// NewMihomoManager 构造 Mihomo 子进程管理器。
func NewMihomoManager(configuredBin, dataDir string) *MihomoManager {
	return &MihomoManager{
		configuredBin: strings.TrimSpace(configuredBin),
		dataDir:       strings.TrimSpace(dataDir),
		instances:     make(map[int64]*managedMihomo),
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
	version := ""
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 0 {
			version = strings.TrimSpace(lines[0])
		}
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
func (m *MihomoManager) TransportFor(ctx context.Context, proxyID int64, configHash string, node map[string]any) (*http.Transport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.instances[proxyID]; ok {
		select {
		case <-existing.done:
			// 子进程已异常退出：清理后重建。
			m.cleanupLocked(proxyID)
		default:
			if existing.configHash == configHash {
				return existing.transport, nil
			}
			m.cleanupLocked(proxyID)
		}
	}

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

	var stderrBuf bytes.Buffer
	cmd := exec.Command(bin, "-d", workDir, "-f", cfgPath)
	cmd.Stderr = &stderrBuf
	cmd.Stdout = &stderrBuf
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
	}
	go func() {
		inst.exitErr = cmd.Wait()
		close(inst.done)
	}()

	if err := waitPortReady(ctx, port, inst.done); err != nil {
		m.stopInstance(inst)
		detail := strings.TrimSpace(stderrBuf.String())
		if len(detail) > 240 {
			detail = detail[:240]
		}
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
	m.instances[proxyID] = inst
	return tr, nil
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
