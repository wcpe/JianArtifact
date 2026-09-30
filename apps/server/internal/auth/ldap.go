package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// ErrLDAPAuthFailed 表示目录侧认证未通过（用户不存在、口令错误或目录不可达统一归此）。
// 调用方对外应表现为与本地登录失败一致，不泄露账号是否存在。
var ErrLDAPAuthFailed = errors.New("LDAP 认证失败")

// ldapDialTimeout 是目录连接与单次读取的超时：目录不可达或不响应时必须快速失败，
// 不能把登录请求挂死。
const ldapDialTimeout = 8 * time.Second

// LDAPVerifier 执行 LDAP 目录侧认证：按配置检索（或按 DN 模板推导）用户 DN，
// 再以该用户凭据绑定验证口令。
type LDAPVerifier struct {
	cfg config.LDAPConfig
	tls *tls.Config
}

// NewLDAPVerifier 构造目录侧校验器；调用方保证 cfg.Enabled() 为真。
// 证书校验默认严格：仅允许通过 CAFile 指定自定义 CA，不提供跳过校验的开关。
func NewLDAPVerifier(cfg config.LDAPConfig) (*LDAPVerifier, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("读取 LDAP CA：%w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("LDAP CA 文件不含有效证书")
		}
		tlsCfg.RootCAs = pool
	}
	return &LDAPVerifier{cfg: cfg, tls: tlsCfg}, nil
}

// Authenticate 校验用户名与口令，返回目录侧身份（Subject 为用户 DN，稳定不变）。
func (v *LDAPVerifier) Authenticate(ctx context.Context, username, password string) (ExternalUser, error) {
	// 空口令一律拒绝：LDAP 的"未认证绑定"（anonymous bind）在空口令下也会成功，
	// 不拦住就等于允许任何人以任意用户名登录。
	if strings.TrimSpace(password) == "" {
		return ExternalUser{}, ErrLDAPAuthFailed
	}
	conn, err := v.dial()
	if err != nil {
		return ExternalUser{}, ErrLDAPAuthFailed
	}
	defer conn.Close()
	_ = ctx // 目录调用本身带超时（见 dial 的 Timeout），ctx 仅用于上层取消语义

	dn, email, err := v.resolveUserDN(conn, username)
	if err != nil {
		return ExternalUser{}, err
	}
	// 以用户本人凭据重新绑定：这一步才是真正的口令校验。
	if err := conn.Bind(dn, password); err != nil {
		return ExternalUser{}, ErrLDAPAuthFailed
	}
	return ExternalUser{Subject: dn, Username: username, Email: email}, nil
}

// dial 建立连接并按配置升级到 TLS（ldaps 直接加密；ldap 可选 StartTLS）。
func (v *LDAPVerifier) dial() (*ldap.Conn, error) {
	conn, err := ldap.DialURL(v.cfg.URL,
		ldap.DialWithTLSConfig(v.tls),
		ldap.DialWithDialer(&net.Dialer{Timeout: ldapDialTimeout}),
	)
	if err != nil {
		return nil, err
	}
	conn.SetTimeout(ldapDialTimeout)
	if v.cfg.StartTLS {
		if err := conn.StartTLS(v.tls); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// resolveUserDN 得到用户 DN 与邮箱：配置了服务账号则检索，否则按 DN 模板推导。
func (v *LDAPVerifier) resolveUserDN(conn *ldap.Conn, username string) (dn, email string, err error) {
	if v.cfg.BindDN == "" {
		// DN 模板模式：用过滤器里的属性名拼出 <attr>=<username>,<BaseDN>；不检索。
		attr := filterAttribute(v.cfg.UserFilter)
		if attr == "" {
			return "", "", ErrLDAPAuthFailed
		}
		return fmt.Sprintf("%s=%s,%s", attr, ldap.EscapeDN(username), v.cfg.BaseDN), "", nil
	}
	if err := conn.Bind(v.cfg.BindDN, v.cfg.BindPassword); err != nil {
		return "", "", ErrLDAPAuthFailed
	}
	filter := strings.ReplaceAll(v.cfg.UserFilter, "{username}", ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(
		v.cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 0, false,
		filter, []string{v.cfg.EmailAttr}, nil,
	))
	if err != nil {
		return "", "", ErrLDAPAuthFailed
	}
	if len(res.Entries) != 1 {
		// 命中多个视为不可信：不猜哪一个才是本人。
		return "", "", ErrLDAPAuthFailed
	}
	return res.Entries[0].DN, res.Entries[0].GetAttributeValue(v.cfg.EmailAttr), nil
}

// filterAttribute 取过滤器里 {username} 所在属性名（形如 "(uid={username})" → "uid"）。
// 取不到返回空串，调用方据此拒绝 DN 模板模式。
func filterAttribute(filter string) string {
	idx := strings.Index(filter, "={username}")
	if idx <= 0 {
		return ""
	}
	name := filter[:idx]
	if i := strings.LastIndexAny(name, "(|&!"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	for _, r := range name {
		if !(r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return name
}
