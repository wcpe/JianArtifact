package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

const auditTestPassword = "only-a-test-password-53"

func startTestSecurityAudit(t *testing.T, svc *appServices) {
	t.Helper()
	run, cancel := context.WithCancel(context.Background())
	if err := svc.securityAudit.Start(run); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer cancel()
		ctx, deadline := context.WithTimeout(context.Background(), 5*time.Second)
		defer deadline()
		if err := svc.securityAudit.Close(ctx); err != nil {
			t.Errorf("关闭安全审计：%v", err)
		}
	})
}

func auditTestRequest(t *testing.T, server *httptest.Server, method, path, contentType, body, token string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Forwarded-For", "203.0.113.31, 192.0.2.11")
	req.Header.Set("X-Forwarded-Proto", "https")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("X-Request-ID"), string(data)
}

func TestSecurityAuditHTTPAuthAttempts(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	startTestSecurityAudit(t, svc)
	_, user, err := svc.authSvc.Bootstrap("audit-admin", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.userSvc.Create("audit-disabled", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.userSvc.Update(disabled.ID, "user", "disabled"); err != nil {
		t.Fatal(err)
	}
	server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), "192.0.2.10:4321", true)
	for _, tc := range []struct {
		name, username, password string
		status                   int
		actor                    int64
	}{
		{"错误口令", "audit-admin", "wrong-test-password-67", 401, 0},
		{"停用账号", "audit-disabled", auditTestPassword, 401, 0},
		{"未知账号", "audit-missing-marker", auditTestPassword, 401, 0},
		{"认证成功", "audit-admin", auditTestPassword, 200, user.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, requestID, _ := auditTestRequest(t, server, "POST", "/api/v1/auth/login", "application/json", fmt.Sprintf(`{"username":%q,"password":%q}`, tc.username, tc.password), "")
			if status != tc.status || requestID == "" {
				t.Fatalf("登录状态或请求号异常：%d / %q", status, requestID)
			}
			var actor sql.NullInt64
			var outcome, actorKind, actorName, authMethod, strength, credential, session, ip, code string
			var gotStatus sql.NullInt64
			deadline := time.Now().Add(time.Second)
			for {
				err := svc.db.QueryRow(`SELECT actor_user_id,outcome,actor_kind,actor_name_snapshot,auth_method,auth_strength,credential_id,session_id,hex(client_ip),error_code,http_status FROM security_event WHERE request_id = ? AND event_type = 'auth.attempt'`, requestID).Scan(&actor, &outcome, &actorKind, &actorName, &authMethod, &strength, &credential, &session, &ip, &code, &gotStatus)
				if err == nil {
					break
				}
				if err != sql.ErrNoRows || time.Now().After(deadline) {
					t.Fatalf("未找到真实请求的认证尝试：%v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if tc.actor == 0 && (actor.Valid || actorKind != "unknown" || outcome != "denied" || code != "invalid_credentials") {
				t.Fatalf("拒绝归属或结果异常：%+v / %s / %s / %s", actor, actorKind, outcome, code)
			}
			if tc.actor != 0 && (!actor.Valid || actor.Int64 != tc.actor || actorKind != "user" || outcome != "succeeded") {
				t.Fatalf("成功归属或结果异常：%+v / %s / %s", actor, actorKind, outcome)
			}
			if gotStatus.Valid || ip != "CB00711F" || actorName != "" || strength != "" || credential != "" || session != "" || authMethod != "password" {
				t.Fatalf("尝试快照字段异常：状态=%+v 来源=%s 方式=%s", gotStatus, ip, authMethod)
			}
		})
	}
	var created int
	if err := svc.db.Get(&created, `SELECT count(*) FROM security_event WHERE event_type = 'auth.session_created'`); err != nil || created != 0 {
		t.Fatalf("不应宣称建立持久会话：%d / %v", created, err)
	}
}

func TestSecurityAuditHTTPProviderLDAPAttribution(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, svc := newSourceTestApplication(t, "raw")
	startTestSecurityAudit(t, svc)
	verifier, err := auth.NewLDAPVerifier(config.LDAPConfig{URL: "ldap://" + address, UserFilter: "(uid={username})", BaseDN: "dc=example,dc=test"})
	if err != nil {
		t.Fatal(err)
	}
	svc.ldapAuth = verifier.Authenticate
	server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), "192.0.2.10:4321", true)
	for _, test := range []struct{ password, outcome, code string }{
		{"test-only-password", "failed", "internal_error"},
		{"  ", "denied", "invalid_credentials"},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			status, requestID, body := auditTestRequest(t, server, "POST", "/api/v1/auth/login", "application/json", fmt.Sprintf(`{"username":"test-only-user","password":%q}`, test.password), "")
			if status != 401 || !strings.Contains(body, `"invalid_credentials"`) || strings.Contains(body, address) {
				t.Fatalf("LDAP 对外失败语义改变：%d %s", status, body)
			}
			assertProviderAudit(t, svc, requestID, "ldap_bind", test.outcome, test.code)
		})
	}
}

func TestSecurityAuditHTTPProviderLDAPUserBind(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, code string
		result              int
		service             bool
	}{
		{"用户名拒绝", "denied", "invalid_credentials", ldap.LDAPResultInvalidCredentials, false},
		{"目录忙", "failed", "internal_error", ldap.LDAPResultBusy, false},
		{"目录不可用", "failed", "internal_error", ldap.LDAPResultUnavailable, false},
		{"绑定连接中断", "failed", "internal_error", -1, false},
		{"用户绑定原因不明", "unknown", "", ldap.LDAPResultOther, false},
		{"服务账号绑定仍为故障", "failed", "internal_error", ldap.LDAPResultOther, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, served := serveAuditTestLDAPBind(t, tc.result)
			cfg, svc := newSourceTestApplication(t, "raw")
			startTestSecurityAudit(t, svc)
			directory := config.LDAPConfig{URL: "ldap://" + address, UserFilter: "(uid={username})", BaseDN: "dc=example,dc=test"}
			if tc.service {
				directory.BindDN, directory.BindPassword = "cn=test-service,dc=example,dc=test", "test-service-password"
			}
			verifier, err := auth.NewLDAPVerifier(directory)
			if err != nil {
				t.Fatal(err)
			}
			svc.ldapAuth = verifier.Authenticate
			server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), "192.0.2.10:4321", true)
			status, requestID, body := auditTestRequest(t, server, "POST", "/api/v1/auth/login", "application/json", `{"username":"test-only-user","password":"test-only-password"}`, "")
			if err := <-served; err != nil {
				t.Fatalf("本机目录未处理 Bind：%v", err)
			}
			if status != 401 || !strings.Contains(body, `"invalid_credentials"`) || strings.Contains(body, "test-private-directory") {
				t.Fatalf("LDAP 对外失败语义改变：%d %s", status, body)
			}
			assertProviderAudit(t, svc, requestID, "ldap_bind", tc.outcome, tc.code)
		})
	}
}

// serveAuditTestLDAPBind 只处理一条本机 Bind，返回指定结果或在收到 Bind 后断开。
func serveAuditTestLDAPBind(t *testing.T, result int) (string, <-chan error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- func() error {
			conn, err := listener.Accept()
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				return err
			}
			var header [2]byte
			if _, err := io.ReadFull(conn, header[:]); err != nil {
				return err
			}
			if header[0] != 0x30 || header[1] >= 0x80 {
				return errors.New("本机测试目录只接受短 LDAP 序列")
			}
			packet := make([]byte, int(header[1])+2)
			copy(packet, header[:])
			if _, err := io.ReadFull(conn, packet[2:]); err != nil {
				return err
			}
			var request struct {
				ID        int
				Operation asn1.RawValue
			}
			if _, err := asn1.Unmarshal(packet, &request); err != nil {
				return err
			}
			if request.Operation.Class != asn1.ClassApplication || request.Operation.Tag != 0 {
				return errors.New("测试请求没有到达 LDAP Bind")
			}
			if result < 0 {
				return nil
			}
			type bindResult struct {
				Code           asn1.Enumerated
				DN, Diagnostic []byte
			}
			response, err := asn1.Marshal(struct {
				ID     int
				Result bindResult `asn1:"application,tag:1"`
			}{
				ID:     request.ID,
				Result: bindResult{Code: asn1.Enumerated(result), Diagnostic: []byte("test-private-directory: server unavailable")},
			})
			if err != nil {
				return err
			}
			_, err = conn.Write(response)
			return err
		}()
	}()
	return listener.Addr().String(), finished
}

func TestSecurityAuditHTTPProviderOIDCAttribution(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body, queryErr, redirect, outcome, code string
		status                                        int
		keys                                          bool
	}{
		{"发现故障", "", "", "verify_failed", "failed", "internal_error", 0, false},
		{"可信令牌端点故障", `{"error":"invalid_grant","error_description":"test-private-upstream"}`, "", "verify_failed", "failed", "internal_error", 503, false},
		{"明确拒绝授权码", `{"error":"invalid_grant","error_description":"test-private-upstream"}`, "", "verify_failed", "denied", "invalid_credentials", 400, false},
		{"不透明令牌响应", `{"error":"unknown_test_reason"}`, "", "verify_failed", "unknown", "", 400, false},
		{"不透明公钥校验", "", "", "verify_failed", "unknown", "", 200, true},
		{"未验证回调错误不能证明故障", "", "temporarily_unavailable", "provider_error", "denied", "unauthorized", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var providerCalls, keyCalls atomic.Int32
			idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				issuer := "http://" + r.Host
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					if test.status == 0 {
						w.WriteHeader(503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
				case "/token":
					w.WriteHeader(test.status)
					if !test.keys {
						_, _ = io.WriteString(w, test.body)
						return
					}
					token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": issuer, "aud": "test-client", "sub": "test-private-sub", "exp": time.Now().Add(time.Minute).Unix()})
					idToken, err := token.SignedString(key)
					if err != nil {
						t.Errorf("签测试令牌：%v", err)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-private-access", "token_type": "Bearer", "id_token": idToken})
				case "/keys":
					keyCalls.Add(1)
					w.WriteHeader(503)
					_, _ = io.WriteString(w, "test-private-jwks")
				}
			}))
			t.Cleanup(idp.Close)
			cfg, svc := newSourceTestApplication(t, "raw")
			startTestSecurityAudit(t, svc)
			verifier := auth.NewOIDCVerifier(config.OIDCConfig{Issuer: idp.URL, ClientID: "test-client", ClientSecret: "test-only-client-secret", UsernameClaim: "preferred_username"})
			signer := auth.NewOIDCFlowSigner(cfg.JWTSecret)
			flow := verifier.NewFlow()
			// 真实 Begin 会把库内签发水位钉进流程状态；直接造流程时要补同样的钉扎。
			revision, err := svc.authSvc.CredentialRevision(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			flow.CredentialRevision = &revision
			flowCookie, err := signer.Encode(flow)
			if err != nil {
				t.Fatal(err)
			}
			svc.oidcDeps = &api.OIDCDeps{Verifier: verifier, FlowSigner: signer, RedirectAfter: "/login"}
			server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), "192.0.2.10:4321", true)
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			query := url.Values{"state": {flow.State}, "code": {"test-private-auth-code"}}
			if test.queryErr != "" {
				query.Set("error", test.queryErr)
			}
			req, err := http.NewRequest("GET", server.URL+"/api/v1/auth/oidc/callback?"+query.Encode(), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: auth.OIDCFlowCookieName, Value: flowCookie})
			req.Header.Set("X-Forwarded-For", "203.0.113.31")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 302 || resp.Header.Get("Location") != "/login#error="+test.redirect {
				t.Fatalf("OIDC 对外失败语义改变：%d %q", resp.StatusCode, resp.Header.Get("Location"))
			}
			if test.keys && keyCalls.Load() == 0 {
				t.Fatal("没有到达实际 JWKS 端点")
			}
			if test.queryErr != "" && providerCalls.Load() != 0 {
				t.Fatal("回调错误字段不应触发提供方请求")
			}
			assertProviderAudit(t, svc, resp.Header.Get("X-Request-ID"), "oidc_callback", test.outcome, test.code)
		})
	}
}

func assertProviderAudit(t *testing.T, svc *appServices, request, method, wantOutcome, wantCode string) {
	t.Helper()
	var actor, gotStatus sql.NullInt64
	var outcome, code, kind, committed, details, ip string
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := svc.db.QueryRow(`SELECT outcome,error_code,actor_kind,actor_user_id,committed,actor_name_snapshot||credential_id||session_id||auth_strength||user_agent,http_status,hex(client_ip) FROM security_event WHERE request_id=? AND event_type='auth.attempt' AND auth_method=?`, request, method).Scan(&outcome, &code, &kind, &actor, &committed, &details, &gotStatus, &ip)
		if err == nil {
			break
		}
		if err != sql.ErrNoRows || time.Now().After(deadline) {
			t.Fatalf("找不到对应认证尝试：%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if outcome != wantOutcome || code != wantCode || kind != "unknown" || actor.Valid || committed != "not_applicable" || details != "" || gotStatus.Valid || ip != "CB00711F" {
		t.Fatalf("认证事实不符：%s/%s，希望 %s/%s；归属=%s/%+v，提交=%s，状态=%+v 来源=%s，有额外快照=%t", outcome, code, wantOutcome, wantCode, kind, actor, committed, gotStatus, ip, details != "")
	}
}

func TestSecurityAuditHTTPAuthQueueFailure(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	startTestSecurityAudit(t, svc)
	if _, err := svc.db.Exec(`CREATE TRIGGER test_deny_auth_event BEFORE INSERT ON security_event WHEN NEW.event_type='auth.attempt' BEGIN SELECT RAISE(ABORT, 'test-private-error'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = svc.db.Exec(`DROP TRIGGER IF EXISTS test_deny_auth_event`) })
	server := newSourceTestServer(t, newApplicationHandler(cfg, svc, nil), "192.0.2.10:4321", true)
	status, _, body := auditTestRequest(t, server, "POST", "/api/v1/auth/login", "application/json", `{"username":"unknown-test","password":"unrecorded-test-password"}`, "")
	if status != http.StatusUnauthorized || strings.Contains(body, "private-error") {
		t.Fatalf("采集故障改变了认证裁决：%d %s", status, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if health := svc.securityAudit.Health(); health.State == "degraded" && health.Coverage == "unknown" && health.Failure == "storage_unavailable" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("采集故障没有报告覆盖未知：%+v", svc.securityAudit.Health())
}

func TestSecurityAuditHTTPShutdownDrainsBeforeClose(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	auditCtx, stopAudit := context.WithCancel(context.Background())
	defer stopAudit()
	if err := svc.securityAudit.Start(auditCtx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.securityAudit.Close(ctx)
	}()
	entered := make(chan string, 1)
	release, ingressStopping := make(chan struct{}), make(chan struct{})
	handler := newApplicationHandler(cfg, svc, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 等实际登录写出响应头时再暂停，此时请求已真实准入，且处理器尚未返回。
		handler.ServeHTTP(auditShutdownBarrierWriter{ResponseWriter: w, beforeWrite: func() {
			entered <- w.Header().Get("X-Request-ID")
			<-release
		}}, r)
	}))
	defer server.Close()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	server.Config.RegisterOnShutdown(func() { close(ingressStopping) })
	const loginPath = "/api/v1/auth/login"
	const loginBody = `{"username":"shutdown-test","password":"unrecorded-test-password"}`
	type loginResponse struct {
		id  string
		err error
	}
	response := make(chan loginResponse, 1)
	go func() {
		resp, err := server.Client().Post(server.URL+loginPath, "application/json", strings.NewReader(loginBody))
		var id string
		if err == nil {
			_ = resp.Body.Close()
			id = resp.Header.Get("X-Request-ID")
			if resp.StatusCode != http.StatusUnauthorized {
				err = fmt.Errorf("已准入登录返回 %d，期望 401", resp.StatusCode)
			}
		}
		response <- loginResponse{id: id, err: err}
	}()
	requestID := waitDrainTest(t, entered)
	if requestID == "" {
		t.Fatal("准入登录没有服务端请求 ID")
	}
	shutdown, evidence := make(chan error, 1), make(chan error, 1)
	go func() {
		shutdown <- svc.shutdownApplication(stopAudit, func() {
			var stop, attempts, matching int
			err := errors.Join(
				svc.db.Get(&stop, `SELECT clean_stop FROM security_audit_checkpoint`),
				svc.db.Get(&attempts, `SELECT count(*) FROM security_event WHERE event_type='auth.attempt'`),
				svc.db.Get(&matching, `SELECT count(*) FROM security_event WHERE event_type='auth.attempt' AND request_id=? AND outcome='denied'`, requestID),
			)
			if err == nil && (stop != 1 || attempts != 1 || matching != 1) {
				err = fmt.Errorf("取消应用前没有干净排空：停止标记=%d 尝试=%d 同请求拒绝=%d", stop, attempts, matching)
			}
			evidence <- err
		}, server.Config)
	}()
	waitAuditSignal(t, ingressStopping)
	if state := svc.securityAudit.Health().State; state == "stopping" || state == "stopped" {
		t.Errorf("请求仍在途时采集已停：%s", state)
	}
	if err := svc.db.Ping(); err != nil {
		t.Errorf("准入登录完成前数据库已不可用：%v", err)
	}
	// 监听器已停止，直接调用同一个生产装配的入口验证迟到请求不会进入登录路由。
	lateRequest := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(loginBody))
	lateRequest.Header.Set("Content-Type", "application/json")
	lateRequest.Header.Set("X-Request-ID", requestID)
	lateResponse := httptest.NewRecorder()
	handler.ServeHTTP(lateResponse, lateRequest)
	if id := lateResponse.Header().Get("X-Request-ID"); lateResponse.Code != http.StatusServiceUnavailable || id == "" || id == requestID {
		t.Errorf("封口后的新登录应返回 503 和新服务端请求 ID：状态=%d 标识=%q", lateResponse.Code, id)
	}
	unblock()
	if result := waitDrainTest(t, response); result.err != nil || result.id != requestID {
		t.Errorf("已准入登录没有收到同请求的 401：标识=%q 期望=%q 错误=%v", result.id, requestID, result.err)
	}
	for _, result := range []<-chan error{evidence, shutdown} {
		select {
		case err := <-result:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("在途请求或关停未结束")
		}
	}
	if svc.db.Ping() == nil {
		t.Fatal("关停结束后数据库仍开放")
	}
}

type auditShutdownBarrierWriter struct {
	http.ResponseWriter
	beforeWrite func()
}

func (w auditShutdownBarrierWriter) WriteHeader(statusCode int) {
	w.beforeWrite()
	w.ResponseWriter.WriteHeader(statusCode)
}

func waitAuditSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("等待 HTTP 生命周期证据超时")
	}
}

type auditFailedResponseWriter struct {
	http.ResponseWriter
	attempted *atomic.Bool
}

func (w auditFailedResponseWriter) Write([]byte) (int, error) {
	if w.attempted != nil {
		w.attempted.Store(true)
	}
	return 0, errors.New("注入响应写失败")
}

func TestSecurityAuditHTTPAuthResponseWriteFailure(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	startTestSecurityAudit(t, svc)
	var attempted atomic.Bool
	handler := newApplicationHandler(cfg, svc, nil)
	server := newSourceTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(auditFailedResponseWriter{ResponseWriter: w, attempted: &attempted}, r)
	}), "192.0.2.10:4321", true)
	_, requestID, body := auditTestRequest(t, server, "POST", "/api/v1/auth/login", "application/json", `{"username":"unknown-test","password":"unrecorded-test-password"}`, "")
	if !attempted.Load() || body != "" {
		t.Fatal("未观察到预期的应用响应写失败")
	}
	assertProviderAudit(t, svc, requestID, "password", "denied", "invalid_credentials")
}

func TestSecurityAuditHTTPRawPublish(t *testing.T) {
	for _, tc := range []struct{ name, table, format, path string }{
		{"合法路径", "", "raw", "docs/Basic 使用指南.txt"},
		{"事件头写失败", "security_event", "raw", "docs/header-failure.txt"},
		{"对象明细写失败", "security_event_object", "raw", "docs/object-failure.txt"},
		{"共用处理器的 Maven 发布", "", "maven", "com/example/demo/1.0/demo-1.0.jar"},
		{"响应写失败", "", "raw", "docs/response-failure.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, svc := newSourceTestApplication(t, tc.format)
			repo, err := svc.repoSvc.Create("audit-publish", tc.format, "hosted", "public", "", repository.RepositoryConfig{})
			if err != nil {
				t.Fatal(err)
			}
			_, user, err := svc.authSvc.Bootstrap("audit-admin", auditTestPassword)
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := svc.tokenSvc.Create(user.ID, "audit-integration")
			if err != nil {
				t.Fatal(err)
			}
			if tc.table != "" {
				when := ""
				if tc.table == "security_event" {
					when = " WHEN NEW.event_type = 'asset.change_committed'"
				}
				if _, err := svc.db.Exec(`CREATE TRIGGER test_deny_audit BEFORE INSERT ON ` + tc.table + when + ` BEGIN SELECT RAISE(ABORT, 'test-private-db-error'); END`); err != nil {
					t.Fatal(err)
				}
			}
			handler := newApplicationHandler(cfg, svc, nil)
			responseFailure := tc.path == "docs/response-failure.txt"
			if responseFailure {
				startTestSecurityAudit(t, svc)
				next := handler
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(auditFailedResponseWriter{ResponseWriter: w}, r)
				})
			}
			server := newSourceTestServer(t, handler, "192.0.2.10:4321", true)
			const content = "hello from a real HTTP upload"
			path := "/repository/" + repo.Name + (&url.URL{Path: "/" + tc.path}).EscapedPath()
			status, requestID, body := auditTestRequest(t, server, "PUT", path, "application/octet-stream", content, token)
			if tc.table != "" {
				// 准入先行：对象明细写失败落在候选封存事件上，对外为 admission_unavailable；
				// 事件头写失败发生在提升发布时，候选及其封存事件对象属合法残留。
				wantCode := "security_audit_unavailable"
				wantObjects := 0
				if tc.table == "security_event_object" {
					wantCode = "admission_unavailable"
				}
				if tc.table == "security_event" {
					wantObjects = 1
				}
				if status != 503 || !strings.Contains(body, `"`+wantCode+`"`) || strings.Contains(body, "test-private-db-error") {
					t.Errorf("故障响应：%d %s", status, body)
				}
				status, _, _ := auditTestRequest(t, server, "GET", path, "", "", "")
				if status != 404 {
					t.Errorf("回滚后制品不可公开：%d", status)
				}
				for _, q := range []string{`SELECT count(*) FROM asset`, `SELECT count(*) FROM asset_path_fts WHERE asset_path_fts MATCH 'failure'`, `SELECT count(*) FROM replication_operation_outbox`, `SELECT count(*) FROM security_event WHERE event_type='asset.change_committed'`} {
					var n int
					if err := svc.db.Get(&n, q); err != nil || n != 0 {
						t.Errorf("回滚后残留：%s => %d / %v", q, n, err)
					}
				}
				var n int
				if err := svc.db.Get(&n, `SELECT count(*) FROM security_event_object`); err != nil || n != wantObjects {
					t.Errorf("封存事件对象残留不符：%d，期望 %d / %v", n, wantObjects, err)
				}
				return
			}
			if status != 201 {
				t.Fatalf("发布失败：%d %s", status, body)
			}
			var actorID, repoID, size int64
			var opID, clientIP, peerIP, trust, pathInEvent, digest, mutationStatus string
			var eventStatus sql.NullInt64
			if err := svc.db.QueryRow(`SELECT e.actor_user_id, e.repository_id, e.operation_id, hex(e.client_ip), hex(e.peer_ip), e.proxy_trust, e.http_status, o.canonical_path, o.digest, o.size_bytes, m.status FROM security_event e JOIN security_event_object o ON o.event_sequence=e.sequence JOIN replication_operation_outbox b ON b.operation_id=e.operation_id JOIN asset_mutation m ON m.id=e.operation_id WHERE e.request_id=? AND e.event_type='asset.change_committed' AND e.committed='committed'`, requestID).Scan(&actorID, &repoID, &opID, &clientIP, &peerIP, &trust, &eventStatus, &pathInEvent, &digest, &size, &mutationStatus); err != nil {
				t.Fatalf("未找到使用真实操作号的提交事件：%v", err)
			}
			if actorID != user.ID || repoID != repo.ID || opID == "" || clientIP != "CB00711F" || peerIP != "C000020A" || trust != "trusted" || eventStatus.Valid || pathInEvent != tc.path || digest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content))) || size != int64(len(content)) || mutationStatus != "completed" {
				t.Fatalf("提交事件字段不一致：主体=%d 仓库=%d 操作=%q 来源=%s/%s/%s 状态=%+v 路径=%q 摘要=%s 大小=%d 变更状态=%s", actorID, repoID, opID, clientIP, peerIP, trust, eventStatus, pathInEvent, digest, size, mutationStatus)
			}
			if responseFailure {
				var outcome string
				deadline := time.Now().Add(3 * time.Second)
				for {
					err := svc.db.Get(&outcome, `SELECT outcome FROM security_event WHERE operation_id=? AND event_type='asset.response_observed'`, opID)
					if err == nil {
						break
					}
					if err != sql.ErrNoRows || time.Now().After(deadline) {
						t.Fatalf("响应失败观察缺失：%v", err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if outcome != "failed" {
					t.Fatalf("不能将响应写失败视为成功：%s", outcome)
				}
			}
		})
	}
}
