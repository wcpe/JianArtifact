package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jianartifact/apps/server/internal/auditctx"
)

func TestSecuritySourceHandlerDropsUnsupportedPrefixBeforeNext(t *testing.T) {
	policy, err := auditctx.NewPolicy("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.42:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.7")
	request.Header.Add("X-Forwarded-Prefix", "")
	request.Header.Add("X-Forwarded-Prefix", "")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if values := r.Header.Values("X-Forwarded-Prefix"); len(values) != 0 {
			t.Errorf("后续处理不得收到不支持的转发前缀：%q", values)
		}
		source, ok := auditctx.SourceFrom(r.Context())
		if !ok || source.ProxyTrust != auditctx.ProxyTrustTrusted || source.ClientIP != netip.MustParseAddr("198.51.100.7") {
			t.Errorf("清理前缀不得改变合法的代理来源：%+v", source)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	requestSourceHandler(policy, next).ServeHTTP(response, request)
	if !called || response.Code != http.StatusNoContent || !slices.Equal(request.Header.Values("X-Forwarded-Prefix"), []string{"", ""}) {
		t.Fatal("入口只应在内部副本上移除前缀头，然后调用下游")
	}
}

func TestSecuritySourceDefaultDoesNotTrustGinForwarding(t *testing.T) {
	handler := New("test", WithProtocolRoutes(func(router gin.IRouter) {
		router.GET("/test/source", func(c *gin.Context) {
			source, ok := auditctx.SourceFrom(c.Request.Context())
			if !ok || source.ProxyTrust != auditctx.ProxyTrustDirect || source.ClientIP != netip.MustParseAddr("198.51.100.42") || source.TrustedScheme != "http" {
				t.Errorf("无代理配置时只应采用直连对端和实际协议：%+v", source)
			}
			c.String(http.StatusOK, "%s", c.ClientIP())
		})
	})).Handler(nil)
	request := httptest.NewRequest(http.MethodGet, "/test/source", nil)
	request.RemoteAddr = "198.51.100.42:54321"
	request.Header.Add("X-Forwarded-For", "203.0.113.8")
	request.Header.Add("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Real-IP", "203.0.113.10")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "198.51.100.42" {
		t.Fatalf("无代理配置的 Gin 必须使用连接地址：%d %q", response.Code, response.Body.String())
	}
}

func TestSecuritySourcePolicyIsAvailableInStandardContext(t *testing.T) {
	policy, err := auditctx.NewPolicy("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, forwarding string
		trust            auditctx.ProxyTrust
		client           netip.Addr
	}{
		{"可信代理", "198.51.100.7", auditctx.ProxyTrustTrusted, netip.MustParseAddr("198.51.100.7")},
		{"无可信终点", "192.0.2.7", auditctx.ProxyTrustInvalidForwarding, netip.Addr{}},
		{"无已知对端", "198.51.100.7", auditctx.ProxyTrustUnknown, netip.Addr{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/test/source", nil)
			request.RemoteAddr = "192.0.2.42:54321"
			if tc.trust == auditctx.ProxyTrustUnknown {
				request.RemoteAddr = "unknown"
			}
			request.Header.Set("X-Forwarded-For", tc.forwarding)
			request.Header.Add(requestIDHeader, "external-id")
			request.Header.Add(requestIDHeader, "second-external-id")
			var source auditctx.Source
			handler := New("test", WithSourcePolicy(policy), WithProtocolRoutes(func(router gin.IRouter) {
				router.GET("/test/source", func(c *gin.Context) {
					var ok bool
					source, ok = auditctx.SourceFrom(c.Request.Context())
					if !ok || source.ProxyTrust != tc.trust || source.ClientIP != tc.client || source.SourceTrustVersion != policy.Version() {
						t.Errorf("标准上下文中的来源不符：%+v", source)
					}
					if c.Request.RemoteAddr != request.RemoteAddr {
						t.Error("不得用代理转发地址改写连接对端")
					}
					if ids := c.Request.Header.Values(requestIDHeader); len(ids) != 1 || ids[0] != source.RequestID {
						t.Error("旧调用方不应读到任何外来请求标识")
					}
					c.Status(http.StatusNoContent)
				})
			})).Handler(nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || source.RequestID == "" || response.Header().Get(requestIDHeader) != source.RequestID || request.Header.Get(requestIDHeader) != "external-id" {
				t.Fatal("响应应使用上下文标识，原始请求保持不变")
			}
		})
	}
}

func TestSecuritySourcePreservesBodyStreamWithoutPreview(t *testing.T) {
	for _, path := range []string{"/api/v1/test-body", "/repository/test-body"} {
		t.Run(path, func(t *testing.T) {
			original := &securitySourceBody{Reader: strings.NewReader(`{"opaque":"not for auditing"}`)}
			handler := New("test", WithProtocolRoutes(func(router gin.IRouter) {
				router.POST(path, func(c *gin.Context) {
					if c.Request.Body != original || original.reads != 0 {
						t.Error("进入业务前不应替换或读取原请求体")
					}
					if auditctx.BodyFrom(c) != "" {
						t.Error("不应记录请求体预览")
					}
					value, err := io.ReadAll(c.Request.Body)
					if err != nil || string(value) != `{"opaque":"not for auditing"}` {
						t.Errorf("请求体改变：%q，%v", value, err)
					}
					if err := c.Request.Body.Close(); err != nil {
						t.Error(err)
					}
					c.Status(http.StatusNoContent)
				})
			})).Handler(nil)
			request := httptest.NewRequest(http.MethodPost, path, original)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || !original.closed {
				t.Fatalf("应完整消费并关闭原请求体：状态 %d，关闭 %v", response.Code, original.closed)
			}
		})
	}
}

type securitySourceBody struct {
	*strings.Reader
	reads  int
	closed bool
}

func (b *securitySourceBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *securitySourceBody) Close() error {
	b.closed = true
	return nil
}
