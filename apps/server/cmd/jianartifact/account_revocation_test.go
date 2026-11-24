package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

func TestAccountRevocationSurvivesReopen(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	adminToken, _, err := svc.authSvc.Bootstrap("access-admin", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := svc.userSvc.Create("access-member", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	jwt, _, err := svc.authSvc.Login(member.Username, auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.tokenSvc.Create(member.ID, "access-test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	defer server.Close()
	me := func(s *httptest.Server, proof string) int {
		code, _, _ := auditTestRequest(t, s, http.MethodGet, "/api/v1/auth/me", "", "", proof)
		return code
	}
	if me(server, jwt) != 200 || me(server, token) != 200 {
		t.Fatal("初始凭据不可用")
	}
	patch := func(body string) {
		code, _, msg := auditTestRequest(t, server, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", member.ID), "application/json", body, adminToken)
		if code != 200 {
			t.Fatalf("账号更新失败：%d %s", code, msg)
		}
	}
	patch(`{"status":"disabled"}`)
	if me(server, jwt) != 401 || me(server, token) != 401 {
		t.Fatal("停用没有拒绝旧凭据")
	}
	patch(`{"status":"active"}`)
	if me(server, jwt) != 401 || me(server, token) != 401 {
		t.Fatal("重新启用复活旧凭据")
	}
	server.Close()
	if err := svc.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openServices(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	again := httptest.NewServer(newApplicationHandler(cfg, reopened, nil))
	defer again.Close()
	if me(again, jwt) != 401 || me(again, token) != 401 || me(again, adminToken) != 200 {
		t.Fatal("同库重启未持久保持账号隔离")
	}
	login, _, err := reopened.authSvc.Login(member.Username, auditTestPassword)
	if err != nil || me(again, login) != 200 {
		t.Fatal("重新启用后合法认证未恢复")
	}
}

func TestAccountRevocationLegacyAndTransitions(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	access, _, err := svc.authSvc.Bootstrap("access-owner", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.userSvc.Create("access-target", auditTestPassword, "admin")
	if err != nil {
		t.Fatal(err)
	}
	legacy, _, _, err := auth.NewJWTManager(cfg.JWTSecret).Issue(u.ID, u.Role)
	if err != nil {
		t.Fatal(err)
	}
	machine, digest := auth.GenerateToken()
	if _, err = svc.db.Exec(`INSERT INTO api_token(user_id,name,token_digest) VALUES(?, 'pre-upgrade', ?)`, u.ID, digest); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	defer srv.Close()
	me := func(token string) int {
		code, _, _ := auditTestRequest(t, srv, http.MethodGet, "/api/v1/auth/me", "", "", token)
		return code
	}
	patch := func(token, body string) (int, string) {
		code, _, out := auditTestRequest(t, srv, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", u.ID), "application/json", body, token)
		return code, out
	}
	state := func() (int64, int) {
		t.Helper()
		var epoch int64
		var count int
		if err := svc.db.Get(&epoch, `SELECT account_epoch FROM user WHERE id=?`, u.ID); err != nil {
			t.Fatal(err)
		}
		if err := svc.db.Get(&count, `SELECT count(*) FROM credential_account_access_receipt WHERE target_user_id=?`, u.ID); err != nil {
			t.Fatal(err)
		}
		return epoch, count
	}
	if me(legacy) != 200 || me(machine) != 200 {
		t.Fatal("初代合法旧凭据未兼容")
	}
	for _, table := range []string{"security_event_object", "credential_account_access_receipt"} {
		if _, err := svc.db.Exec(`CREATE TRIGGER access_fixture_failure BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'fixture-unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		code, out := patch(access, `{"role":"user"}`)
		if _, err := svc.db.Exec(`DROP TRIGGER access_fixture_failure`); err != nil {
			t.Fatal(err)
		}
		if code != 503 || !strings.Contains(out, "security_audit_unavailable") || strings.Contains(out, "operationId") || strings.Contains(out, "fixture-unavailable") {
			t.Fatalf("不可用失去脱敏 503：%d", code)
		}
		if epoch, count := state(); epoch != 1 || count != 0 {
			t.Fatal("失败事务改变账号资格或回执")
		}
	}
	for _, bad := range []string{`{"role":""}`, `{"status":""}`, `{"role":"unknown"}`, `{"status":"unknown"}`} {
		if code, _ := patch(access, bad); code != 400 {
			t.Fatalf("无效资格参数返回 %d", code)
		}
	}
	if code, _ := patch(access, `{"role":"user","webLoginDisabled":true,"status":"disabled"}`); code != 200 {
		t.Fatalf("组合收窄：%d", code)
	}
	if epoch, count := state(); epoch != 2 || count != 1 {
		t.Fatal("组合变更应单次推进代次与回执")
	}
	if code, _ := patch(access, `{"role":"user","webLoginDisabled":true,"status":"disabled"}`); code != 200 {
		t.Fatalf("重复请求：%d", code)
	}
	if code, _ := patch(access, `{"role":"admin","webLoginDisabled":false,"status":"active"}`); code != 200 {
		t.Fatalf("恢复资格：%d", code)
	}
	if epoch, count := state(); epoch != 2 || count != 2 {
		t.Fatal("重复不能制造新事件；恢复资格不能回拨代次")
	}
	if me(legacy) != 401 || me(machine) != 401 {
		t.Fatal("恢复资格复活未绑定旧凭据")
	}
	var facts int
	if err := svc.db.Get(&facts, `SELECT count(*) FROM security_event e JOIN security_event_object o ON o.event_sequence=e.sequence
	JOIN credential_account_access_receipt r ON r.event_id=e.event_id AND r.operation_id=e.operation_id
	WHERE e.event_type='auth.account_access_changed' AND e.actor_kind='user' AND e.actor_user_id<>r.target_user_id AND o.object_kind='user' AND o.object_id=CAST(r.target_user_id AS TEXT) AND r.target_user_id=?`, u.ID); err != nil || facts != 2 {
		t.Fatalf("事务事实并非真实发起人与目标：%d %v", facts, err)
	}
	initial, err := svc.userSvc.Create("historically-disabled", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	old, _, _, err := auth.NewJWTManager(cfg.JWTSecret).Issue(initial.ID, initial.Role)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.users.Update(initial.ID, "", "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.userSvc.Update(initial.ID, "", "active"); err != nil {
		t.Fatal(err)
	}
	if me(old) != 401 || me(access) != 200 {
		t.Fatal("历史停用账号旧会话复活，或影响独立账号")
	}
}

func TestAccountRevocationIssuanceFence(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	svc.authSvc.SetMfaService(nil)
	_, u, err := svc.authSvc.Bootstrap("issuance-admin", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, member, err := svc.authSvc.LoginExternal(domain.ExternalIdentity{Source: "oidc", Subject: "issuance-external", Username: "issuance-member"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	revision, err := svc.authSvc.CredentialRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jwt, _, err := svc.authSvc.Login(u.Username, auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	a := auth.NewAuthenticator(auth.NewJWTManager(cfg.JWTSecret), svc.store)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	p, err := a.AdmissionPrincipal(req)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := svc.tokenSvc.CreateForPrincipal(ctx, p, "before-lock")
	if err != nil || before == "" {
		t.Fatal("撤销前签发失败")
	}
	changed := make(chan error, 1)
	go func() { changed <- svc.userSvc.ChangePassword(u.ID, "rotated-fixture-user") }()
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if token, _, err := svc.tokenSvc.CreateForPrincipal(ctx, p, "stale"); token != "" || !errors.Is(err, domain.ErrCredentialVersionConflict) {
		t.Fatal("陈旧的在途证明能发证")
	}
	if _, err := svc.userSvc.UpdateForPrincipal(ctx, p, member.ID, "", "disabled", nil); !errors.Is(err, domain.ErrCredentialVersionConflict) {
		t.Fatal("陈旧的在途证明能改账号资格")
	}
	if token, _, err := svc.authSvc.LoginExternalContext(ctx, domain.ExternalIdentity{Source: "oidc", Subject: "issuance-external", Username: "issuance-member"}, revision); err != nil || token == "" {
		t.Fatal("独立账号在途外部流程不应被误撤销")
	}
	if err := svc.userSvc.ChangePassword(member.ID, "rotated-external-fixture"); err != nil {
		t.Fatal(err)
	}
	if token, _, err := svc.authSvc.LoginExternalContext(ctx, domain.ExternalIdentity{Source: "oidc", Subject: "issuance-external", Username: "issuance-member"}, revision); token != "" || !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatal("外部认证跨改密后未要求重开")
	}
}

func TestAccountRevocationSingleCredentialAndExpiry(t *testing.T) {
	cfg, svc := newSourceTestApplication(t, "raw")
	svc.authSvc.SetMfaService(nil)
	jwt, u, err := svc.authSvc.Bootstrap("single-revoke-admin", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.authSvc.Login(u.Username, auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, t1, err := svc.tokenSvc.Create(u.ID, "single-revoke")
	if err != nil {
		t.Fatal(err)
	}
	token2, t2, err := svc.tokenSvc.Create(u.ID, "single-expiry")
	if err != nil {
		t.Fatal(err)
	}
	signer := auth.NewJWTManager(cfg.JWTSecret)
	claims, err := signer.Parse(jwt)
	if err != nil || claims.AccountEpoch != 1 || claims.SessionID == "" || claims.SessionID == claims.ID {
		t.Fatal("新会话未绑定独立标识和代次")
	}
	var bound int
	if err := svc.db.Get(&bound, `SELECT count(*) FROM credential_session WHERE session_id=? AND user_id=? AND account_epoch=1`, claims.SessionID, u.ID); err != nil || bound != 1 {
		t.Fatal("新会话缺少持久行")
	}
	srv := httptest.NewServer(newApplicationHandler(cfg, svc, nil))
	defer srv.Close()
	access := func(proof string) int {
		code, _, _ := auditTestRequest(t, srv, http.MethodGet, "/api/v1/auth/me", "", "", proof)
		return code
	}
	if err := svc.tokenSvc.Delete(t1.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if access(token) != 401 || access(token2) != 200 {
		t.Fatal("单 Token 吊销未隔离其它 Token")
	}
	if _, err := svc.db.Exec(`UPDATE api_token SET expires_at=1 WHERE id=?`, t2.ID); err != nil {
		t.Fatal(err)
	}
	if access(token2) != 401 {
		t.Fatal("过期 Token 仍有效")
	}
	if _, _, _ = auditTestRequest(t, srv, http.MethodPost, "/api/v1/auth/logout", "", "", jwt); access(jwt) != 401 || access(second) != 200 {
		t.Fatal("单会话登出未生效或波及同账号其他会话")
	}
	claims, err = signer.Parse(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`DELETE FROM credential_session WHERE session_id=?`, claims.SessionID); err != nil {
		t.Fatal(err)
	}
	if access(second) != 401 {
		t.Fatal("已签名会话缺失持久行时未拒绝")
	}
}
