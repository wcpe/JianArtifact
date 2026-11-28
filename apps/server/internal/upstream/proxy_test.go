package upstream

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateStandardProxyURL(t *testing.T) {
	preview, norm, err := ValidateStandardProxyURL("http", "http://alice:secret@127.0.0.1:7890")
	if err != nil {
		t.Fatalf("合法 HTTP 代理被拒绝：%v", err)
	}
	if preview != "http://127.0.0.1:7890" {
		t.Fatalf("脱敏预览异常 = %q", preview)
	}
	if strings.Contains(preview, "secret") || strings.Contains(preview, "alice") {
		t.Fatalf("预览不得泄露凭据：%q", preview)
	}
	if !strings.Contains(norm, "alice:secret@") {
		t.Fatalf("规范化地址应保留认证信息以便建连：%q", norm)
	}

	preview, _, err = ValidateStandardProxyURL("socks5", "socks5h://user:pwd@10.0.0.2:7891")
	if err != nil {
		t.Fatalf("合法 SOCKS5 代理被拒绝：%v", err)
	}
	if preview != "socks5h://10.0.0.2:7891" {
		t.Fatalf("SOCKS5 预览异常 = %q", preview)
	}

	// 协议不匹配应拒绝
	if _, _, err := ValidateStandardProxyURL("socks5", "http://127.0.0.1:7890"); err == nil {
		t.Fatal("socks5 类型不应接受 http:// 地址")
	}
	if _, _, err := ValidateStandardProxyURL("http", "socks5://127.0.0.1:7891"); err == nil {
		t.Fatal("http 类型不应接受 socks5:// 地址")
	}
	if _, _, err := ValidateStandardProxyURL("http", "http://127.0.0.1:7890/path?x=1"); err == nil {
		t.Fatal("带路径/查询参数的代理地址应被拒绝")
	}
}

func TestClientDoRoutesViaHTTPProxyAndPreservesSSRFGuard(t *testing.T) {
	var proxyHit bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "via-proxy:"+r.URL.String())
	}))
	defer proxySrv.Close()

	tr, err := NewProxyTransport("http", proxySrv.URL)
	if err != nil {
		t.Fatalf("构造 HTTP 代理传输层失败：%v", err)
	}
	defer tr.CloseIdleConnections()

	c := NewClient(5 * time.Second)
	c.SetProxyResolver(func(_ context.Context, proxyID int64) (http.RoundTripper, error) {
		if proxyID == 42 {
			return tr, nil
		}
		return nil, ErrProxyUnavailable
	})

	// 1) 正常公网域名请求通过出站代理（即使本地不解析该假域名，也不受本地 DNS/Fake-IP 阻拦）。
	ctx := WithOutboundProxyID(context.Background(), 42)
	body, _, err := c.Fetch(ctx, "http://libraries.minecraft.example", "/com/mojang/logging/1.2.7/logging-1.2.7.jar")
	if err != nil {
		t.Fatalf("经出站代理回源失败：%v", err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if !proxyHit || !strings.Contains(string(data), "via-proxy:http://libraries.minecraft.example/com/mojang/logging/1.2.7/logging-1.2.7.jar") {
		t.Fatalf("响应未经过代理：%q", string(data))
	}

	// 2) 目标地址指向内网字面 IP 或 localhost 仍须被 SSRF 策略拦截。
	for _, bad := range []string{"http://127.0.0.1:8080", "http://169.254.169.254", "http://localhost:9000"} {
		if _, _, err := c.Fetch(ctx, bad, "/secret"); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("经代理访问内网目标 %q 应被拒绝，得 %v", bad, err)
		}
	}

	// 3) 未知 proxyID 不得静默降级直连。
	missingCtx := WithOutboundProxyID(context.Background(), 999)
	if _, _, err := c.Fetch(missingCtx, "http://libraries.minecraft.example", "/a"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("未知 proxyID 应返回 ErrProxyUnavailable，得 %v", err)
	}
}

func TestParseMihomoConfigShareURIsAndYAML(t *testing.T) {
	vmessJSON := `{"v":"2","ps":"hk-vmess","add":"hk.example.com","port":"443","id":"11111111-1111-1111-1111-111111111111","aid":"0","net":"ws","host":"cdn.example.com","path":"/ws","tls":"tls","sni":"cdn.example.com"}`
	vmessURI := "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessJSON))
	ssUser := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:super-secret-pass"))

	cases := []struct {
		name            string
		raw             string
		wantType        string
		wantPreviewPart string
		forbidInPreview string
	}{
		{
			name:            "VLESS Reality",
			raw:             "vless://11111111-1111-1111-1111-111111111111@hk.example.com:443?security=reality&sni=www.microsoft.com&fp=chrome&pbk=PUBKEY123&sid=abcd&flow=xtls-rprx-vision#HK",
			wantType:        "vless",
			wantPreviewPart: "vless://hk.example.com:443 (reality)",
			forbidInPreview: "PUBKEY123",
		},
		{
			name:            "VMess WS TLS",
			raw:             vmessURI,
			wantType:        "vmess",
			wantPreviewPart: "vmess://hk.example.com:443 (ws+tls)",
			forbidInPreview: "11111111",
		},
		{
			name:            "Trojan gRPC",
			raw:             "trojan://my-trojan-pwd@jp.example.com:443?sni=jp.example.com&type=grpc&serviceName=gun#JP",
			wantType:        "trojan",
			wantPreviewPart: "trojan://jp.example.com:443 (grpc)",
			forbidInPreview: "my-trojan-pwd",
		},
		{
			name:            "Shadowsocks SIP002",
			raw:             "ss://" + ssUser + "@sg.example.com:8388#SG",
			wantType:        "ss",
			wantPreviewPart: "ss://sg.example.com:8388",
			forbidInPreview: "super-secret-pass",
		},
		{
			name:            "Hysteria2 hy2 alias",
			raw:             "hy2://hy2-secret@us.example.com:8443?sni=us.example.com&obfs=salamander&obfs-password=obfspwd#US",
			wantType:        "hysteria2",
			wantPreviewPart: "hysteria2://us.example.com:8443",
			forbidInPreview: "hy2-secret",
		},
		{
			name:            "TUIC",
			raw:             "tuic://11111111-1111-1111-1111-111111111111:tuic-pass@eu.example.com:443?sni=eu.example.com&alpn=h3#EU",
			wantType:        "tuic",
			wantPreviewPart: "tuic://eu.example.com:443",
			forbidInPreview: "tuic-pass",
		},
		{
			name:            "AnyTLS",
			raw:             "anytls://anytls-secret@tw.example.com:443?sni=tw.example.com#TW",
			wantType:        "anytls",
			wantPreviewPart: "anytls://tw.example.com:443",
			forbidInPreview: "anytls-secret",
		},
		{
			name: "Mihomo YAML single node",
			raw: `name: hk-custom
type: vless
server: hk2.example.com
port: 443
uuid: secret-uuid
tls: true
network: ws`,
			wantType:        "vless",
			wantPreviewPart: "vless://hk2.example.com:443 (ws+tls)",
			forbidInPreview: "secret-uuid",
		},
		{
			name: "Mihomo YAML proxies list",
			raw: `proxies:
  - name: node-a
    type: hysteria2
    server: hy.example.com
    port: 443
    password: secret-hy2-password`,
			wantType:        "hysteria2",
			wantPreviewPart: "hysteria2://hy.example.com:443",
			forbidInPreview: "secret-hy2-password",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, preview, err := ParseMihomoConfig(tc.raw)
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if node["type"] != tc.wantType {
				t.Fatalf("type = %v, 期望 %s", node["type"], tc.wantType)
			}
			if node["name"] != "upstream-node" {
				t.Fatalf("节点名应被规范化为 upstream-node，得 %v", node["name"])
			}
			if preview != tc.wantPreviewPart {
				t.Fatalf("preview = %q, 期望 %q", preview, tc.wantPreviewPart)
			}
			if tc.forbidInPreview != "" && strings.Contains(preview, tc.forbidInPreview) {
				t.Fatalf("preview 不得包含敏感凭据 %q: %q", tc.forbidInPreview, preview)
			}
			rendered, err := RenderMihomoRuntimeYAML(node, 17890, "jian", "randpass")
			if err != nil {
				t.Fatalf("渲染运行时 YAML 失败：%v", err)
			}
			yamlText := string(rendered)
			if !strings.Contains(yamlText, "bind-address: 127.0.0.1") ||
				!strings.Contains(yamlText, "allow-lan: false") ||
				!strings.Contains(yamlText, "jian:randpass") {
				t.Fatalf("运行时 YAML 缺少安全沙箱字段：\n%s", yamlText)
			}
		})
	}
}

func TestParseMihomoConfigRejectsDisallowedType(t *testing.T) {
	if _, _, err := ParseMihomoConfig("type: external-controller\nserver: 127.0.0.1\nport: 9090"); err == nil {
		t.Fatal("非法协议类型应被拒绝")
	}
}
