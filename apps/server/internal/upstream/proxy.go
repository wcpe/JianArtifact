package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ErrProxyUnavailable 表示目标仓库挂载的出站代理不可用、已禁用或未找到。
var ErrProxyUnavailable = errors.New("upstream: 出站代理不可用")

type outboundProxyIDContextKey struct{}

// WithOutboundProxyID 在请求上下文上绑定全局出站代理 ID；<=0 表示直连（不经代理）。
// 该绑定随 ctx 自动穿透 FetchWithCredential、ProbeWithCredential、Session（含 OCI/PyPI/NuGet）
// 与重定向各跳，确保同一代理仓库的所有回源与探测走同一条出口链路。
func WithOutboundProxyID(ctx context.Context, proxyID int64) context.Context {
	if proxyID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, outboundProxyIDContextKey{}, proxyID)
}

// OutboundProxyIDFromContext 从上下文读回绑定的出站代理 ID；未绑定返回 0。
func OutboundProxyIDFromContext(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	if id, ok := ctx.Value(outboundProxyIDContextKey{}).(int64); ok && id > 0 {
		return id
	}
	return 0
}

// ProxyTransportResolver 按出站代理 ID 解析对应的 HTTP 传输层（支持 HTTP/HTTPS、SOCKS5 与托管 Mihomo）。
type ProxyTransportResolver func(ctx context.Context, proxyID int64) (http.RoundTripper, error)

// ValidateStandardProxyURL 校验 HTTP/HTTPS 或 SOCKS5 代理地址，并返回脱敏预览（不含凭据）与规范化后的地址。
func ValidateStandardProxyURL(kind, raw string) (preview, normalized string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("代理地址不能为空")
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil || !u.IsAbs() || u.Host == "" {
		return "", "", errors.New("代理地址格式非法")
	}
	scheme := strings.ToLower(u.Scheme)
	switch kind {
	case "http":
		if scheme != "http" && scheme != "https" {
			return "", "", errors.New("HTTP 代理地址协议必须为 http:// 或 https://")
		}
	case "socks5":
		if scheme != "socks5" && scheme != "socks5h" {
			return "", "", errors.New("SOCKS5 代理地址协议必须为 socks5:// 或 socks5h://")
		}
	default:
		return "", "", fmt.Errorf("不支持的代理类型：%s", kind)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", errors.New("代理主机名不能为空")
	}
	portStr := u.Port()
	if portStr != "" {
		port, convErr := strconv.Atoi(portStr)
		if convErr != nil || port < 1 || port > 65535 {
			return "", "", errors.New("代理端口非法（须为 1-65535）")
		}
	} else {
		switch scheme {
		case "http":
			portStr = "80"
		case "https":
			portStr = "443"
		default:
			return "", "", errors.New("SOCKS5 代理地址必须显式指定端口")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", "", errors.New("代理地址不得包含路径、查询参数或片段")
	}
	u.Scheme = scheme
	u.Path = ""
	preview = scheme + "://" + net.JoinHostPort(host, portStr)
	return preview, u.String(), nil
}

// NewProxyTransport 为标准 HTTP/HTTPS 或 SOCKS5/SOCKS5H 代理构造 *http.Transport。
//
// SOCKS5 走 golang.org/x/net/proxy.SOCKS5 并将域名直接透传给代理端解析（等价 socks5h），
// 避免宿主本地 DNS 污染或 Clash Fake-IP（198.18.0.0/15）干扰远端上游域名。
func NewProxyTransport(kind, rawProxyURL string) (*http.Transport, error) {
	_, normalized, err := ValidateStandardProxyURL(kind, rawProxyURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return nil, err
	}
	baseDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	switch kind {
	case "http":
		return &http.Transport{
			Proxy:                 http.ProxyURL(u),
			DialContext:           baseDialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}, nil
	case "socks5":
		var auth *proxy.Auth
		if u.User != nil {
			pwd, _ := u.User.Password()
			auth = &proxy.Auth{
				User:     u.User.Username(),
				Password: pwd,
			}
		}
		socksDialer, err := proxy.SOCKS5("tcp", u.Host, auth, baseDialer)
		if err != nil {
			return nil, fmt.Errorf("upstream: 初始化 SOCKS5 代理失败：%w", err)
		}
		var dialCtx func(context.Context, string, string) (net.Conn, error)
		if cd, ok := socksDialer.(proxy.ContextDialer); ok {
			dialCtx = cd.DialContext
		} else {
			dialCtx = func(ctx context.Context, network, addr string) (net.Conn, error) {
				type result struct {
					conn net.Conn
					err  error
				}
				ch := make(chan result, 1)
				go func() {
					c, dErr := socksDialer.Dial(network, addr)
					ch <- result{c, dErr}
				}()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case res := <-ch:
					return res.conn, res.err
				}
			}
		}
		return &http.Transport{
			DialContext:           dialCtx,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}, nil
	default:
		return nil, fmt.Errorf("upstream: 不支持的代理类型 %q", kind)
	}
}

// validateProxyTargetURL 校验经出站代理访问的上游目标地址：
// 必须为无 userinfo 的 http/https 绝对地址，并拒绝 localhost/内网保留域名及内网字面 IP（防 SSRF）；
// 不在宿主本地执行 DNS 解析，使 DNS 解析交由远端代理完成（规避本地 DNS 污染与 Fake-IP 拦截）。
func (p outboundPolicy) validateProxyTargetURL(target *url.URL) error {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		return fmt.Errorf("%w: 协议、主机或用户信息非法", ErrUnsafeURL)
	}
	if p.allowPrivate {
		return nil
	}
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("%w: 主机名非法", ErrUnsafeURL)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isUnsafeIP(ip) {
			return fmt.Errorf("%w: 目标地址被禁止", ErrUnsafeURL)
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("%w: 目标地址被禁止", ErrUnsafeURL)
	}
	return nil
}

func (p outboundPolicy) checkProxyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && !sameOrigin(via[len(via)-1].URL, req.URL) {
		req.Header.Del("Authorization")
	}
	return p.validateProxyTargetURL(req.URL)
}
