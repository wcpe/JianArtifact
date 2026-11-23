package config

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSecuritySourceCanonicalVersion(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.7:8080"
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	load := func(raw string) (string, string) {
		t.Helper()
		t.Setenv(EnvTrustedProxyCIDRs, raw)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		return cfg.SourcePolicy.Version(), cfg.SourcePolicy.Resolve(req, "server-id").ClientIP.String()
	}
	version, ip := load("192.0.2.7/24, 2001:db8::4/32")
	aliasVersion, aliasIP := load("2001:db8::/32,::ffff:192.0.2.7/120,192.0.2.0/25,192.0.2.7/24")
	changedVersion, _ := load("192.0.2.7/24")
	defaultVersion, defaultIP := load("")
	if version == "" || aliasVersion != version || changedVersion == version || defaultVersion == "" || defaultVersion == version {
		t.Fatalf("规范配置版本不符：%q、%q、%q、%q", version, aliasVersion, changedVersion, defaultVersion)
	}
	if ip != "198.51.100.2" || aliasIP != ip || defaultIP != "192.0.2.7" {
		t.Fatalf("配置未反映到来源策略：%q、%q、%q", ip, aliasIP, defaultIP)
	}
}

func TestLoadSecuritySourceRejectsInvalidCIDRs(t *testing.T) {
	for _, raw := range []string{"10.0.0.1", "10.0.0.0/8,", "10.0.0.0/8,,192.0.2.0/24", "fe80::%eth0/64", "::ffff:192.0.2.0/95"} {
		t.Run(raw, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "not-created")
			t.Setenv(EnvDataDir, dir)
			t.Setenv("JIAN_TRUSTED_PROXY_CIDRS", raw)
			cfg, err := Load()
			if cfg != nil || err == nil || !strings.Contains(err.Error(), "JIAN_TRUSTED_PROXY_CIDRS") {
				t.Fatalf("无效可信代理配置必须报错，cfg 是否非空 %v，错误 %v", cfg != nil, err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("无效可信代理配置不应创建数据目录：%v", err)
			}
		})
	}
}
