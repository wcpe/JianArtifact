// 受限机器凭据在协议入口取 scope 交集、在管理面被通道拒绝。
package httpserver_test

import (
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// bootstrapJWT 自举管理员，返回 JWT 会话（受限凭据只能由交互会话签发）
// 与同时可用的管理 API Token（协议/仓库设置用）。
func bootstrapJWT(t *testing.T, e *protocolEnv) (string, string) {
	t.Helper()
	sessionToken, _, _ := installAdmin(t, e, "admin", "admin-pass-123")
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", sessionToken,
		api.CreateTokenRequest{Name: "protocol-test"}, &created); code != http.StatusCreated {
		t.Fatalf("创建协议测试 Token 状态码 = %d，期望 201", code)
	}
	return sessionToken, created.Token
}

func issueScopedToken(t *testing.T, e *protocolEnv, sessionJWT, repo, prefix string, actions []string) string {
	t.Helper()
	scope := api.MachineTokenScope{Repository: repo, Actions: []api.MachineTokenScopeActions{}}
	for _, a := range actions {
		scope.Actions = append(scope.Actions, api.MachineTokenScopeActions(a))
	}
	if prefix != "" {
		scope.PathPrefix = &prefix
	}
	purpose := api.CreateTokenRequestPurpose("artifact_publish")
	req := api.CreateTokenRequest{
		Name:    "scoped-ci",
		Purpose: &purpose,
		Scopes:  &[]api.MachineTokenScope{scope},
	}
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", sessionJWT, req, &created); code != http.StatusCreated {
		t.Fatalf("受限签发状态码 = %d，期望 201", code)
	}
	return created.Token
}

func TestScopedTokenRawProtocolScopeIntersection(t *testing.T) {
	t.Parallel()
	e := newProtocolEnv(t)
	sessionJWT, adminToken := bootstrapJWT(t, e)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")

	scoped := issueScopedToken(t, e, sessionJWT, "raw-hosted", "allowed/", []string{"read", "publish"})
	payload := []byte("scoped payload")

	// 作用域内路径：scope × ACL 交集允许发布。
	rec := e.rawReq(http.MethodPut, "/repository/raw-hosted/allowed/a.txt", "Bearer "+scoped, "text/plain", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("作用域内 PUT 状态码 = %d，期望 201（体：%s）", rec.Code, rec.Body.String())
	}
	// 前缀相邻但不属同一段：allowed2/ 不命中 allowed/。
	rec = e.rawReq(http.MethodPut, "/repository/raw-hosted/allowed2/b.txt", "Bearer "+scoped, "text/plain", payload)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("段外 PUT 状态码 = %d，期望 403（体：%s）", rec.Code, rec.Body.String())
	}
	// 作用域内读取允许。
	rec = e.rawReq(http.MethodGet, "/repository/raw-hosted/allowed/a.txt", "Bearer "+scoped, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("作用域内 GET 状态码 = %d，期望 200", rec.Code)
	}
	// 管理员名下受限 Token 不因角色放大到作用域外。
	rec = e.rawReq(http.MethodPut, "/repository/raw-hosted/elsewhere/c.txt", "Bearer "+scoped, "text/plain", payload)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("作用域外 PUT 状态码 = %d，期望 403", rec.Code)
	}
}

func TestScopedTokenDeniedManagementChannel(t *testing.T) {
	t.Parallel()
	e := newProtocolEnv(t)
	sessionJWT, adminToken := bootstrapJWT(t, e)
	e.createRawRepo(t, adminToken, "raw-hosted", "private")
	scoped := issueScopedToken(t, e, sessionJWT, "raw-hosted", "", []string{"read", "publish"})

	// 受限凭据不能进入管理面（列表、发证、设置均为通道拒绝）。
	for _, path := range []string{"/api/v1/tokens", "/api/v1/repositories"} {
		var out any
		if code := e.jsonReq(t, http.MethodGet, path, scoped, nil, &out); code != http.StatusForbidden {
			t.Fatalf("%s 受限凭据状态码 = %d，期望 403 credential_channel_denied", path, code)
		}
	}
	// 受限凭据不能签发任何凭据。
	var created api.TokenCreated
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/tokens", scoped,
		api.CreateTokenRequest{Name: "nested"}, &created); code != http.StatusForbidden {
		t.Fatalf("受限凭据发证状态码 = %d，期望 403", code)
	}
}
