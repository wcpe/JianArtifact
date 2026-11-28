package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// fakeLookup 构造一个受控的 DNS 解析器，让 SSRF 判定可以在无网络环境下断言。
func fakeLookup(records map[string][]string) lookupIPAddrFunc {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		raw, ok := records[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		out := make([]net.IPAddr, 0, len(raw))
		for _, item := range raw {
			ip := net.ParseIP(item)
			if ip == nil {
				continue
			}
			out = append(out, net.IPAddr{IP: ip})
		}
		return out, nil
	}
}

// newPolicyWithLookup 构造只替换解析器的出站策略（allowPrivate=false，与生产一致）。
func newPolicyWithLookup(records map[string][]string) outboundPolicy {
	return outboundPolicy{
		allowPrivate: false,
		lookupIPAddr: fakeLookup(records),
		dialContext:  ipv4FirstDialer.DialContext,
	}
}

// TestProxyTargetRejectsHostnameResolvingToPrivateIP 是 B-1 回归测试：
// 经出站代理访问时，域名解析到内网必须被拒绝——挂载出站代理并不能让目标变可信，
// PyPI/NuGet 的 index 链接、Cargo 的 dl 模板、OCI 的 realm 都来自上游响应体。
func TestProxyTargetRejectsHostnameResolvingToPrivateIP(t *testing.T) {
	policy := newPolicyWithLookup(map[string][]string{
		"internal.corp.example": {"10.0.0.7"},
		"metadata.example":      {"169.254.169.254"},
		"loopback.example":      {"127.0.0.1"},
		"cgnat.example":         {"100.64.0.1"},
		"public.example":        {"93.184.216.34"},
	})

	bad := []string{
		"http://internal.corp.example/a",
		"http://metadata.example/latest/meta-data/",
		"http://loopback.example/secret",
		"http://cgnat.example/x",
	}
	for _, raw := range bad {
		target := mustParseURL(t, raw)
		if err := policy.validateProxyTargetURL(context.Background(), target); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("域名解析到内网的 %q 应被拒绝，得 %v", raw, err)
		}
	}

	if err := policy.validateProxyTargetURL(context.Background(), mustParseURL(t, "http://public.example/a")); err != nil {
		t.Fatalf("公网域名应放行，得 %v", err)
	}
}

// TestProxyTargetAllowsFakeIPRange 验证 Clash/Mihomo Fake-IP 兼容：
// 解析结果全部落在 198.18.0.0/15 时视为代理内核分配的占位地址，必须放行，
// 否则所有启用 Fake-IP 的部署都无法回源。
func TestProxyTargetAllowsFakeIPRange(t *testing.T) {
	policy := newPolicyWithLookup(map[string][]string{
		"fakeip.example":  {"198.18.0.5", "198.19.3.7"},
		"mixed.example":   {"198.18.0.5", "10.1.2.3"},
		"real198.example": {"198.18.0.5"},
	})

	if err := policy.validateProxyTargetURL(context.Background(), mustParseURL(t, "http://fakeip.example/a")); err != nil {
		t.Fatalf("纯 Fake-IP 结果应放行，得 %v", err)
	}
	// 混入真实内网即为夹带：不能因为出现了 Fake-IP 记录就整体放行。
	if err := policy.validateProxyTargetURL(context.Background(), mustParseURL(t, "http://mixed.example/a")); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("Fake-IP 夹带真实内网地址应被拒绝，得 %v", err)
	}
}

// TestProxyTargetAllowsUnresolvableHost 钉住「本地解析不到不等于不安全」这条边界：
// 经代理时真实解析发生在代理侧（socks5h 与 Fake-IP 都如此），本地 DNS 与代理侧
// 可能是两套命名空间。若把解析失败当成不安全，所有内网无外网 DNS 的部署都会不可用，
// 而这类部署正是挂载出站代理的主要场景。
func TestProxyTargetAllowsUnresolvableHost(t *testing.T) {
	policy := newPolicyWithLookup(map[string][]string{})
	if err := policy.validateProxyTargetURL(context.Background(), mustParseURL(t, "http://only-proxy-can-resolve.example/a")); err != nil {
		t.Fatalf("本地无法解析的主机应放行（交由代理侧解析），得 %v", err)
	}
}

// TestProxyTargetRejectsReservedHostnames 验证内网保留域名在解析之前就被拒绝。
func TestProxyTargetRejectsReservedHostnames(t *testing.T) {
	policy := newPolicyWithLookup(map[string][]string{})
	for _, raw := range []string{
		"http://localhost:9000/a",
		"http://svc.localhost/a",
		"http://printer.local/a",
		"http://metadata.google.internal/a",
	} {
		if err := policy.validateProxyTargetURL(context.Background(), mustParseURL(t, raw)); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("内网保留域名 %q 应被拒绝，得 %v", raw, err)
		}
	}
}

// TestProxyTargetRejectsUnsafeSchemeAndUserinfo 覆盖 URL 形状校验。
func TestProxyTargetRejectsUnsafeSchemeAndUserinfo(t *testing.T) {
	policy := newPolicyWithLookup(map[string][]string{})
	for _, raw := range []string{
		"ftp://public.example/a",
		"http://user:pass@public.example/a",
		"http:///nohost",
	} {
		if err := policy.validateProxyTargetURL(context.Background(), mustParseURL(t, raw)); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("非法形状 %q 应被拒绝，得 %v", raw, err)
		}
	}
}

// TestProxyRedirectStillValidatesEachHop 验证经代理的重定向逐跳校验仍然生效，
// 并且跨源时剥离 Authorization（避免上游跳 CDN 时把凭据带出去）。
func TestProxyRedirectStillValidatesEachHop(t *testing.T) {
	policy := newPolicyWithLookup(map[string][]string{
		"cdn.example":      {"93.184.216.34"},
		"internal.example": {"10.9.8.7"},
	})

	prev := &http.Request{URL: mustParseURL(t, "http://cdn.example/a")}
	prev.Header = http.Header{}
	next := &http.Request{URL: mustParseURL(t, "http://internal.example/b")}
	next.Header = http.Header{"Authorization": {"Bearer leaked"}}

	err := policy.checkProxyRedirect(next, []*http.Request{prev})
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("重定向到内网应被拒绝，得 %v", err)
	}
	// 即使被拒绝，凭据也已在判定前被剥离，不会留在请求上被后续误用。
	if got := next.Header.Get("Authorization"); got != "" {
		t.Fatalf("跨源重定向应剥离 Authorization，仍为 %q", got)
	}
}

// mustParseURL 是测试内的小工具：URL 必然可解析，失败即测试写错。
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("测试 URL 非法 %q：%v", raw, err)
	}
	return u
}

// TestProxyTransportDoesNotLeakCredentialsIntoHeaders 验证代理认证不进请求头：
// HTTP 代理凭据由 transport 层按连接生成，SOCKS5 走握手，都不该出现在发给目标的头里。
func TestProxyTransportDoesNotLeakCredentialsIntoHeaders(t *testing.T) {
	var gotProxyAuth string
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 目标请求（经 HTTP 代理转发后到达代理服务器）。
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		if r.Header.Get("Authorization") != "" {
			t.Errorf("目标请求不应携带 Authorization，得 %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxySrv.Close()

	tr, err := NewProxyTransport("http", proxySrv.URL)
	if err != nil {
		t.Fatalf("构造代理传输层失败：%v", err)
	}
	defer tr.CloseIdleConnections()

	c := NewClient(5 * time.Second)
	c.SetProxyResolver(func(_ context.Context, _ int64) (http.RoundTripper, error) { return tr, nil })
	ctx := WithOutboundProxyID(context.Background(), 7)
	body, _, err := c.Fetch(ctx, "http://public.example", "/a")
	if err != nil {
		t.Fatalf("经代理回源失败：%v", err)
	}
	_ = body.Close()
	if gotProxyAuth != "" {
		t.Fatalf("Proxy-Authorization 不应出现在目标请求头中，得 %q", gotProxyAuth)
	}
}
