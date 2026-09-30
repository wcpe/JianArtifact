package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/metrics"
)

// newMetricsContext 构造一次 GET /metrics 的测试上下文。
func newMetricsContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	return c, rec
}

func TestGetMetricsRequiresWiring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 未注入指标源时返回 503，而不是空实现或 panic。
	c, rec := newMetricsContext()
	NewHandlers(Deps{}).GetMetrics(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未注入指标源时应返回 503，实际 %d", rec.Code)
	}
}

func TestGetMetricsServesPrometheusTextWithoutFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := metrics.New()
	registry.ProtocolRequest("GET", 200, "hit")
	registry.ProtocolRequest("get", 404, "miss")
	h := NewHandlers(Deps{Version: "9.9.9", Metrics: metrics.NewExposition(registry, nil)})

	c, rec := newMetricsContext()
	h.GetMetrics(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, metrics.ContentType) {
		t.Fatalf("内容类型应为 %q，实际 %q", metrics.ContentType, ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`jianartifact_protocol_requests_total{cache_result="hit",method="GET",status="200"} 1`,
		`jianartifact_protocol_requests_total{cache_result="miss",method="GET",status="404"} 1`,
		"# TYPE jianartifact_runtime_goroutines gauge",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应缺少 %q：\n%s", want, body)
		}
	}
	// 匿名口径：不输出版本等指纹信息（对齐 /healthz、/readyz、/api/v1/status 的脱敏规则）。
	if strings.Contains(body, "9.9.9") {
		t.Fatalf("匿名响应不得包含版本指纹：\n%s", body)
	}
}
