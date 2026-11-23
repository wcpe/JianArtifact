package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestPublishPolicyAccountAuditActor(t *testing.T) {
	for _, route := range publishPolicyAccountRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newPublishPolicyAccountFixture(t)
			srv := httptest.NewServer(newApplicationHandler(f.cfg, f.svc, nil))
			defer srv.Close()
			code, requestID, _ := auditTestRequest(t, srv, http.MethodPut, route.path(f.targetID), "application/json", route.body, f.token)
			if code != http.StatusOK || requestID == "" {
				t.Fatalf("合法更新状态=%d", code)
			}
			disabled, epoch, facts, receipts := f.accountState(t)
			if !disabled || epoch != 2 || facts != 1 || receipts != 1 {
				t.Fatalf("资格未完整提交：disabled=%v epoch=%d facts=%d receipts=%d", disabled, epoch, facts, receipts)
			}
			var counted int
			err := f.svc.db.Get(&counted, `SELECT count(*) FROM security_event e JOIN security_event_object o ON o.event_sequence=e.sequence
 JOIN credential_account_access_receipt r ON r.event_id=e.event_id AND r.operation_id=e.operation_id
 WHERE e.event_type='auth.account_access_changed' AND e.actor_kind='user' AND r.actor_kind='user'
 AND e.actor_user_id=? AND r.actor_user_id=? AND e.request_id=? AND e.auth_method='web_jwt'
 AND e.committed='committed' AND o.ordinal=1 AND o.object_kind='user' AND o.object_id=CAST(r.target_user_id AS TEXT) AND r.target_user_id=?`, f.adminID, f.adminID, requestID, f.targetID)
			if err != nil || counted != 1 {
				t.Fatalf("资格事件未对应实际管理员、请求及目标：%d %v", counted, err)
			}
		})
	}
}

var publishPolicyAccountRoutes = []struct {
	name string
	path func(int64) string
	body string
}{
	{"single", func(id int64) string { return fmt.Sprintf("/api/v1/users/%d/publish-policies/policy-account", id) }, `{"webLoginDisabled":true,"maxAssetsHour":7}`},
	{"batch", func(id int64) string { return fmt.Sprintf("/api/v1/users/%d/publish-policies", id) }, `{"repositories":["policy-account"],"webLoginDisabled":true,"maxAssetsHour":7}`},
}

type publishPolicyAccountFixture struct {
	cfg               *config.Config
	svc               *appServices
	token             string
	adminID, targetID int64
}

func (f publishPolicyAccountFixture) accountState(t *testing.T) (disabled bool, epoch int64, facts, receipts int) {
	t.Helper()
	var row struct {
		Disabled bool  `db:"web_login_disabled"`
		Epoch    int64 `db:"account_epoch"`
	}
	if err := f.svc.db.Get(&row, `SELECT web_login_disabled,account_epoch FROM user WHERE id=?`, f.targetID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.db.Get(&facts, `SELECT count(*) FROM security_event WHERE event_type='auth.account_access_changed'`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.db.Get(&receipts, `SELECT count(*) FROM credential_account_access_receipt WHERE target_user_id=?`, f.targetID); err != nil {
		t.Fatal(err)
	}
	return row.Disabled, row.Epoch, facts, receipts
}

func newPublishPolicyAccountFixture(t *testing.T) publishPolicyAccountFixture {
	t.Helper()
	cfg, svc := newSourceTestApplication(t, "raw")
	jwt, admin, err := svc.authSvc.Bootstrap("policy-operator", auditTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	target, err := svc.userSvc.Create("policy-target", auditTestPassword, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.repoSvc.Create("policy-account", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	return publishPolicyAccountFixture{cfg: cfg, svc: svc, token: jwt, adminID: admin.ID, targetID: target.ID}
}

func TestPublishPolicyAccountRejectsRevokedActor(t *testing.T) {
	for _, route := range publishPolicyAccountRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newPublishPolicyAccountFixture(t)
			claims, err := f.svc.jwt.Parse(f.token)
			if err != nil {
				t.Fatal(err)
			}
			h := newApplicationHandler(f.cfg, f.svc, nil)
			logout := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 入口验权后首次读取请求体时，提交该会话的正常登出，无需等待或修改生产接线。
				r.Body = &policyLogoutBeforeBody{ReadCloser: r.Body, logout: func() error {
					err := f.svc.authSvc.Logout(claims.ID, claims.ExpiresAt.Unix())
					logout <- err
					return err
				}}
				h.ServeHTTP(w, r)
			}))
			defer srv.Close()
			code, _, body := auditTestRequest(t, srv, http.MethodPut, route.path(f.targetID), "application/json", route.body, f.token)
			select {
			case err := <-logout:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("未到入口验权后的夹点")
			}
			f.assertRejected(t, code, body, http.StatusConflict, "credential_version_conflict")
		})
	}
}

type policyLogoutBeforeBody struct {
	io.ReadCloser
	logout func() error
}

func (r *policyLogoutBeforeBody) Read(p []byte) (int, error) {
	if r.logout != nil {
		logout := r.logout
		r.logout = nil
		if err := logout(); err != nil {
			return 0, err
		}
	}
	return r.ReadCloser.Read(p)
}

func TestPublishPolicyAccountAuditUnavailable(t *testing.T) {
	for _, route := range publishPolicyAccountRoutes {
		t.Run(route.name, func(t *testing.T) {
			f := newPublishPolicyAccountFixture(t)
			if _, err := f.svc.db.Exec(`CREATE TRIGGER policy_test_event_failure BEFORE INSERT ON credential_account_access_receipt BEGIN SELECT RAISE(ABORT,'policy-private-detail'); END`); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(newApplicationHandler(f.cfg, f.svc, nil))
			defer srv.Close()
			code, _, body := auditTestRequest(t, srv, http.MethodPut, route.path(f.targetID), "application/json", route.body, f.token)
			f.assertRejected(t, code, body, http.StatusServiceUnavailable, "security_audit_unavailable")
		})
	}
}

func (f publishPolicyAccountFixture) assertRejected(t *testing.T, code int, body string, want int, errorCode string) {
	t.Helper()
	if code != want || !strings.Contains(body, `"error":{`) || !strings.Contains(body, `"code":"`+errorCode+`"`) || strings.Contains(body, "policy-private-detail") || strings.Contains(body, "operationId") {
		t.Fatalf("需要脱敏 %d %s，实际状态=%d", want, errorCode, code)
	}
	disabled, epoch, facts, receipts := f.accountState(t)
	if disabled || epoch != 1 || facts != 0 || receipts != 0 {
		t.Fatal("拒绝请求改变账号或留下成功事实")
	}
	policy, err := f.svc.publishPolicySvc.Get(f.targetID, "policy-account")
	if err != nil || policy.MaxAssetsHour != 0 {
		t.Fatal("资格子步拒绝后继续保存了仓库策略")
	}
}
