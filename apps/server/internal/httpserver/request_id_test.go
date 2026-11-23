package httpserver

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIDOnGinRoutingResponses(t *testing.T) {
	handler := New("test", WithProtocolRoutes(func(router gin.IRouter) {
		router.POST("/test/submit/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	})).Handler(nil)
	seen := map[string]bool{}
	for _, tc := range []struct {
		method, path, location string
		status                 int
	}{
		{http.MethodGet, "/healthz/?probe=1", "/healthz?probe=1", http.StatusMovedPermanently},
		{http.MethodPost, "/test/submit?probe=1", "/test/submit/?probe=1", http.StatusTemporaryRedirect},
		{http.MethodGet, "/test/missing/", "", http.StatusNotFound},
		{http.MethodGet, "/healthz/", "/healthz", http.StatusMovedPermanently},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			inboundIDs := []string{"00000000000000000000000000000001", "00000000000000000000000000000002"}
			for _, id := range inboundIDs {
				request.Header.Add(requestIDHeader, id)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if location := response.Header().Get("Location"); response.Code != tc.status || location != tc.location {
				t.Errorf("状态与跳转目标为 %d %q，期望 %d %q", response.Code, location, tc.status, tc.location)
			}
			ids := response.Header().Values(requestIDHeader)
			id := response.Header().Get(requestIDHeader)
			decoded, err := hex.DecodeString(id)
			if len(ids) != 1 || err != nil || len(decoded) != requestIDBytes || seen[id] || slices.Contains(inboundIDs, id) {
				t.Errorf("响应应有且只有一个新生成的服务端请求标识，实际 %q", ids)
			}
			seen[id] = true
			if request.URL.RequestURI() != tc.path || !slices.Equal(request.Header.Values(requestIDHeader), inboundIDs) {
				t.Error("路由处理不得修改调用方的请求 URL 或头")
			}
		})
	}
}

func TestRequestIDAlwaysGeneratedBeforeRejection(t *testing.T) {
	handler := New("test",
		WithAllowedHosts(func() []string { return []string{"allowed.example"} }),
		WithOriginTokenGuard(func() (bool, string, string) { return true, "X-Test-Origin-Token", "test-origin" }),
	).Handler(nil)
	seen := map[string]bool{}
	for _, tc := range []struct {
		host  string
		token string
		ids   []string
	}{
		{"blocked.example", "test-origin", nil},
		{"blocked.example", "test-origin", []string{"client-controlled"}},
		{"allowed.example", "", []string{"client-controlled"}},
		{"allowed.example", "incorrect", []string{"one", "two"}},
	} {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		request.Host = tc.host
		if tc.token != "" {
			request.Header.Set("X-Test-Origin-Token", tc.token)
		}
		for _, id := range tc.ids {
			request.Header.Add(requestIDHeader, id)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("主机或回源校验返回 %d，期望 404", response.Code)
		}
		id := response.Header().Get(requestIDHeader)
		decoded, err := hex.DecodeString(id)
		if len(response.Header().Values(requestIDHeader)) != 1 || err != nil || len(decoded) != requestIDBytes || seen[id] || slices.Contains(tc.ids, id) {
			t.Fatalf("应在拒绝前生成唯一服务端标识，实际 %q", id)
		}
		seen[id] = true
	}
}
