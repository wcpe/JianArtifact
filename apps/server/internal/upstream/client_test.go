package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSetTimeout 验证 SetTimeout 后回源整体超时按新值生效（FR-89 回源超时动态配置）。
func TestSetTimeout(t *testing.T) {
	// 慢服务器：固定 200ms 后才响应。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewTestClient(DefaultTimeout)
	// 默认超时（30s）下慢服务器可完成。
	if _, _, err := c.Fetch(context.Background(), srv.URL, "/"); err != nil {
		t.Fatalf("默认超时下回源应成功：%v", err)
	}
	// SetTimeout 收紧到 50ms 后应超时。
	c.SetTimeout(50 * time.Millisecond)
	if _, _, err := c.Fetch(context.Background(), srv.URL, "/"); !IsTimeout(err) {
		t.Errorf("SetTimeout 后回源应超时，得 %v", err)
	}
	// SetTimeout(<=0) 回退默认超时：慢服务器恢复成功。
	c.SetTimeout(0)
	if _, _, err := c.Fetch(context.Background(), srv.URL, "/"); err != nil {
		t.Fatalf("SetTimeout(0) 应回退默认超时：%v", err)
	}
}

// TestClientConcurrentSetTimeoutAndFetch 并发 SetTimeout 与 Fetch 无数据竞争（-race 验证）。
func TestClientConcurrentSetTimeoutAndFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewTestClient(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = c.Fetch(context.Background(), srv.URL, "/")
		}()
		go func(n int) {
			defer wg.Done()
			c.SetTimeout(time.Duration(n+1) * time.Millisecond)
		}(i)
	}
	wg.Wait()
}

func TestFetchRejectsUnsafeUpstreamAddress(t *testing.T) {
	c := NewClient(time.Second)
	for _, baseURL := range []string{
		"ftp://packages.example.test",
		"https://user:secret@packages.example.test",
		"http://127.0.0.1",
		"http://10.0.0.1",
		"http://169.254.169.254",
		"http://[::1]",
	} {
		t.Run(baseURL, func(t *testing.T) {
			_, _, err := c.Fetch(context.Background(), baseURL, "/artifact")
			if !errors.Is(err, ErrUnsafeURL) {
				t.Fatalf("应拒绝不安全上游 %q，得 %v", baseURL, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("拒绝信息不得回显凭据：%v", err)
			}
		})
	}
}

func TestFetchRejectsPrivateDNSRecord(t *testing.T) {
	dialed := false
	c := newClient(time.Second, false, func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("不应拨号")
	})

	_, _, err := c.Fetch(context.Background(), "https://packages.example.test", "/artifact")
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("私网 DNS 记录应被拒绝，得 %v", err)
	}
	if dialed {
		t.Fatal("私网 DNS 记录不得触发拨号")
	}
}

func TestFetchChecksRedirectTarget(t *testing.T) {
	var redirected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			redirected = true
			http.Redirect(w, r, "http://127.0.0.1/metadata", http.StatusFound)
			return
		}
		t.Fatalf("不安全重定向不应发起第二跳请求：%s", r.URL.Path)
	}))
	defer srv.Close()

	c := newClient(time.Second, false, publicTestResolver, testDialer(srv.Listener.Addr().String()))
	_, _, err := c.Fetch(context.Background(), "http://packages.example.test", "/start")
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("应拒绝重定向到环回地址，得 %v", err)
	}
	if !redirected {
		t.Fatal("未到达首跳服务，测试未覆盖重定向")
	}
}

func TestFetchAllowsPublicAddressAndResolvesAgainBeforeDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lookups := 0
	resolver := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "packages.example.test" {
			t.Fatalf("意外解析主机：%s", host)
		}
		lookups++
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.9")}}, nil
	}
	c := newClient(time.Second, false, resolver, testDialer(srv.Listener.Addr().String()))
	body, _, err := c.Fetch(context.Background(), "http://packages.example.test", "/artifact")
	if err != nil {
		t.Fatalf("安全公网地址应可访问：%v", err)
	}
	_ = body.Close()
	if lookups < 2 {
		t.Fatalf("连接前应重新解析 DNS，实际解析 %d 次", lookups)
	}
}

func TestFetchRejectsDNSRebindingBeforeDial(t *testing.T) {
	lookups := 0
	resolver := func(_ context.Context, _ string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("203.0.113.9")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dialed := false
	c := newClient(time.Second, false, resolver, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("不应拨号")
	})

	_, _, err := c.Fetch(context.Background(), "http://packages.example.test", "/artifact")
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("DNS 重绑定到环回地址应被拒绝，得 %v", err)
	}
	if lookups != 2 {
		t.Fatalf("应在连接前第二次解析 DNS，实际 %d 次", lookups)
	}
	if dialed {
		t.Fatal("DNS 重绑定到不安全地址后不应拨号")
	}
}

func TestFetchWithCredentialRejectsUnsafeAddressWithoutLeakage(t *testing.T) {
	const credentialRef = "UNSAFE_UPSTREAM"
	const credential = "private-user:must-not-leak"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	c := NewClient(time.Second)
	_, _, err := c.FetchWithCredential(context.Background(), "http://127.0.0.1", "/artifact", credentialRef)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("带凭据的危险地址应被拒绝，得 %v", err)
	}
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("错误不得泄露凭据：%v", err)
	}
}

func TestFetchWithCredentialUsesBearerToken(t *testing.T) {
	const credentialRef = "BEARER_UPSTREAM"
	const credential = "private-bearer-token"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	body, _, err := NewTestClient(time.Second).FetchWithCredential(context.Background(), srv.URL, "/artifact", credentialRef)
	if err != nil {
		t.Fatalf("Bearer 凭据回源应成功：%v", err)
	}
	_ = body.Close()
}

func TestFetchWithCredentialStripsAuthorizationOnParentToChildRedirect(t *testing.T) {
	const credentialRef = "REDIRECT_BEARER"
	const credential = "must-not-reach-child"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	srv, client := newCrossOriginRedirectTestClient(t, func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
			t.Fatalf("首跳应携带 Bearer 凭据，得 %q", got)
		}
	})
	defer srv.Close()

	body, _, err := client.FetchWithCredential(context.Background(), "http://packages.example.test", "/start", credentialRef)
	if err != nil {
		t.Fatalf("跨域重定向回源应成功：%v", err)
	}
	_ = body.Close()
}

func TestFetchWithCredentialKeepsAuthorizationOnSameOriginRedirect(t *testing.T) {
	const credentialRef = "SAME_ORIGIN_BEARER"
	const credential = "kept-on-same-origin"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/target", http.StatusFound)
		case "/target":
			if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
				t.Fatalf("同 origin 重定向应保留凭据，得 %q", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("意外路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	body, _, err := NewTestClient(time.Second).FetchWithCredential(context.Background(), srv.URL, "/start", credentialRef)
	if err != nil {
		t.Fatalf("同 origin 重定向回源应成功：%v", err)
	}
	_ = body.Close()
}

func TestProbeWithCredentialStripsAuthorizationOnParentToChildRedirect(t *testing.T) {
	const credentialRef = "REDIRECT_BASIC"
	const credential = "release:must-not-reach-child"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	srv, client := newCrossOriginRedirectTestClient(t, func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Basic cmVsZWFzZTptdXN0LW5vdC1yZWFjaC1jaGlsZA==" {
			t.Fatalf("首跳应携带 Basic 凭据，得 %q", got)
		}
	})
	defer srv.Close()

	status, err := client.ProbeWithCredential(context.Background(), "http://packages.example.test/start", credentialRef)
	if err != nil {
		t.Fatalf("跨域重定向探测应成功：%v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("探测状态应为 200，得 %d", status)
	}
}

func TestDoStripsAuthorizationOnParentToChildRedirect(t *testing.T) {
	srv, client := newCrossOriginRedirectTestClient(t, func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer must-not-reach-child" {
			t.Fatalf("首跳应携带 Authorization，得 %q", got)
		}
	})
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://packages.example.test/start", nil)
	if err != nil {
		t.Fatalf("构造请求：%v", err)
	}
	req.Header.Set("Authorization", "Bearer must-not-reach-child")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("在线迁移使用 Do 的跨域重定向应成功：%v", err)
	}
	_ = resp.Body.Close()
}

func TestSessionReadAbsoluteSendsCredentialOnlyToConfiguredOrigin(t *testing.T) {
	const credentialRef = "SESSION_ORIGIN"
	const credential = "session-token"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	var configuredSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/configured":
			configuredSeen = true
			if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
				t.Fatalf("受配 origin 请求应携带凭据，得 %q", got)
			}
		case "/external":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Fatalf("非配置 origin 请求不得携带凭据，得 %q", got)
			}
		default:
			t.Fatalf("意外路径：%s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newClient(time.Second, false, publicTestResolver, testDialer(srv.Listener.Addr().String()))
	session, err := client.NewSession("http://packages.example.test/base", credentialRef)
	if err != nil {
		t.Fatalf("创建受控会话失败：%v", err)
	}

	body, _, err := session.ReadAbsolute(context.Background(), "http://packages.example.test/configured")
	if err != nil {
		t.Fatalf("配置 origin 读取失败：%v", err)
	}
	_ = body.Close()
	body, _, err = session.ReadAbsolute(context.Background(), "http://cdn.packages.example.test/external")
	if err != nil {
		t.Fatalf("非配置 origin 读取失败：%v", err)
	}
	_ = body.Close()
	if !configuredSeen {
		t.Fatal("未覆盖配置 origin 请求")
	}
}

func TestSessionReadAbsoluteStripsCredentialOnCrossOriginRedirect(t *testing.T) {
	const credentialRef = "SESSION_REDIRECT"
	const credential = "must-not-reach-redirect-target"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	var initialSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			initialSeen = true
			if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
				t.Fatalf("首跳应携带凭据，得 %q", got)
			}
			http.Redirect(w, r, "http://cdn.packages.example.test/target", http.StatusFound)
		case "/target":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Fatalf("跨 origin 重定向不得携带凭据，得 %q", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("意外路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := newClient(time.Second, false, publicTestResolver, testDialer(srv.Listener.Addr().String()))
	session, err := client.NewSession("http://packages.example.test", credentialRef)
	if err != nil {
		t.Fatalf("创建受控会话失败：%v", err)
	}
	body, _, err := session.ReadAbsolute(context.Background(), "http://packages.example.test/start")
	if err != nil {
		t.Fatalf("重定向读取失败：%v", err)
	}
	_ = body.Close()
	if !initialSeen {
		t.Fatal("未覆盖携带凭据的首跳请求")
	}
}

func TestSessionReadAbsoluteRejectsUnsafeURLWithoutLeakingSecrets(t *testing.T) {
	const credentialRef = "SESSION_SAFE"
	const credential = "private-user:must-not-leak"
	const unsafeURL = "http://release-user:private-password@127.0.0.1/private"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	session, err := NewTestClient(time.Second).NewSession("https://packages.example.test", credentialRef)
	if err != nil {
		t.Fatalf("创建受控会话失败：%v", err)
	}
	_, _, err = session.ReadAbsolute(context.Background(), unsafeURL)
	if !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("危险绝对地址应被拒绝，得 %v", err)
	}
	for _, secret := range []string{credential, unsafeURL, "private-password"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("错误不得泄露敏感信息 %q：%v", secret, err)
		}
	}
}

func newCrossOriginRedirectTestClient(t *testing.T, checkInitial func(*http.Request)) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			checkInitial(r)
			http.Redirect(w, r, "http://cdn.packages.example.test/target", http.StatusFound)
		case "/target":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Fatalf("跨 origin 重定向不得携带凭据，得 %q", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("意外路径：%s", r.URL.Path)
		}
	}))
	return srv, newClient(time.Second, false, publicTestResolver, testDialer(srv.Listener.Addr().String()))
}

func publicTestResolver(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("203.0.113.9")}}, nil
}

func testDialer(target string) dialContextFunc {
	return func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
}
