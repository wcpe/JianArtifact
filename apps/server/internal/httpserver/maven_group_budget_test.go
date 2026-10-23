package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMavenGroupConfirmedHitStagingTimeoutIs504 是「group 回源预算与落盘预算解耦」在协议层的验收断言：
// 上游立即回 200 响应头（成员确认命中）但 body 长期滴流，落盘阶段撞上回源整体超时后，
// maven handler 必须回 504（不是 404「制品不存在」，也不是 500「内部错误」）。
//
// 500 正是漏补落盘错误分类时的症状：putStagedBlob 的裸错误不经 mapUpstreamErr，
// 会被 writeAssetErr 落到 default 分支。回源整体超时由测试环境的 5s 客户端超时提供。
func TestMavenGroupConfirmedHitStagingTimeoutIs504(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// body 长期滴流，直到回源整体超时把连接掐断。
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := w.Write([]byte("drip")); err != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createMavenRepo(t, adminToken, "mvn-budget-proxy", "proxy", srv.URL, nil)
	e.createMavenRepo(t, adminToken, "mvn-budget-group", "group", "", []string{"mvn-budget-proxy"})
	path := "/repository/mvn-budget-group/com/example/app/1.0.0/app-1.0.0.jar"

	rec := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("确认命中但落盘超时时状态码 = %d，期望 504（不得 404 也不得 500）", rec.Code)
	}
}
