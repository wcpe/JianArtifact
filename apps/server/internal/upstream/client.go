// Package upstream 提供 proxy 仓库的回源 HTTP 客户端。
//
// 分层（见 internal/doc.go）：位于依赖链底部，与 blobstore/persistence 平级，
// 被 domain 层编排使用。仅负责按 baseURL+path 发起只读 GET 并流式返回响应体，
// 不含缓存、并发收敛或格式语义（这些在 domain 层）。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout 是回源请求的缺省整体超时。
const DefaultTimeout = 30 * time.Second

// ErrNotFound 表示上游明确返回 404：对 proxy 视为未命中、对 group 继续下一成员。
var ErrNotFound = errors.New("upstream: 上游资源不存在")

// ErrGone 表示上游明确返回 410：由需要保留该语义的协议映射为 410。
var ErrGone = errors.New("upstream: 上游资源已下架")

// ErrUnsafeURL 表示上游地址不符合统一出站安全策略。
var ErrUnsafeURL = errors.New("upstream: 上游地址不安全")

// ErrCredentialUnavailable 表示凭据引用未配置。错误不包含凭据值。
var ErrCredentialUnavailable = errors.New("upstream: 凭据引用不可用")

// StatusError 表示上游返回了非 200/404 的 HTTP 状态码。
// domain 层据此映射为 502（坏网关）。
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream: 上游返回状态码 %d", e.Code)
}

// Client 是回源 HTTP 客户端，持有带超时的 *http.Client。
// 超时可在运行时经 SetTimeout 调整（FR-89 回源超时动态配置）；
// 内部用 RWMutex 保护 http.Client 的替换，Fetch 与 SetTimeout 并发安全。
type Client struct {
	mu     sync.RWMutex
	http   *http.Client
	policy outboundPolicy
}

type lookupIPAddrFunc func(context.Context, string) ([]net.IPAddr, error)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

type outboundPolicy struct {
	allowPrivate bool
	lookupIPAddr lookupIPAddrFunc
	dialContext  dialContextFunc
}

// ipv4FirstDialer 固定走 IPv4（tcp4）的 Dialer：规避部分宿主无 IPv6 出口、
// 而上游 DNS 返回 AAAA 优先导致"先试 IPv6 失败再回退 IPv4"的额外延迟。
// 通用 Maven 仓库回源场景下，制品上游几乎都支持 IPv4，固定 tcp4 是稳的。
var ipv4FirstDialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// newUpstreamHTTP 构造带 IPv4 优先 Dialer 的 *http.Client。
func newUpstreamHTTP(timeout time.Duration, policy outboundPolicy) *http.Client {
	transport := &http.Transport{
		DialContext:           policy.secureDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: policy.checkRedirect,
	}
}

// NewClient 构造回源客户端；timeout<=0 时取 DefaultTimeout。
func NewClient(timeout time.Duration) *Client {
	return newClient(timeout, false, nil, nil)
}

// NewTestClient 构造仅供测试使用的回源客户端，允许访问本地回环测试服务器。
// 生产代码必须使用 NewClient，以确保所有出站请求执行地址安全校验。
func NewTestClient(timeout time.Duration) *Client {
	return newClient(timeout, true, nil, nil)
}

// NewClientWithPolicy 构造回源客户端并显式指定是否放行回环/私网地址。
//
// 仅用于用户显式声明的可信内网站来源（如本机或内网 Nexus）：allowPrivate=true 会绕过
// SSRF 私网地址校验，因此调用方必须确保该来源由用户在迁移任务中显式声明（allowPrivateSource），
// 且绝不用于来自不可信输入的地址。生产默认使用 NewClient（allowPrivate=false）。
// 注意：此开关只影响在线迁移回源，不影响代理回源（proxy 仓库远程地址）的安全校验。
func NewClientWithPolicy(timeout time.Duration, allowPrivate bool) *Client {
	return newClient(timeout, allowPrivate, nil, nil)
}

func newClient(timeout time.Duration, allowPrivate bool, lookup lookupIPAddrFunc, dial dialContextFunc) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	if dial == nil {
		dial = ipv4FirstDialer.DialContext
	}
	policy := outboundPolicy{allowPrivate: allowPrivate, lookupIPAddr: lookup, dialContext: dial}
	return &Client{http: newUpstreamHTTP(timeout, policy), policy: policy}
}

// SetTimeout 运行时更新回源整体超时；timeout<=0 时取 DefaultTimeout。
func (c *Client) SetTimeout(timeout time.Duration) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c.mu.Lock()
	c.http = newUpstreamHTTP(timeout, c.policy)
	c.mu.Unlock()
}

// Timeout 返回当前回源整体超时。
func (c *Client) Timeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.http.Timeout
}

// Fetch 以 GET 拉取 baseURL 与 path 拼接后的资源，返回响应体（调用方负责关闭）与响应头。
// 上游 404 返回 ErrNotFound、410 返回 ErrGone；其余非 2xx 返回 *StatusError；传输 / 超时错误原样返回。
func (c *Client) Fetch(ctx context.Context, baseURL, path string) (io.ReadCloser, http.Header, error) {
	return c.fetch(ctx, baseURL, path, "")
}

// FetchWithCredential 按凭据引用拉取上游资源。环境值含冒号时使用 Basic，
// 其余值作为 Bearer 令牌；凭据值仅存在于当前请求头，绝不持久化或写入错误。
func (c *Client) FetchWithCredential(ctx context.Context, baseURL, path, credentialRef string) (io.ReadCloser, http.Header, error) {
	return c.fetch(ctx, baseURL, path, credentialRef)
}

func (c *Client) fetch(ctx context.Context, baseURL, path, credentialRef string) (io.ReadCloser, http.Header, error) {
	full, err := joinURL(baseURL, path)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream: 构造请求：%w", err)
	}
	if credentialRef != "" {
		credential, err := ResolveCredential(credentialRef)
		if err != nil {
			return nil, nil, err
		}
		applyCredential(req, credential)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	return readResponse(resp)
}

// ResolveCredential 从专用命名空间读取私有上游凭据；缺失时仅返回通用错误。
func ResolveCredential(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if !IsCredentialRef(ref) {
		return "", ErrCredentialUnavailable
	}
	credential, ok := os.LookupEnv("JIAN_UPSTREAM_CREDENTIAL_" + ref)
	if !ok || credential == "" {
		return "", ErrCredentialUnavailable
	}
	return credential, nil
}

// IsCredentialRef 仅接受专用环境变量命名空间中的逻辑名称。
func IsCredentialRef(ref string) bool {
	if ref == "" || len(ref) > 63 {
		return false
	}
	for i := range len(ref) {
		c := ref[i]
		if c >= 'A' && c <= 'Z' || c == '_' {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return ref[0] >= 'A' && ref[0] <= 'Z'
}

func applyCredential(req *http.Request, credential string) {
	if user, password, ok := strings.Cut(credential, ":"); ok {
		req.SetBasicAuth(user, password)
		return
	}
	req.Header.Set("Authorization", "Bearer "+credential)
}

// ProbeWithCredential 对上游根地址执行 HEAD 探测，并沿用与制品回源相同的安全策略。
// 返回 HTTP 状态码，调用方根据格式和可用性策略决定其是否表示连接失败。
func (c *Client) ProbeWithCredential(ctx context.Context, remoteURL, credentialRef string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, remoteURL, nil)
	if err != nil {
		return 0, fmt.Errorf("upstream: 构造探测请求：%w", err)
	}
	if credentialRef != "" {
		credential, err := ResolveCredential(credentialRef)
		if err != nil {
			return 0, err
		}
		applyCredential(req, credential)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// Do 执行已构造的上游请求。它在每个请求开始时校验目标地址；重定向由同一策略逐跳校验。
// 后续格式专用客户端注入显式凭据时必须复用此方法，不能绕过安全传输层。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: 请求为空", ErrUnsafeURL)
	}
	if err := c.policy.validateURL(req.Context(), req.URL); err != nil {
		return nil, err
	}
	c.mu.RLock()
	hc := c.http
	c.mu.RUnlock()
	return hc.Do(req)
}

// IsTimeout 报告 err 是否源于超时（context 截止或网络超时）。
// domain 层据此把回源超时映射为 504。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// joinURL 以 baseURL 为基址拼接相对 path，去重分隔斜杠。baseURL 必须为绝对 http/https URL。
func joinURL(baseURL, path string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("%w: 地址格式非法", ErrUnsafeURL)
	}
	if base.Scheme != "http" && base.Scheme != "https" || base.Host == "" || base.User != nil {
		return "", fmt.Errorf("%w: 地址格式非法", ErrUnsafeURL)
	}
	if path == "" {
		return base.String(), nil
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return base.String(), nil
}

func (p outboundPolicy) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && !sameOrigin(via[len(via)-1].URL, req.URL) {
		// net/http 会向子域复制 Authorization；上游跳转到 CDN 时必须明确剥离。
		req.Header.Del("Authorization")
	}
	return p.validateURL(req.Context(), req.URL)
}

// sameOrigin 只在协议、主机和有效端口均相同时返回 true。
func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) {
		return false
	}
	return strings.EqualFold(left.Hostname(), right.Hostname()) && effectivePort(left) == effectivePort(right)
}

func effectivePort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(target.Scheme, "http") {
		return "80"
	}
	return ""
}

func (p outboundPolicy) validateURL(ctx context.Context, target *url.URL) error {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		return fmt.Errorf("%w: 协议、主机或用户信息非法", ErrUnsafeURL)
	}
	_, err := p.resolvePublicIPs(ctx, target.Hostname())
	return err
}

func (p outboundPolicy) secureDialContext(ctx context.Context, _ string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: 连接地址非法", ErrUnsafeURL)
	}
	ips, err := p.resolvePublicIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		if ip.To4() == nil {
			continue
		}
		conn, dialErr := p.dialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("upstream: 上游未解析到可连接的 IPv4 地址")
}

func (p outboundPolicy) resolvePublicIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !p.allowPrivate && isUnsafeIP(ip) {
			return nil, fmt.Errorf("%w: 目标地址被禁止", ErrUnsafeURL)
		}
		return []net.IP{ip}, nil
	}
	ips, err := p.lookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("upstream: 解析上游主机失败：%w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("upstream: 上游主机未解析到地址")
	}
	resolved := make([]net.IP, 0, len(ips))
	for _, addr := range ips {
		if addr.IP == nil {
			continue
		}
		if !p.allowPrivate && isUnsafeIP(addr.IP) {
			return nil, fmt.Errorf("%w: 目标地址被禁止", ErrUnsafeURL)
		}
		resolved = append(resolved, addr.IP)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("upstream: 上游主机未解析到地址")
	}
	return resolved, nil
}

func isUnsafeIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil {
		if ipv4.IsLoopback() || ipv4.IsPrivate() || ipv4.IsLinkLocalUnicast() || ipv4.IsLinkLocalMulticast() || ipv4.IsMulticast() || ipv4.IsUnspecified() {
			return true
		}
		return isInCIDR(ipv4, "100.64.0.0/10") || isInCIDR(ipv4, "198.18.0.0/15")
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

func isInCIDR(ip net.IP, cidr string) bool {
	_, network, err := net.ParseCIDR(cidr)
	return err == nil && network.Contains(ip)
}
