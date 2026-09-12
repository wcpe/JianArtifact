package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
)

// writeFreezeHandlers 仅实现本文件覆盖的契约入口；其余接口方法由嵌入接口满足。
type writeFreezeHandlers struct {
	api.ServerInterface
	loginCalls  int
	createCalls int
	backupCalls int
	readCalls   int
}

func (h *writeFreezeHandlers) Login(c *gin.Context) {
	h.loginCalls++
	c.Status(http.StatusNoContent)
}

func (h *writeFreezeHandlers) CreateUser(c *gin.Context) {
	h.createCalls++
	c.Status(http.StatusCreated)
}

func (h *writeFreezeHandlers) CreateBackup(c *gin.Context) {
	h.backupCalls++
	c.Status(http.StatusAccepted)
}

func (h *writeFreezeHandlers) ListRepositories(c *gin.Context, _ api.ListRepositoriesParams) {
	h.readCalls++
	c.Status(http.StatusOK)
}

type writeFreezeTestServer struct {
	handler             http.Handler
	handlers            *writeFreezeHandlers
	freeze              *domain.FreezeController
	uploadChunkCalls    int
	uploadCompleteCalls int
	protocolWriteCalls  int
}

// newWriteFreezeTestServer 用真实 httpserver.New + WithWriteFreeze 装配测试服务。
//
// 维护端点与 URL 导入端点已是**真实契约端点**（由 RegisterHandlersWithOptions 注册），
// 因此这里不再手动注册、也不再断言具体状态码——中间件的职责只是"放行/拦截"，
// 放行后由真实 handler 自己处理鉴权（本测试未挂认证中间件，故会返回 401）。
// 只有尚未进入契约的分片上传路径仍以协议路由注册，以便断言精确状态码。
func newWriteFreezeTestServer(frozen bool) *writeFreezeTestServer {
	handlers := &writeFreezeHandlers{}
	freeze := domain.NewFreezeController(nil)
	if frozen {
		// 手动冻结：零值 until 表示不自动解冻。
		freeze.Freeze(time.Time{}, "relocation")
	}
	env := &writeFreezeTestServer{handlers: handlers, freeze: freeze}
	server := New("test",
		WithHandlers(handlers),
		WithWriteFreeze(freeze.State),
		WithProtocolRoutes(func(r gin.IRouter) {
			// 分片上传尚未进入契约，仍以协议路由注册以便断言精确状态码；
			// 它同样只写 restore-staging 与 restore.pending，冻结期必须放行。
			r.PUT("/api/v1/backups/uploads/u1/chunks/0", func(c *gin.Context) {
				env.uploadChunkCalls++
				c.Status(http.StatusNoContent)
			})
			r.POST("/api/v1/backups/uploads/u1/complete", func(c *gin.Context) {
				env.uploadCompleteCalls++
				c.Status(http.StatusOK)
			})
			r.PUT("/repository/example/path", func(c *gin.Context) {
				env.protocolWriteCalls++
				c.Status(http.StatusCreated)
			})
		}),
	)
	env.handler = server.Handler(nil)
	return env
}

func (e *writeFreezeTestServer) request(method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func assertWriteFrozen(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（响应：%s）", recorder.Code, recorder.Body.String())
	}
	var response api.Error
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析错误响应失败：%v（响应：%s）", err, recorder.Body.String())
	}
	if response.Error.Code != "write_frozen" {
		t.Fatalf("错误码 = %q，期望 write_frozen", response.Error.Code)
	}
}

// assertNotWriteFrozen 断言请求未被写冻结中间件拦截。
// 用于"解冻出口/导入路径必须放行"的回归：只要不是 503，就说明中间件放行了。
func assertNotWriteFrozen(t *testing.T, recorder *httptest.ResponseRecorder, what string) {
	t.Helper()
	if recorder.Code == http.StatusServiceUnavailable {
		t.Fatalf("%s 被写冻结中间件拦截（503），冻结期应当仍可放行：%s", what, recorder.Body.String())
	}
}

// TestWriteFreezeRejectsWritesExceptAllowed 验证关键回归：冻结中维护端点与导入/上传放行，
// 其余写被 503 + write_frozen 拦下（防"冻上就解不开"）。
func TestWriteFreezeRejectsWritesExceptAllowed(t *testing.T) {
	env := newWriteFreezeTestServer(true)

	// 写被拦：503 + write_frozen，且处理器不应被调用。
	assertWriteFrozen(t, env.request(http.MethodPost, "/api/v1/users"))
	assertWriteFrozen(t, env.request(http.MethodPost, "/api/v1/backups"))
	assertWriteFrozen(t, env.request(http.MethodPut, "/repository/example/path"))
	if env.handlers.createCalls != 0 {
		t.Errorf("创建用户处理器调用次数 = %d，期望 0", env.handlers.createCalls)
	}
	if env.handlers.backupCalls != 0 {
		t.Errorf("生成备份处理器调用次数 = %d，期望 0", env.handlers.backupCalls)
	}
	if env.protocolWriteCalls != 0 {
		t.Errorf("协议写处理器调用次数 = %d，期望 0", env.protocolWriteCalls)
	}

	// 解冻出口放行：维护命名空间全方法 + 登录。
	// 维护/导入已是真实契约端点，放行后由 handler 自己鉴权（本测试未挂认证中间件 → 401）；
	// 关键断言是「未被冻结中间件拦成 503」——冻结后解不开是这类开关最危险的失败模式。
	assertNotWriteFrozen(t, env.request(http.MethodPost, "/api/v1/maintenance/freeze"), "POST 维护冻结端点")
	assertNotWriteFrozen(t, env.request(http.MethodDelete, "/api/v1/maintenance/freeze"), "DELETE 维护冻结端点")
	assertNotWriteFrozen(t, env.request(http.MethodGet, "/api/v1/maintenance/freeze"), "GET 维护冻结端点")
	if recorder := env.request(http.MethodPost, "/api/v1/auth/login"); recorder.Code != http.StatusNoContent {
		t.Fatalf("登录状态码 = %d，期望 204", recorder.Code)
	}
	if env.handlers.loginCalls != 1 {
		t.Errorf("登录处理器调用次数 = %d，期望 1", env.handlers.loginCalls)
	}

	// 导入/上传放行：冻结窗口的全部意义就是停写后做搬迁切换。
	assertNotWriteFrozen(t, env.request(http.MethodPost, "/api/v1/backups/import"), "URL 导入")
	if recorder := env.request(http.MethodPut, "/api/v1/backups/uploads/u1/chunks/0"); recorder.Code != http.StatusNoContent {
		t.Fatalf("上传分片状态码 = %d，期望 204", recorder.Code)
	}
	if recorder := env.request(http.MethodPost, "/api/v1/backups/uploads/u1/complete"); recorder.Code != http.StatusOK {
		t.Fatalf("完成上传状态码 = %d，期望 200", recorder.Code)
	}
	if env.uploadChunkCalls != 1 || env.uploadCompleteCalls != 1 {
		t.Errorf("上传调用次数异常：chunk=%d complete=%d", env.uploadChunkCalls, env.uploadCompleteCalls)
	}

	// 读放行。
	if recorder := env.request(http.MethodGet, "/api/v1/repositories"); recorder.Code != http.StatusOK {
		t.Fatalf("读取状态码 = %d，期望 200", recorder.Code)
	}
	if env.handlers.readCalls != 1 {
		t.Errorf("读取处理器调用次数 = %d，期望 1", env.handlers.readCalls)
	}
}

func TestWriteFreezeDisabledKeepsWrites(t *testing.T) {
	env := newWriteFreezeTestServer(false)

	if recorder := env.request(http.MethodPost, "/api/v1/users"); recorder.Code != http.StatusCreated {
		t.Fatalf("管理写状态码 = %d，期望 201", recorder.Code)
	}
	if recorder := env.request(http.MethodPut, "/repository/example/path"); recorder.Code != http.StatusCreated {
		t.Fatalf("协议写状态码 = %d，期望 201", recorder.Code)
	}
	if env.handlers.createCalls != 1 {
		t.Errorf("创建用户处理器调用次数 = %d，期望 1", env.handlers.createCalls)
	}
	if env.protocolWriteCalls != 1 {
		t.Errorf("协议写处理器调用次数 = %d，期望 1", env.protocolWriteCalls)
	}
}

func TestWriteFreezeUnfreezeRestoresWrites(t *testing.T) {
	env := newWriteFreezeTestServer(true)
	if recorder := env.request(http.MethodPost, "/api/v1/users"); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("冻结中管理写应 503，得 %d", recorder.Code)
	}
	env.freeze.Unfreeze()
	if recorder := env.request(http.MethodPost, "/api/v1/users"); recorder.Code != http.StatusCreated {
		t.Fatalf("解冻后管理写应 201，得 %d", recorder.Code)
	}
}
