package auditctx

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSecuritySourceResolve(t *testing.T) {
	policy, err := NewPolicy("10.0.0.0/8, 2001:db8:1::/48")
	if err != nil {
		t.Fatal(err)
	}
	const peer = "10.9.8.7"
	const peerPort = peer + ":12345"
	const client = "192.0.2.5"
	const chain = client + ", 2001:db8:1::1, 10.0.0.3"
	const eightHops = client + ",10.0.0.1,10.0.0.2,10.0.0.3,10.0.0.4,10.0.0.5,10.0.0.6,10.0.0.7"
	exactBudget := client + strings.Repeat(" ", 1024-len(client)-len("https"))
	for _, tc := range []struct {
		name, remote                     string
		xff, xfp                         []string
		tls, noPolicy                    bool
		wantTrust                        ProxyTrust
		wantPeer, wantClient, wantScheme string
		wantChain                        []string
	}{
		{name: "没有规则忽略所有转发", remote: peerPort, xff: []string{"bad", "conflicting"}, xfp: []string{"https"}, noPolicy: true, wantTrust: ProxyTrustDirect, wantPeer: peer, wantClient: peer, wantScheme: "http"},
		{name: "非受信连接忽略头", remote: "[::ffff:198.51.100.7]:1234", xff: []string{chain}, xfp: []string{"https"}, wantTrust: ProxyTrustDirect, wantPeer: "198.51.100.7", wantClient: "198.51.100.7", wantScheme: "http"},
		{name: "可信尾段与最近代理协议", remote: peerPort, xff: []string{"198.51.100.18, " + chain}, xfp: []string{" HTTPS "}, wantTrust: ProxyTrustTrusted, wantPeer: peer, wantClient: client, wantScheme: "https", wantChain: []string{client, "2001:db8:1::1", "10.0.0.3"}},
		{name: "映射地址和端口", remote: "[::ffff:10.9.8.7]:80", xff: []string{"[::ffff:192.0.2.5]:8181, [2001:db8:1::7]:99"}, wantTrust: ProxyTrustTrusted, wantPeer: peer, wantClient: client, wantChain: []string{client, "2001:db8:1::7"}},
		{name: "真实TLS优先", remote: peerPort, xff: []string{client}, xfp: []string{"http"}, tls: true, wantTrust: ProxyTrustTrusted, wantPeer: peer, wantClient: client, wantScheme: "https", wantChain: []string{client}},
		{name: "8跳恰好允许", remote: peerPort, xff: []string{eightHops}, wantTrust: ProxyTrustTrusted, wantPeer: peer, wantClient: client, wantChain: strings.Split(eightHops, ",")},
		{name: "超过8跳拒绝", remote: peerPort, xff: []string{client + "," + eightHops}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "恰好1KiB允许", remote: peerPort, xff: []string{exactBudget}, xfp: []string{"https"}, wantTrust: ProxyTrustTrusted, wantPeer: peer, wantClient: client, wantScheme: "https", wantChain: []string{client}},
		{name: "超过1KiB不部分采用", remote: peerPort, xff: []string{exactBudget + " "}, xfp: []string{"https"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "转发头重复", remote: peerPort, xff: []string{client, client}, xfp: []string{"https"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "协议头重复", remote: peerPort, xff: []string{client}, xfp: []string{"https", "https"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "协议头包含逗号", remote: peerPort, xff: []string{client}, xfp: []string{"https,http"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "非法协议", remote: peerPort, xff: []string{client}, xfp: []string{"wss"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "转发格式非法", remote: peerPort, xff: []string{"192.000.2.5"}, xfp: []string{"https"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "反代链空段", remote: peerPort, xff: []string{client + ",,10.0.0.1"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "无非代理终点", remote: peerPort, xff: []string{"10.0.0.1, 2001:db8:1::8"}, tls: true, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer, wantScheme: "https"},
		{name: "同义头不替代缺失的链", remote: peerPort, xfp: []string{"https"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "转发带zone", remote: peerPort, xff: []string{"[fe80::1%eth0]:80"}, wantTrust: ProxyTrustInvalidForwarding, wantPeer: peer},
		{name: "连接带zone", remote: "[fe80::1%eth0]:80", xff: []string{chain}, xfp: []string{"https"}, wantTrust: ProxyTrustUnknown},
		{name: "连接地址不可识别", remote: "unknown-peer", xff: []string{chain}, xfp: []string{"https"}, wantTrust: ProxyTrustUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			req.Header["X-Forwarded-For"] = tc.xff
			req.Header["X-Forwarded-Proto"] = tc.xfp
			req.Header.Set("Forwarded", "for=203.0.113.9;proto=https")
			req.Header.Set("X-Real-IP", "203.0.113.8")
			req.Header.Set("User-Agent", "\x00audit\xff\n"+strings.Repeat("界", 100))
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			p := policy
			if tc.noPolicy {
				p = Policy{}
			}
			got := p.Resolve(req, "server-id")
			if got.ProxyTrust != tc.wantTrust || got.PeerIP != testAddr(tc.wantPeer) || got.ClientIP != testAddr(tc.wantClient) || got.TrustedScheme != tc.wantScheme || got.RequestID != "server-id" || got.OriginRequestID != "" || got.SourceTrustVersion != p.Version() {
				t.Fatalf("来源不符：%+v", got)
			}
			var wantChain []netip.Addr
			for _, ip := range tc.wantChain {
				wantChain = append(wantChain, testAddr(ip))
			}
			if !slices.Equal(got.ForwardedChain, wantChain) {
				t.Fatalf("获准转发尾段不符：%v，期望 %v", got.ForwardedChain, wantChain)
			}
			if !utf8.ValidString(got.UserAgent) || len(got.UserAgent) > 256 || strings.ContainsFunc(got.UserAgent, unicode.IsControl) || !strings.HasPrefix(got.UserAgent, "audit") {
				t.Fatalf("UA 必须是有界的非权威文本：%q", got.UserAgent)
			}
			if req.RemoteAddr != tc.remote {
				t.Fatal("不得修改实际连接地址")
			}
		})
	}
}

func TestSecuritySourceContextAndSnapshot(t *testing.T) {
	ctx := context.Background()
	if got, ok := SourceFrom(ctx); ok || got.ClientIP.IsValid() || got.ProxyTrust != ProxyTrustUnknown {
		t.Fatalf("缺失来源不应伪造事实：%+v，存在 %v", got, ok)
	}
	ip := netip.MustParseAddr("192.0.2.5")
	original := Source{RequestID: "launch-id", ClientIP: ip, ProxyTrust: ProxyTrustTrusted, SourceTrustVersion: "config-version", ForwardedChain: []netip.Addr{ip}}
	ctx = WithSource(ctx, original)
	original.ForwardedChain[0] = netip.Addr{}
	got, ok := SourceFrom(ctx)
	if !ok || !slices.Equal(got.ForwardedChain, []netip.Addr{ip}) {
		t.Fatalf("应复制写入的来源：%+v", got)
	}
	got.ForwardedChain[0] = netip.Addr{}
	got, _ = SourceFrom(ctx)
	if got.ForwardedChain[0] != ip {
		t.Fatal("读取者不应能修改上下文里的来源")
	}
	want := SourceSnapshot{OriginRequestID: "launch-id", ClientIP: ip, ProxyTrust: ProxyTrustTrusted, SourceTrustVersion: "config-version"}
	if snap, ok := got.Snapshot(); !ok || snap != want || !snap.Valid() {
		t.Fatalf("发起快照不符：%+v，存在 %v", snap, ok)
	}
	for _, bad := range []Source{
		{OriginRequestID: "launch-id", ClientIP: ip, ProxyTrust: ProxyTrustTrusted, SourceTrustVersion: "config-version"},
		{RequestID: "launch-id", PeerIP: ip, ProxyTrust: ProxyTrustInvalidForwarding, SourceTrustVersion: "config-version"},
		{RequestID: "launch-id", ClientIP: netip.MustParseAddr("::ffff:192.0.2.5"), ProxyTrust: ProxyTrustDirect, SourceTrustVersion: "config-version"},
		{RequestID: "launch-id", ClientIP: ip, ProxyTrust: ProxyTrustDirect},
	} {
		if snap, ok := bad.Snapshot(); ok || snap != (SourceSnapshot{}) {
			t.Fatalf("不应固化未采集、无效或补造的来源：%+v", snap)
		}
	}
}

func testAddr(value string) netip.Addr {
	if value == "" {
		return netip.Addr{}
	}
	return netip.MustParseAddr(value)
}
