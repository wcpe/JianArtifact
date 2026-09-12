package httpserver_test

import (
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestManagementWriteRejectionsArePersistedAsSecurityAudit(t *testing.T) {
	env := newTestEnv(t)
	var bootstrap api.LoginResponse
	if code := env.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "", api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &bootstrap); code != http.StatusCreated {
		t.Fatalf("自举状态码=%d", code)
	}
	if code := env.do(t, http.MethodPost, "/api/v1/users", "", api.CreateUserRequest{Username: "unauthenticated", Password: "password-123"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("管理写未认证状态码=%d，期望 401", code)
	}
	var ordinary api.User
	if code := env.do(t, http.MethodPost, "/api/v1/users", bootstrap.Token, api.CreateUserRequest{Username: "ordinary", Password: "password-123"}, &ordinary); code != http.StatusCreated {
		t.Fatalf("创建普通用户状态码=%d", code)
	}
	var ordinaryLogin api.LoginResponse
	if code := env.do(t, http.MethodPost, "/api/v1/auth/login", "", api.LoginRequest{Username: "ordinary", Password: "password-123"}, &ordinaryLogin); code != http.StatusOK {
		t.Fatalf("普通用户登录状态码=%d", code)
	}
	if code := env.do(t, http.MethodPost, "/api/v1/users", ordinaryLogin.Token, api.CreateUserRequest{Username: "forbidden", Password: "password-123"}, nil); code != http.StatusForbidden {
		t.Fatalf("管理写越权状态码=%d，期望 403", code)
	}
	rows, err := env.audits.List(repository.AuditFilter{Action: "management.write_rejected", Limit: 20})
	if err != nil {
		t.Fatalf("读取安全审计：%v", err)
	}
	if len(rows) != 2 || rows[0].Result != "rejected" || rows[1].Result != "rejected" {
		t.Fatalf("管理 401/403 应各写一条脱敏安全审计，实得 %+v", rows)
	}
	for _, row := range rows {
		if row.Detail != "status=401" && row.Detail != "status=403" {
			t.Fatalf("安全审计 detail 必须为固定脱敏状态，实得 %q", row.Detail)
		}
	}
}
