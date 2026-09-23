package auditctx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

func TestTokenPreview(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"空值不回显", "", ""},
		{"bearer 保留首尾", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abcdef1dnM", "Bearer eyJhbG****1dnM"},
		{"无 scheme 也掩码", "abcdefghijklmnop", "abcdef****mnop"},
		{"短令牌全掩码", "abc", "****"},
		{"仅 scheme", "Bearer", "****"},
	}
	for _, tc := range cases {
		if got := TokenPreview(tc.raw); got != tc.want {
			t.Fatalf("%s: TokenPreview(%q) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestSanitizeBody(t *testing.T) {
	t.Run("JSON 掩码敏感键并缩进", func(t *testing.T) {
		got := SanitizeBody([]byte(`{"username":"admin","password":"s3cret","nested":{"token":"t"},"ids":[1,2]}`))
		want := `{
  "ids": [
    1,
    2
  ],
  "nested": {
    "token": "****"
  },
  "password": "****",
  "username": "admin"
}`
		if got != want {
			t.Fatalf("SanitizeBody 输出不符：\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("非 JSON 原样保留（截断到上限）", func(t *testing.T) {
		if got := SanitizeBody([]byte("  plain text body  ")); got != "plain text body" {
			t.Fatalf("非 JSON 应保留原文，got=%q", got)
		}
	})

	t.Run("非 UTF-8 请求体标注而非原样回显（GBK 客户端回归）", func(t *testing.T) {
		// GBK 编码的「你好」= C4 E3 BA C3：JSON 解析必失败；此前回退会把损坏字节
		// 原样入库，前端按 UTF-8 解码后成 U+FFFD 乱码（看起来像真实数据）。
		got := SanitizeBody([]byte("{\"reason\":\"\xC4\xE3\xBA\xC3\"}"))
		if !strings.Contains(got, "不是合法 UTF-8") {
			t.Fatalf("非 UTF-8 请求体应返回明确标注，got=%q", got)
		}
	})

	t.Run("空请求体返回空串", func(t *testing.T) {
		if got := SanitizeBody([]byte("   ")); got != "" {
			t.Fatalf("空请求体应返回空串，got=%q", got)
		}
	})
}

func TestIsManagementWrite(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{"POST", "/api/v1/repositories", true},
		{"DELETE", "/api/v1/artifacts/1", true},
		{"GET", "/api/v1/repositories", false},
		{"HEAD", "/api/v1/repositories", false},
		{"OPTIONS", "/api/v1/repositories", false},
		{"POST", "/healthz", false},
	}
	for _, tc := range cases {
		if got := IsManagementWrite(tc.method, tc.path); got != tc.want {
			t.Fatalf("IsManagementWrite(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// parseCapturedHeaders 解析 CaptureHeaders 输出，便于逐键断言。
func parseCapturedHeaders(t *testing.T, raw string) map[string]string {
	t.Helper()
	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("捕获的请求头不是合法 JSON：%v（原文 %q）", err, raw)
	}
	return parsed
}

func TestCaptureHeadersWhitelistAndSensitiveSkip(t *testing.T) {
	t.Run("只保留白名单常用头，Authorization 与白名单外头不入列", func(t *testing.T) {
		header := http.Header{}
		header.Set("User-Agent", "agent/1.0")
		header.Set("Accept-Language", "zh-CN")
		header.Set("Content-Type", "application/json")
		header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
		header.Set("Authorization", "Bearer super-secret-token")
		header.Set("Cookie", "session=abc")
		header.Set("X-Custom-Debug", "internal-only")
		got := CaptureHeaders(header)
		parsed := parseCapturedHeaders(t, got)
		if parsed["User-Agent"] != "agent/1.0" || parsed["Accept-Language"] != "zh-CN" ||
			parsed["Content-Type"] != "application/json" || parsed["X-Forwarded-For"] != "203.0.113.9, 10.0.0.1" {
			t.Fatalf("白名单头应完整保留，实际 %+v", parsed)
		}
		// Authorization 走 TokenPreview 机制，连同其它白名单外头（Cookie/自定义头）绝不入列。
		for _, banned := range []string{"Authorization", "authorization", "super-secret-token", "Cookie", "session=abc", "X-Custom-Debug"} {
			if strings.Contains(got, banned) {
				t.Fatalf("输出不得包含 %q：%s", banned, got)
			}
		}
	})

	t.Run("Referer 保留路径但剔除凭据、query 与 fragment", func(t *testing.T) {
		header := http.Header{}
		header.Set("Referer", "https://user:password@example.test/private/path?access_token=signed-secret#fragment")
		parsed := parseCapturedHeaders(t, CaptureHeaders(header))
		if parsed["Referer"] != "https://example.test/private/path" {
			t.Fatalf("Referer 应移除 userinfo/query/fragment，实际 %q", parsed["Referer"])
		}
		for _, secret := range []string{"password", "access_token", "signed-secret", "fragment"} {
			if strings.Contains(CaptureHeaders(header), secret) {
				t.Fatalf("Referer 不得泄露 %q", secret)
			}
		}
	})

	t.Run("非绝对 Referer 丢弃", func(t *testing.T) {
		header := http.Header{}
		header.Set("Referer", "/private/path?token=secret")
		if got := CaptureHeaders(header); got != "" {
			t.Fatalf("非绝对 Referer 应丢弃，实际 %q", got)
		}
	})

	t.Run("命中 sensitiveKeys 的头名即使进了白名单也跳过（纵深防御）", func(t *testing.T) {
		// 模拟白名单演进时误加入敏感头名：CaptureHeaders 必须按 sensitiveKeys 二次拦截。
		saved := headerAllowlist
		headerAllowlist = append(append([]string(nil), saved...), "Authorization", "Token")
		t.Cleanup(func() { headerAllowlist = saved })
		header := http.Header{}
		header.Set("Authorization", "Bearer ak-live-123456")
		header.Set("Token", "tk-live-abcdef")
		header.Set("User-Agent", "agent/2.0")
		got := CaptureHeaders(header)
		for _, banned := range []string{"Authorization", "ak-live-123456", "tk-live-abcdef"} {
			if strings.Contains(got, banned) {
				t.Fatalf("命中敏感名单的头 %q 不得入列：%s", banned, got)
			}
		}
		if parsed := parseCapturedHeaders(t, got); parsed["User-Agent"] != "agent/2.0" {
			t.Fatalf("非敏感白名单头应保留，实际 %+v", parsed)
		}
	})

	t.Run("无白名单头命中时返回空串", func(t *testing.T) {
		if got := CaptureHeaders(http.Header{}); got != "" {
			t.Fatalf("空请求应返回空串，got=%q", got)
		}
	})
}

func TestCaptureHeadersTruncatesLongValues(t *testing.T) {
	t.Run("单值超 256 字符按字符截断且保持合法 UTF-8", func(t *testing.T) {
		header := http.Header{}
		// 用中文（3 字节/字符）验证按 rune 截断而不是按字节硬切。
		header.Set("User-Agent", strings.Repeat("超", 300))
		parsed := parseCapturedHeaders(t, CaptureHeaders(header))
		value := parsed["User-Agent"]
		if !utf8.ValidString(value) {
			t.Fatalf("截断后必须是合法 UTF-8：%q", value)
		}
		if runes := []rune(value); len(runes) != maxHeaderValueRunes+1 {
			t.Fatalf("截断后应为 %d 字符（上限 + 省略号），实际 %d", maxHeaderValueRunes+1, len(runes))
		}
		if !strings.HasSuffix(value, "…") {
			t.Fatalf("截断应以省略号结尾：%q", value)
		}
	})

	t.Run("多值头合并后同样截断", func(t *testing.T) {
		header := http.Header{}
		header["X-Forwarded-For"] = []string{strings.Repeat("1", 200), strings.Repeat("2", 200)}
		parsed := parseCapturedHeaders(t, CaptureHeaders(header))
		if runes := []rune(parsed["X-Forwarded-For"]); len(runes) != maxHeaderValueRunes+1 {
			t.Fatalf("多值头合并后也应截断到 %d 字符，实际 %d", maxHeaderValueRunes+1, len(runes))
		}
	})

	t.Run("未超限的值原样保留", func(t *testing.T) {
		header := http.Header{}
		header.Set("Origin", "https://artifact.example.com")
		parsed := parseCapturedHeaders(t, CaptureHeaders(header))
		if parsed["Origin"] != "https://artifact.example.com" {
			t.Fatalf("短值不应被改动，实际 %q", parsed["Origin"])
		}
	})
}

func TestHeadersMiddlewareAndHeadersFrom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/repositories", nil)
	c.Request.Header.Set("Accept", "application/json")
	c.Request.Header.Set("Authorization", "Bearer secret")
	// 未经过中间件时回退空串。
	if got := HeadersFrom(c); got != "" {
		t.Fatalf("未捕获时应为空串，got=%q", got)
	}
	HeadersMiddleware()(c)
	got := HeadersFrom(c)
	parsed := parseCapturedHeaders(t, got)
	if parsed["Accept"] != "application/json" {
		t.Fatalf("中间件应捕获白名单头，实际 %+v", parsed)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "Authorization") {
		t.Fatalf("中间件捕获不得包含凭据：%s", got)
	}
}

func TestServerDurationMs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/repositories", nil)
	// 未进入业务路由处理（无起点）时为 0。
	if got := ServerDurationMs(c); got != 0 {
		t.Fatalf("无起点应返回 0，got=%d", got)
	}
	// 模拟 50ms 前进入业务路由处理。
	c.Set(ServerStartKey, time.Now().Add(-50*time.Millisecond))
	if got := ServerDurationMs(c); got < 50 {
		t.Fatalf("分段耗时应 ≥50ms，got=%d", got)
	}
	// 起点之后的总耗时（DurationMs）此时没有 StartKey，仍为 0，两键互不干扰。
	if got := DurationMs(c); got != 0 {
		t.Fatalf("StartKey 未设置时总耗时应为 0，got=%d", got)
	}
}
