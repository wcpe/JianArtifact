package config

import (
	"strings"
	"testing"
)

// TestLDAPDisabledWhenURLMissing 未配置 URL 时整组不启用，且不得报错（存在即启用的先例）。
func TestLDAPDisabledWhenURLMissing(t *testing.T) {
	for _, env := range []string{
		EnvLDAPURL, EnvLDAPBindDN, EnvLDAPBindPassword, EnvLDAPBaseDN,
		EnvLDAPUserFilter, EnvLDAPEmailAttr, EnvLDAPStartTLS, EnvLDAPCAFile,
	} {
		t.Setenv(env, "")
	}
	cfg, err := ldapConfig()
	if err != nil {
		t.Fatalf("未配置 URL 不应报错：%v", err)
	}
	if cfg.Enabled() {
		t.Fatal("未配置 URL 时不应启用 LDAP")
	}
}

// TestLDAPFullConfigParsed 完整配置解析：缺省过滤器与邮箱属性生效，StartTLS 按字面量解析。
func TestLDAPFullConfigParsed(t *testing.T) {
	t.Setenv(EnvLDAPURL, "ldaps://ldap.example.com:636")
	t.Setenv(EnvLDAPBindDN, "cn=svc,dc=example,dc=com")
	t.Setenv(EnvLDAPBindPassword, "svc-secret")
	t.Setenv(EnvLDAPBaseDN, "ou=people,dc=example,dc=com")
	t.Setenv(EnvLDAPUserFilter, "")
	t.Setenv(EnvLDAPEmailAttr, "")
	t.Setenv(EnvLDAPStartTLS, "")
	t.Setenv(EnvLDAPCAFile, "")

	cfg, err := ldapConfig()
	if err != nil {
		t.Fatalf("完整配置不应报错：%v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("配置了 URL 即应启用")
	}
	if cfg.UserFilter != defaultLDAPUserFilter || cfg.EmailAttr != defaultLDAPEmailAttr {
		t.Fatalf("缺省过滤器/邮箱属性不符：%q / %q", cfg.UserFilter, cfg.EmailAttr)
	}
	if cfg.StartTLS {
		t.Fatal("未显式开启时 StartTLS 应为关闭")
	}
}

// TestLDAPHalfConfigAndContradictionsRejected 半配置与自相矛盾的组合必须在启动期报错，
// 不静默半启用（对齐 OIDC 与 FR-131 的防静默降级口径）。
func TestLDAPHalfConfigAndContradictionsRejected(t *testing.T) {
	base := func() {
		t.Setenv(EnvLDAPURL, "ldap://ldap.example.com:389")
		t.Setenv(EnvLDAPBindDN, "")
		t.Setenv(EnvLDAPBindPassword, "")
		t.Setenv(EnvLDAPBaseDN, "ou=people,dc=example,dc=com")
		t.Setenv(EnvLDAPUserFilter, "(uid={username})")
		t.Setenv(EnvLDAPEmailAttr, "mail")
		t.Setenv(EnvLDAPStartTLS, "")
		t.Setenv(EnvLDAPCAFile, "")
	}

	// 非法协议
	base()
	t.Setenv(EnvLDAPURL, "https://ldap.example.com")
	if _, err := ldapConfig(); err == nil {
		t.Fatal("非 ldap/ldaps 协议应报错")
	}

	// 缺基准 DN
	base()
	t.Setenv(EnvLDAPBaseDN, "")
	if _, err := ldapConfig(); err == nil || !strings.Contains(err.Error(), EnvLDAPBaseDN) {
		t.Fatalf("缺基准 DN 应报错并点明变量：%v", err)
	}

	// 过滤器缺占位
	base()
	t.Setenv(EnvLDAPUserFilter, "(uid=someone)")
	if _, err := ldapConfig(); err == nil || !strings.Contains(err.Error(), EnvLDAPUserFilter) {
		t.Fatalf("过滤器缺占位应报错并点明变量：%v", err)
	}

	// 配了服务账号却没给口令
	base()
	t.Setenv(EnvLDAPBindDN, "cn=svc,dc=example,dc=com")
	if _, err := ldapConfig(); err == nil || !strings.Contains(err.Error(), EnvLDAPBindPassword) {
		t.Fatalf("服务账号缺口令应报错并点明变量：%v", err)
	}

	// StartTLS 与 ldaps 冲突
	base()
	t.Setenv(EnvLDAPURL, "ldaps://ldap.example.com:636")
	t.Setenv(EnvLDAPStartTLS, "true")
	if _, err := ldapConfig(); err == nil || !strings.Contains(err.Error(), EnvLDAPStartTLS) {
		t.Fatalf("StartTLS 与 ldaps 冲突应报错并点明变量：%v", err)
	}

	// CA 文件不可读
	base()
	t.Setenv(EnvLDAPCAFile, "no-such-ca-file.pem")
	if _, err := ldapConfig(); err == nil || !strings.Contains(err.Error(), EnvLDAPCAFile) {
		t.Fatalf("CA 不可读应报错并点明变量：%v", err)
	}
}
