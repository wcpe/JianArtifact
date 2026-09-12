package httpserver

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOriginTokenMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := func() (bool, string, string) { return true, "X-Jian-Origin-Token", "secret-token-value-123456" }
	router := gin.New()
	router.Use(originTokenMiddleware(guard))
	router.GET("/readyz", func(c *gin.Context) { c.Status(200) })

	// 未携带 Token（非回环）→ 404
	req := httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "maven.wcpe.top"
	req.RemoteAddr = "198.51.100.9:43210"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("缺 Token 应 404，实际 %d", rec.Code)
	}

	// 携带错误 Token → 404
	req = httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "maven.wcpe.top"
	req.RemoteAddr = "198.51.100.9:43210"
	req.Header.Set("X-Jian-Origin-Token", "wrong-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("错误 Token 应 404，实际 %d", rec.Code)
	}

	// 携带正确 Token → 200
	req = httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "maven.wcpe.top"
	req.RemoteAddr = "198.51.100.9:43210"
	req.Header.Set("X-Jian-Origin-Token", "secret-token-value-123456")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("正确 Token 应放行，实际 %d", rec.Code)
	}

	// 回环（无 Token）→ 恒放行
	req = httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "127.0.0.1:50020"
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("回环应放行，实际 %d", rec.Code)
	}
}

func TestOriginTokenMiddlewareDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(originTokenMiddleware(func() (bool, string, string) { return false, "", "" }))
	router.GET("/readyz", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "whatever.example.com"
	req.RemoteAddr = "198.51.100.9:43210"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("未启用时无 Token 也应放行，实际 %d", rec.Code)
	}
}

func TestOriginTokenRejectsSpoofedLoopbackHostFromExternalPeer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := func() (bool, string, string) { return true, "X-Jian-Origin-Token", "secret-token-value-123456" }
	router := gin.New()
	router.Use(originTokenMiddleware(guard))
	router.GET("/readyz", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "127.0.0.1:50020"
	req.RemoteAddr = "198.51.100.9:43210"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("外部来源伪造回环 Host 且缺少 Token 应 404，实际 %d", rec.Code)
	}
}
