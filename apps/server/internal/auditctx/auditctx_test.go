package auditctx

import (
	"strings"
	"testing"
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
