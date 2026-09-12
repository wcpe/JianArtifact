package runner

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestOnlineRequestErrorRedactsExternalAddress(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "DNS 解析失败",
			err:  &net.DNSError{Name: "nexus-secret.example", Err: "没有此主机"},
			want: "Nexus 来源域名解析失败",
		},
		{
			name: "拨号失败",
			err: &net.OpError{
				Op:   "dial",
				Net:  "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.77"), Port: 8443},
				Err:  errors.New("连接被拒绝"),
			},
			want: "Nexus 来源网络连接失败",
		},
		{
			name: "TLS 握手失败",
			err:  &tls.RecordHeaderError{Msg: "TLS 首记录无效"},
			want: "Nexus 来源 TLS 握手失败",
		},
		{
			name: "普通网络失败",
			err:  errors.New("Get http://nexus-secret.example:8081: 网络异常"),
			want: "Nexus 来源网络请求失败",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := onlineRequestError(tt.err)
			if got.Error() != tt.want {
				t.Fatalf("错误分类 = %q，期望 %q", got, tt.want)
			}
			if strings.Contains(got.Error(), "nexus-secret.example") || strings.Contains(got.Error(), "203.0.113.77") {
				t.Fatalf("错误不得泄露来源地址：%q", got)
			}
		})
	}
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		download string
		want     bool
	}{
		{"同来源默认端口", "http://nexus.example", "http://nexus.example:80/file", true},
		{"协议不同", "http://nexus.example", "https://nexus.example/file", false},
		{"主机不同", "https://nexus.example", "https://other.example/file", false},
		{"端口不同", "https://nexus.example", "https://nexus.example:8443/file", false},
		{"相对下载地址", "https://nexus.example", "/file", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameOrigin(tt.source, tt.download); got != tt.want {
				t.Fatalf("sameOrigin(%q, %q) = %v，期望 %v", tt.source, tt.download, got, tt.want)
			}
		})
	}
}
