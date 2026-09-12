package auditctx

import (
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
