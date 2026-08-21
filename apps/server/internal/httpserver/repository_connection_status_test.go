package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// bootstrapAndLogin 自举首个管理员并返回其会话令牌。
func bootstrapAndLogin(t *testing.T, e *testEnv) string {
	t.Helper()
	var boot api.LoginResponse
	if code := e.do(t, http.MethodPost, "/api/v1/auth/bootstrap", "",
		api.BootstrapRequest{Username: "admin", Password: "admin-pass-123"}, &boot); code != http.StatusCreated {
		t.Fatalf("自举状态码 = %d，期望 201", code)
	}
	return boot.Token
}

// TestRepositoryConnectionStatusFilled 连接状态填充（FR-114 + MD-2 合并规则）：
//   - 列表对管理员返回 connectionStatus：online 的 proxy 按内存态（未探测=READY），
//     offline 优先覆盖为 OFFLINE，hosted 不返回；
//   - 普通用户列表不返回 connectionStatus（管理面信息）。
func TestRepositoryConnectionStatusFilled(t *testing.T) {
	e := newTestEnv(t)
	adminToken := bootstrapAndLogin(t, e)

	// 建普通用户 alice。
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

	// 建 proxy 与 hosted 仓库。
	var proxy api.Repository
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "npm-proxy", Format: "npm", Type: "proxy", RemoteUrl: ptr("https://registry.npmjs.org")},
		&proxy); code != http.StatusCreated {
		t.Fatalf("建 proxy 仓库状态码 = %d，期望 201", code)
	}
	if proxy.ConnectionStatus == nil {
		t.Fatalf("管理员建 proxy 仓库应返回 connectionStatus")
	}
	if proxy.ConnectionStatus.Status != api.READY {
		t.Fatalf("新建 proxy 仓库未探测应 READY，得 %s", proxy.ConnectionStatus.Status)
	}
	if proxy.ConnectionStatus.BlockedUntil != nil {
		t.Errorf("READY 状态 blockedUntil 应为空，得 %v", proxy.ConnectionStatus.BlockedUntil)
	}

	var hosted api.Repository
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "raw-hosted", Format: "raw", Type: "hosted"},
		&hosted); code != http.StatusCreated {
		t.Fatalf("建 hosted 仓库状态码 = %d，期望 201", code)
	}
	if hosted.ConnectionStatus != nil {
		t.Errorf("hosted 仓库不应返回 connectionStatus，得 %+v", hosted.ConnectionStatus)
	}

	// 管理员列表：proxy 带 READY，hosted 无。
	var list api.RepositoryList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories?page_size=100", adminToken, nil, &list); code != http.StatusOK {
		t.Fatalf("管理员列仓库状态码 = %d，期望 200", code)
	}
	for _, r := range list.Items {
		switch r.Name {
		case "npm-proxy":
			if r.ConnectionStatus == nil || r.ConnectionStatus.Status != api.READY {
				t.Errorf("管理员列表 proxy 应带 connectionStatus=READY，得 %+v", r.ConnectionStatus)
			}
		case "raw-hosted":
			if r.ConnectionStatus != nil {
				t.Errorf("管理员列表 hosted 不应带 connectionStatus，得 %+v", r.ConnectionStatus)
			}
		}
	}

	// 普通用户列表：不应带 connectionStatus。
	var userList api.RepositoryList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories?page_size=100", aliceLogin.Token, nil, &userList); code != http.StatusOK {
		t.Fatalf("普通用户列仓库状态码 = %d，期望 200", code)
	}
	for _, r := range userList.Items {
		if r.ConnectionStatus != nil {
			t.Errorf("普通用户列表不应带 connectionStatus，得 %+v", r.ConnectionStatus)
		}
	}
}

// TestRepositoryOfflineStatusOverrides 离线优先覆盖（MD-2）：置 offline 后无论内存态
// 为何，响应 connectionStatus 一律 OFFLINE。
func TestRepositoryOfflineStatusOverrides(t *testing.T) {
	e := newTestEnv(t)
	adminToken := bootstrapAndLogin(t, e)

	var proxy api.Repository
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "npm-proxy", Format: "npm", Type: "proxy", RemoteUrl: ptr("https://registry.npmjs.org")},
		&proxy); code != http.StatusCreated {
		t.Fatalf("建 proxy 仓库状态码 = %d，期望 201", code)
	}

	// 置 offline → 响应 connectionStatus=OFFLINE。
	var off api.Repository
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", adminToken,
		api.SetRepositoryOnlineRequest{Online: ptr(false)}, &off); code != http.StatusOK {
		t.Fatalf("置 offline 状态码 = %d，期望 200", code)
	}
	if off.ConnectionStatus == nil || off.ConnectionStatus.Status != api.OFFLINE {
		t.Fatalf("offline proxy 应 connectionStatus=OFFLINE，得 %+v", off.ConnectionStatus)
	}

	// 列表同样带 OFFLINE。
	var list api.RepositoryList
	if code := e.do(t, http.MethodGet, "/api/v1/repositories?page_size=100", adminToken, nil, &list); code != http.StatusOK {
		t.Fatalf("列仓库状态码 = %d，期望 200", code)
	}
	for _, r := range list.Items {
		if r.Name == "npm-proxy" {
			if r.ConnectionStatus == nil || r.ConnectionStatus.Status != api.OFFLINE {
				t.Errorf("offline proxy 列表应 connectionStatus=OFFLINE，得 %+v", r.ConnectionStatus)
			}
		}
	}
}

// TestRecheckConnectionEndpoint 手动重测端点（FR-114）：
// 仅管理员；仅 online 的 proxy 可重测；重测触发上游探测并返回最新状态。
func TestRecheckConnectionEndpoint(t *testing.T) {
	e := newTestEnv(t)
	adminToken := bootstrapAndLogin(t, e)

	// 建普通用户 alice。
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

	// 真实上游：先 500 后 200，用于验证重测状态刷新。
	fail := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	var proxy api.Repository
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "npm-proxy", Format: "npm", Type: "proxy", RemoteUrl: ptr(upstream.URL)},
		&proxy); code != http.StatusCreated {
		t.Fatalf("建 proxy 仓库状态码 = %d，期望 201", code)
	}

	// 建一个 hosted 仓库（用于验证非 proxy 不可重测）。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "raw-hosted", Format: "raw", Type: "hosted"},
		nil); code != http.StatusCreated {
		t.Fatalf("建 hosted 仓库状态码 = %d，期望 201", code)
	}

	// 未认证 → 401。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/npm-proxy/recheck-connection", "",
		nil, nil); code != http.StatusUnauthorized {
		t.Errorf("未认证重测状态码 = %d，期望 401", code)
	}

	// 非管理员 → 403。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/npm-proxy/recheck-connection", aliceLogin.Token,
		nil, nil); code != http.StatusForbidden {
		t.Errorf("非管理员重测状态码 = %d，期望 403", code)
	}

	// 上游不可达 → 重测返回 AUTO_BLOCKED。
	var blocked api.ConnectionStatus
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/npm-proxy/recheck-connection", adminToken,
		nil, &blocked); code != http.StatusOK {
		t.Fatalf("管理员重测状态码 = %d，期望 200", code)
	}
	if blocked.Status != api.AUTOBLOCKED {
		t.Fatalf("上游不可达重测应 AUTO_BLOCKED，得 %s", blocked.Status)
	}

	// 上游恢复 → 重测返回 AVAILABLE，blockedUntil 清空。
	fail = false
	var avail api.ConnectionStatus
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/npm-proxy/recheck-connection", adminToken,
		nil, &avail); code != http.StatusOK {
		t.Fatalf("管理员重测状态码 = %d，期望 200", code)
	}
	if avail.Status != api.AVAILABLE {
		t.Fatalf("上游恢复重测应 AVAILABLE，得 %s", avail.Status)
	}
	if avail.BlockedUntil != nil {
		t.Errorf("AVAILABLE 状态 blockedUntil 应为空，得 %v", avail.BlockedUntil)
	}

	// 仓库不存在 → 404。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/ghost/recheck-connection", adminToken,
		nil, nil); code != http.StatusNotFound {
		t.Errorf("仓库不存在重测状态码 = %d，期望 404", code)
	}

	// hosted 仓库不可重测 → 400。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/raw-hosted/recheck-connection", adminToken,
		nil, nil); code != http.StatusBadRequest {
		t.Errorf("hosted 重测状态码 = %d，期望 400", code)
	}
}

// TestRecheckConnectionOfflineRejected offline 仓库不可重测（FR-114：仅 online 可重测）。
func TestRecheckConnectionOfflineRejected(t *testing.T) {
	e := newTestEnv(t)
	adminToken := bootstrapAndLogin(t, e)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	if code := e.do(t, http.MethodPost, "/api/v1/repositories", adminToken,
		api.CreateRepositoryRequest{Name: "npm-proxy", Format: "npm", Type: "proxy", RemoteUrl: ptr(upstream.URL)},
		nil); code != http.StatusCreated {
		t.Fatalf("建 proxy 仓库状态码 = %d，期望 201", code)
	}
	// 置 offline。
	if code := e.do(t, http.MethodPut, "/api/v1/repositories/npm-proxy/online", adminToken,
		api.SetRepositoryOnlineRequest{Online: ptr(false)}, nil); code != http.StatusOK {
		t.Fatalf("置 offline 状态码 = %d，期望 200", code)
	}
	// offline 仓库重测 → 400。
	if code := e.do(t, http.MethodPost, "/api/v1/repositories/npm-proxy/recheck-connection", adminToken,
		nil, nil); code != http.StatusBadRequest {
		t.Errorf("offline 仓库重测状态码 = %d，期望 400", code)
	}
}
