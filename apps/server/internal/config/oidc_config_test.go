package config

import (
	"strings"
	"testing"
)

// TestOIDCDisabledWhenIssuerMissing 未配置 issuer 时整组不启用，且不得报错（存在即启用的先例）。
func TestOIDCDisabledWhenIssuerMissing(t *testing.T) {
	for _, env := range []string{
		EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecret,
		EnvOIDCRedirectURL, EnvOIDCAllowedDomains, EnvOIDCUsernameClaim,
	} {
		t.Setenv(env, "")
	}
	cfg, err := oidcConfig()
	if err != nil {
		t.Fatalf("未配置 issuer 不应报错：%v", err)
	}
	if cfg.Enabled() {
		t.Fatal("未配置 issuer 时不应启用 OIDC")
	}
}

// TestOIDCFullConfigParsed 完整配置解析：缺省 claim 生效，允许域名归一为小写并剔除空项。
func TestOIDCFullConfigParsed(t *testing.T) {
	t.Setenv(EnvOIDCIssuer, "https://idp.example.com")
	t.Setenv(EnvOIDCClientID, "jianartifact")
	t.Setenv(EnvOIDCClientSecret, "secret-value")
	t.Setenv(EnvOIDCRedirectURL, "https://repo.example.com/api/v1/auth/oidc/callback")
	t.Setenv(EnvOIDCAllowedDomains, " Example.COM , ,corp.example.com ")
	t.Setenv(EnvOIDCUsernameClaim, "")

	cfg, err := oidcConfig()
	if err != nil {
		t.Fatalf("完整配置不应报错：%v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("配置了 issuer 即应启用")
	}
	if cfg.UsernameClaim != defaultOIDCUsernameClaim {
		t.Fatalf("缺省用户名 claim 不符：%q", cfg.UsernameClaim)
	}
	if len(cfg.AllowedDomains) != 2 || cfg.AllowedDomains[0] != "example.com" || cfg.AllowedDomains[1] != "corp.example.com" {
		t.Fatalf("允许域名应归一为小写并剔除空项：%v", cfg.AllowedDomains)
	}
}

// TestOIDCHalfConfigRejected 半配置与非法 URL 必须在启动期直接报错，不静默半启用。
func TestOIDCHalfConfigRejected(t *testing.T) {
	t.Setenv(EnvOIDCIssuer, "https://idp.example.com")
	t.Setenv(EnvOIDCClientID, "")
	t.Setenv(EnvOIDCClientSecret, "")
	t.Setenv(EnvOIDCRedirectURL, "")
	_, err := oidcConfig()
	if err == nil {
		t.Fatal("半配置应报错")
	}
	if !strings.Contains(err.Error(), EnvOIDCClientID) || !strings.Contains(err.Error(), EnvOIDCClientSecret) || !strings.Contains(err.Error(), EnvOIDCRedirectURL) {
		t.Fatalf("错误信息应列出全部缺失项：%v", err)
	}

	t.Setenv(EnvOIDCClientID, "jianartifact")
	t.Setenv(EnvOIDCClientSecret, "secret-value")
	t.Setenv(EnvOIDCRedirectURL, "ftp://repo.example.com/callback")
	if _, err := oidcConfig(); err == nil {
		t.Fatal("非 http(s) 回调地址应报错")
	}

	t.Setenv(EnvOIDCIssuer, "idp.example.com")
	t.Setenv(EnvOIDCRedirectURL, "https://repo.example.com/callback")
	if _, err := oidcConfig(); err == nil {
		t.Fatal("非绝对 URL 的 issuer 应报错")
	}
}
