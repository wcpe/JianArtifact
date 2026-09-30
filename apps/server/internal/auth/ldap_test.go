package auth

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// 说明：目录协议往返（检索 → 绑定）的覆盖交由**真实目录实机验收**完成
// （见 docs/specs/0.11.0-external-identity-providers.md §5）。
// 原因：可用的进程内 LDAP 服务端库在关停阶段会从自身 goroutine 触发空指针 panic，
// 且多实例并存时相互干扰（实测：单测通过、同包并行运行时失败），不适合作为
// 常驻回归测试的基座。此处只保留**确定性**用例：安全守卫与纯函数。

// dummyVerifier 构造不连接任何目录的校验器，用于覆盖"连接之前"的守卫逻辑。
func dummyVerifier(t *testing.T, mutate func(*config.LDAPConfig)) *LDAPVerifier {
	t.Helper()
	cfg := config.LDAPConfig{
		URL:        "ldap://127.0.0.1:1",
		BaseDN:     "ou=people,dc=example,dc=com",
		UserFilter: "(uid={username})",
		EmailAttr:  "mail",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	v, err := NewLDAPVerifier(cfg)
	if err != nil {
		t.Fatalf("构造目录校验器：%v", err)
	}
	return v
}

// TestLDAPRejectsEmptyPassword 空口令必须在连接目录之前就拒绝：
// LDAP 的匿名绑定在空口令下可能成功，放行即等于任何人可用任意用户名登录。
func TestLDAPRejectsEmptyPassword(t *testing.T) {
	v := dummyVerifier(t, nil)
	for _, pw := range []string{"", "   "} {
		if _, err := v.Authenticate(context.Background(), "alice", pw); !errors.Is(err, ErrLDAPAuthFailed) {
			t.Fatalf("空口令必须拒绝（password=%q），实际 %v", pw, err)
		}
	}
}

// TestLDAPDirectoryUnreachableFailsFast 目录不可达必须快速失败，不能把登录请求挂死。
func TestLDAPDirectoryUnreachableFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位端口：%v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 关闭后该端口上不会有目录

	v := dummyVerifier(t, func(c *config.LDAPConfig) { c.URL = "ldap://" + addr })
	start := time.Now()
	if _, err := v.Authenticate(context.Background(), "alice", "alice-pw"); !errors.Is(err, ErrLDAPAuthFailed) {
		t.Fatalf("目录不可达应认证失败：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("目录不可达时失败过慢：%v", elapsed)
	}
}

// TestFilterAttributeDerivation DN 模板模式依赖从过滤器推导属性名；
// 取不出合法属性名时必须拒绝（否则会拼出错误的 DN）。
func TestFilterAttributeDerivation(t *testing.T) {
	cases := map[string]string{
		"(uid={username})":                        "uid",
		"(&(objectClass=person)(uid={username}))": "uid",
		"(sAMAccountName={username})":             "sAMAccountName",
		"(mail={username})":                       "mail",
		"({username})":                            "", // 无属性名
		"(cn=someone)":                            "", // 无占位
		"(uid={username})(|(cn=x))":               "uid",
		"(uid evil={username})":                   "", // 非法字符
	}
	for filter, want := range cases {
		if got := filterAttribute(filter); got != want {
			t.Fatalf("filterAttribute(%q) = %q，期望 %q", filter, got, want)
		}
	}
}
