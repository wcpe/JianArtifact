package httpserver_test

import (
	"net/http"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// TestRepositoryOnlineEndpoint 仓库 online/offline 端点（FR-113）：
// 仅管理员可操作；置离线后持久化读回；仓库不存在 404。
func TestRepositoryOnlineEndpoint(t *testing.T) {
	e := newTestEnv(t)

	// 自举管理员。
	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	adminToken := boot.Token

	// 建普通用户并登录（非管理员）。
	var alice api.User
	if code := e.do(t, http.MethodPost, "/api/v1/users", adminToken,
		api.CreateUserRequest{Username: "alice", Password: "alice-pass-123"}, &alice); code != http.StatusCreated {
		t.Fatalf("建用户状态码 = %d，期望 201", code)
	}
	var aliceLogin api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/login", "",
		api.LoginRequest{Username: "alice", Password: "alice-pass-123"}, &aliceLogin); code != http.StatusOK {
		t.Fatalf("alice 登录状态码 = %d，期望 200", code)
	}
	aliceToken := aliceLogin.Token

	// 建一个 proxy 仓库。
	var created api.Repository
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "npm-proxy", Format: "npm", Type: "proxy", RemoteUrl: ptr("https://registry.npmjs.org")},
		&created); code != http.StatusCreated {
		t.Fatalf("建仓库状态码 = %d，期望 201", code)
	}
	// 默认在线。
	if created.Online == nil || !*created.Online {
		t.Fatalf("新建仓库应默认 online=true，得 %+v", created.Online)
	}

	// 未认证 → 401。
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", "",
		api.SetRepositoryOnlineRequest{Online: ptr(false)}, nil); code != http.StatusUnauthorized {
		t.Errorf("未认证置 offline 状态码 = %d，期望 401", code)
	}

	// 非管理员 → 403。
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", aliceToken,
		api.SetRepositoryOnlineRequest{Online: ptr(false)}, nil); code != http.StatusForbidden {
		t.Errorf("非管理员置 offline 状态码 = %d，期望 403", code)
	}

	// 管理员置 offline → 200 且回显 online=false。
	var off api.Repository
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", adminToken,
		api.SetRepositoryOnlineRequest{Online: ptr(false)}, &off); code != http.StatusOK {
		t.Fatalf("管理员置 offline 状态码 = %d，期望 200", code)
	}
	if off.Online == nil || *off.Online {
		t.Errorf("置 offline 后响应应 online=false，得 %+v", off.Online)
	}

	// 写后读回：列表带出 online=false。
	var list api.RepositoryList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories?page_size=100", adminToken, nil, &list); code != http.StatusOK {
		t.Fatalf("列仓库状态码 = %d，期望 200", code)
	}
	found := false
	for _, r := range list.Items {
		if r.Name == "npm-proxy" {
			found = true
			if r.Online == nil || *r.Online {
				t.Errorf("列仓库应带出 online=false，得 %+v", r.Online)
			}
		}
	}
	if !found {
		t.Fatalf("列表中未找到 npm-proxy")
	}

	// 管理员置回在线 → 200。
	var on api.Repository
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", adminToken,
		api.SetRepositoryOnlineRequest{Online: ptr(true)}, &on); code != http.StatusOK {
		t.Fatalf("管理员置 online 状态码 = %d，期望 200", code)
	}
	if on.Online == nil || !*on.Online {
		t.Errorf("置 online 后响应应 online=true，得 %+v", on.Online)
	}

	// 仓库不存在 → 404。
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/ghost/online", adminToken,
		api.SetRepositoryOnlineRequest{Online: ptr(false)}, nil); code != http.StatusNotFound {
		t.Errorf("仓库不存在置 offline 状态码 = %d，期望 404", code)
	}

	// 请求体非法（缺 online 字段）→ 400。
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", adminToken,
		map[string]any{}, nil); code != http.StatusBadRequest {
		t.Errorf("缺 online 字段状态码 = %d，期望 400", code)
	}
}

func ptr[T any](v T) *T { return &v }
