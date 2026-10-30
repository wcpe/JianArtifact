package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auth"
)

// serveAs 用指定主体（nil = 匿名）打一次请求。
//
// 主体通过中间件注入，与生产 authenticator.Optional() 的行为一致；
// 不能用 gin.CreateTestContext + HandleContext —— 那样不会执行路由注册的
// 处理器链，鉴权断言会得到与线上不同的结果。
func serveAs(t *testing.T, r *gin.Engine, p *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// newAuthRouter 组装一个可注入主体的路由：先挂主体中间件，再注册待测端点。
func newAuthRouter(p *auth.Principal, register func(*gin.Engine, *Handlers)) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p != nil {
			c.Set("auth.principal", p)
		}
		c.Next()
	})
	register(r, NewHandlers(Deps{}))
	return r
}

// TestMaintenanceEndpointsRequireAdmin 覆盖全部新增维护端点的鉴权边界：
// 匿名 401、非管理员 403。
//
// 为什么必须补这一层：这些端点的 admin 校验写在 handler 内部（requireAdmin），
// 而既有测试统一注入**管理员**主体——于是「非管理员会不会被拒」从来没被验证过。
// 新增端点时一旦漏写 requireAdmin，编译和现有测试都不会报错，一个普通用户
// 就能触发清理、恢复或彻底删除。这里把该边界变成可执行断言。
func TestMaintenanceEndpointsRequireAdmin(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.ReleaseMode)

	cases := []struct {
		name, method, path, body string
		register                 func(*gin.Engine, *Handlers)
	}{
		{"存储体检", http.MethodGet, "/api/v1/maintenance/storage-health", "", func(r *gin.Engine, h *Handlers) {
			r.GET("/api/v1/maintenance/storage-health", h.GetStorageHealth)
		}},
		{"清理预览", http.MethodPost, "/api/v1/maintenance/cleanup/preview", `{"kind":"empty_dirs"}`, registerCleanupRoutes},
		{"清理执行", http.MethodPost, "/api/v1/maintenance/cleanup", `{"kind":"empty_dirs"}`, registerCleanupRoutes},
		{"任务列表", http.MethodGet, "/api/v1/maintenance/clean-tasks", "", registerCleanupRoutes},
		{"任务详情", http.MethodGet, "/api/v1/maintenance/clean-tasks/1", "", registerCleanupRoutes},
		{"回收站列表", http.MethodGet, "/api/v1/maintenance/trash", "", registerTrashRoutes},
		{"回收站恢复", http.MethodPost, "/api/v1/maintenance/trash/restore", `{"ids":[1]}`, registerTrashRoutes},
		{"回收站彻底删除", http.MethodPost, "/api/v1/maintenance/trash/purge", `{"ids":[1]}`, registerTrashRoutes},
		{"回收站清空", http.MethodPost, "/api/v1/maintenance/trash/purge-all", `{"all":true}`, registerTrashRoutes},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// 依赖留空：鉴权必须在触碰业务依赖之前就拒绝，
			// 因此即便控制器是 nil（端点会 503）也必须先拿到 401/403。
			if rec := serveAs(t, newAuthRouter(nil, tc.register), nil, tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
				t.Fatalf("匿名应 401，得 %d：%s", rec.Code, rec.Body.String())
			}
			if rec := serveAs(t, newAuthRouter(userPrincipal(), tc.register), userPrincipal(), tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
				t.Fatalf("非管理员应 403，得 %d：%s", rec.Code, rec.Body.String())
			}
		})
	}
}
