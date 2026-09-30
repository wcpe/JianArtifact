package auth

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	ldapserver "github.com/vjeantet/ldapserver"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
)

// 本文件是**目录协议级验收用例**：对进程内真 LDAP 服务端走完整的
// 「服务账号 bind → 检索用户 → 用户本人 bind」往返。
//
// 它默认**跳过**：所用服务端库在关停阶段会从其自身 goroutine 触发空指针 panic，
// 且同包多实例并存时相互干扰（实测：单独运行通过、与其他用例同跑失败），
// 因此不适合作为常驻回归测试；需要时用 JIAN_LDAP_PROTOCOL_TEST=1 显式开启，
// 其输出作为实机验收证据（见 docs/specs/0.11.0-external-identity-providers.md §5）。

func TestLDAPProtocolRoundTrip(t *testing.T) {
	if os.Getenv("JIAN_LDAP_PROTOCOL_TEST") != "1" {
		t.Skip("目录协议往返为验收用例，设 JIAN_LDAP_PROTOCOL_TEST=1 显式运行")
	}
	aliceDN := "uid=alice,ou=people,dc=example,dc=com"
	svcDN := "cn=svc,dc=example,dc=com"
	passwords := map[string]string{svcDN: "svc-secret", aliceDN: "alice-pw"}
	var sawSearch bool

	mux := ldapserver.NewRouteMux()
	mux.Bind(func(w ldapserver.ResponseWriter, m *ldapserver.Message) {
		req := m.GetBindRequest()
		dn, password := string(req.Name()), string(req.AuthenticationSimple())
		if password != "" && passwords[dn] == password {
			w.Write(ldapserver.NewBindResponse(ldapserver.LDAPResultSuccess))
			return
		}
		w.Write(ldapserver.NewBindResponse(ldapserver.LDAPResultInvalidCredentials))
	})
	mux.Search(func(w ldapserver.ResponseWriter, m *ldapserver.Message) {
		sawSearch = true
		w.Write(ldapserver.NewSearchResultEntry(aliceDN))
		w.Write(ldapserver.NewSearchResultDoneResponse(ldapserver.LDAPResultSuccess))
	})
	server := ldapserver.NewServer()
	server.Handler = mux
	go func() {
		defer func() { _ = recover() }()
		_ = server.ListenAndServe("127.0.0.1:0")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for server.Listener == nil {
		if time.Now().After(deadline) {
			t.Fatal("目录未在预期时间内就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}

	verifier, err := NewLDAPVerifier(config.LDAPConfig{
		URL:          "ldap://" + server.Listener.Addr().String(),
		BindDN:       svcDN,
		BindPassword: "svc-secret",
		BaseDN:       "ou=people,dc=example,dc=com",
		UserFilter:   "(uid={username})",
		EmailAttr:    "mail",
	})
	if err != nil {
		t.Fatalf("构造校验器：%v", err)
	}

	user, err := verifier.Authenticate(context.Background(), "alice", "alice-pw")
	if err != nil {
		t.Fatalf("目录往返应成功：%v", err)
	}
	if user.Subject != aliceDN {
		t.Fatalf("Subject 应为用户 DN：%q", user.Subject)
	}
	if !sawSearch {
		t.Fatal("检索模式必须实际发起检索")
	}
	t.Logf("验收证据：检索已发生=%v，绑定 DN=%s，Subject=%s", sawSearch, svcDN, user.Subject)

	// 负例：口令错误必须被拒（且与用户不存在的表现一致）。
	if _, err := verifier.Authenticate(context.Background(), "alice", "wrong-pw"); err == nil {
		t.Fatal("错误口令必须拒绝")
	}
	if _, err := verifier.Authenticate(context.Background(), "alice", ""); err == nil {
		t.Fatal("空口令必须拒绝")
	}
	if strings.TrimSpace(user.Username) == "" {
		t.Fatal("用户名不得为空")
	}
}
