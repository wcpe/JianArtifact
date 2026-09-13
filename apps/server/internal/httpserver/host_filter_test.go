package httpserver

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHostFilterMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	allowed := []string{"maven.example.com", "repo.example.com"}
	router := gin.New()
	router.Use(hostFilterMiddleware(func() []string { return allowed }))
	router.GET("/readyz", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "maven.example.com"
	req.RemoteAddr = "198.51.100.9:43210"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("白名单命中应放行，实际 %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "203.0.113.7:50020" // 文档保留 IP（TEST-NET-3），代表“用 IP 直连”的 Host
	req.RemoteAddr = "198.51.100.9:43210"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("IP 直连 Host 应 404，实际 %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "127.0.0.1:50020"
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("回环应放行，实际 %d", rec.Code)
	}

	// 空列表不限制。
	empty := gin.New()
	empty.Use(hostFilterMiddleware(func() []string { return nil }))
	empty.GET("/readyz", func(c *gin.Context) { c.Status(200) })
	req = httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "whatever.example.com"
	req.RemoteAddr = "198.51.100.9:43210"
	rec = httptest.NewRecorder()
	empty.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("空白名单应放行，实际 %d", rec.Code)
	}
}

func TestHostFilterRejectsSpoofedLoopbackHostFromExternalPeer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(hostFilterMiddleware(func() []string { return []string{"maven.example.com"} }))
	router.GET("/readyz", func(c *gin.Context) { c.Status(200) })

	req := httptest.NewRequest("GET", "/readyz", nil)
	req.Host = "127.0.0.1:50020"
	req.RemoteAddr = "198.51.100.9:43210"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("外部来源伪造回环 Host 应 404，实际 %d", rec.Code)
	}
}

func TestRemoteAddrLoopbackClassification(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:1234":              true,
		"[::1]:1234":                  true,
		"[::ffff:127.0.0.1]:1234":     true,
		"127.0.0.1":                   true,
		"::1":                         true,
		"198.51.100.9:43210":          false,
		"[::ffff:198.51.100.9]:43210": false,
		"localhost:1234":              false,
		"":                            false,
	}
	for remoteAddr, want := range tests {
		t.Run(remoteAddr, func(t *testing.T) {
			if got := remoteAddrIsLoopback(remoteAddr); got != want {
				t.Fatalf("remoteAddrIsLoopback(%q) = %v，期望 %v", remoteAddr, got, want)
			}
		})
	}
}
