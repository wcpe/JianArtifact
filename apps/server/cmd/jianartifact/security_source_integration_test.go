package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func newSourceTestApplication(t *testing.T, formats string) (*config.Config, *appServices) {
	t.Helper()
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(config.EnvJWTSecret, "source-integration-test-key-only")
	t.Setenv(config.EnvEnabledFormats, formats)
	t.Setenv(config.EnvTrustedProxyCIDRs, "192.0.2.0/24")
	t.Setenv(config.EnvPublicURL, "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置：%v", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		t.Fatalf("装配服务：%v", err)
	}
	// 本夹具覆盖审计/协议链路，管理员强认证门由 httpserver 端到端验收；
	// 关掉签发门避免夹具管理员登录被 claim_required 拦截，同时摘掉管理面
	// 二次验证（requireRecentMfa 在服务未装配时降级放行）。
	svc.authSvc.SetMfaService(nil)
	svc.mfaSvc = nil
	t.Cleanup(func() { _ = svc.db.Close() })
	return cfg, svc
}

// 使用本地 HTTP 连接；只在测试服务器入口用文档地址模拟连接对端，不从请求头获取对端。
func newSourceTestServer(t *testing.T, handler http.Handler, peer string, tls bool) *httptest.Server {
	t.Helper()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = peer
		handler.ServeHTTP(w, r)
	})
	server := httptest.NewUnstartedServer(wrapped)
	if tls {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

func sourceTestRequest(t *testing.T, server *httptest.Server, method, path, chain, scheme string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "registry.example.test"
	req.Header.Set("X-Request-ID", "test-client-request-id")
	if chain != "" {
		req.Header.Set("X-Forwarded-For", chain)
	}
	if scheme != "" {
		req.Header.Set("X-Forwarded-Proto", scheme)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestSecuritySourceApplicationStoresAuditAndDownloadIP(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	if _, err := svc.repoSvc.Create("source-raw", "raw", "hosted", "public", "", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.assetSvc.Put("source-raw", "item.txt", strings.NewReader("source test"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	handler := newApplicationHandler(cfg, svc, nil)
	for _, tc := range []struct{ name, peer, chain, wantIP string }{
		{"可信尾段的首个客户端", "192.0.2.10:4321", "203.0.113.21, 192.0.2.11", "203.0.113.21"},
		{"同一代理的另一个客户端", "192.0.2.10:4321", "203.0.113.22", "203.0.113.22"},
		{"不受信连接只取直连地址", "198.51.100.30:4321", "203.0.113.23", "198.51.100.30"},
		{"可信对端缺少有效转发链", "192.0.2.10:4321", "192.0.2.11", ""},
		{"未知对端不采用报文头", "invalid-peer", "203.0.113.24", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSourceTestServer(t, handler, tc.peer, true)
			auditResp, _ := sourceTestRequest(t, server, http.MethodPost, "/api/v1/repositories", tc.chain, "https")
			if auditResp.StatusCode != http.StatusUnauthorized && auditResp.StatusCode != http.StatusForbidden {
				t.Fatalf("未认证管理写状态 = %d", auditResp.StatusCode)
			}
			id := auditResp.Header.Get("X-Request-ID")
			if id == "" || id == "test-client-request-id" {
				t.Fatalf("响应没有生成独立的服务端请求 ID：%q", id)
			}
			var ip, requestID string
			if err := svc.db.QueryRow(`SELECT ip, request_id FROM audit_log WHERE request_id = ?`, id).Scan(&ip, &requestID); err != nil {
				t.Fatalf("按响应请求 ID 查询旧审计：%v", err)
			}
			if ip != tc.wantIP || requestID != id {
				t.Errorf("审计来源 = %q / %q，应为 %q / %q", ip, requestID, tc.wantIP, id)
			}
			downloadResp, body := sourceTestRequest(t, server, http.MethodGet, "/repository/source-raw/item.txt", tc.chain, "https")
			if downloadResp.StatusCode != http.StatusOK || string(body) != "source test" {
				t.Fatalf("下载状态 = %d，内容 = %q", downloadResp.StatusCode, body)
			}
		})
	}
	if err := svc.assetDownloadSvc.Flush(time.Now()); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.db.Query(`SELECT client_ip, SUM(download_count) FROM asset_download_minutes WHERE repo = ? AND asset_path = ? GROUP BY client_ip`, "source-raw", "item.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int{}
	for rows.Next() {
		var ip string
		var count int
		if err := rows.Scan(&ip, &count); err != nil {
			t.Fatal(err)
		}
		got[ip] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"203.0.113.21": 1, "203.0.113.22": 1, "198.51.100.30": 1, "": 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("下载来源计数 = %v，应为 %v", got, want)
	}
}

func TestSecuritySourceApplicationCargoOrigin(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "cargo")
	if _, err := svc.repoSvc.Create("source-cargo", "cargo", "hosted", "public", "", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, peer, chain, scheme, publicURL, wantBase string
		tls                                            bool
	}{
		{name: "直连只依据实际传输", peer: "198.51.100.30:4321", scheme: "https", wantBase: "http://registry.example.test"},
		{name: "可信代理的协议", peer: "192.0.2.10:4321", chain: "203.0.113.21", scheme: "https", wantBase: "https://registry.example.test"},
		{name: "无效链不提供协议", peer: "192.0.2.10:4321", chain: "192.0.2.11", scheme: "https", wantBase: "http://registry.example.test"},
		{name: "可信链缺少协议时保留实际传输", peer: "192.0.2.10:4321", chain: "203.0.113.21", wantBase: "http://registry.example.test"},
		{name: "直接 TLS 不由报文头降级", peer: "198.51.100.30:4321", scheme: "http", wantBase: "https://registry.example.test", tls: true},
		{name: "配置优先", peer: "192.0.2.10:4321", chain: "203.0.113.21", scheme: "https", publicURL: "https://public.example.test", wantBase: "https://public.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.PublicURL = tc.publicURL
			server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), tc.peer, tc.tls)
			resp, body := sourceTestRequest(t, server, http.MethodGet, "/cargo/source-cargo/config.json", tc.chain, tc.scheme)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Cargo 配置状态 = %d，响应 = %s", resp.StatusCode, body)
			}
			var conf struct{ API, DL string }
			if err := json.Unmarshal(body, &conf); err != nil {
				t.Fatal(err)
			}
			wantAPI := tc.wantBase + "/cargo/source-cargo"
			if conf.API != wantAPI || conf.DL != wantAPI+"/api/v1/crates/{crate}/{version}/download" {
				t.Errorf("Cargo api/dl = %q / %q，期望基于 %q", conf.API, conf.DL, wantAPI)
			}
		})
	}
}
