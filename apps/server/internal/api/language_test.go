package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestLang(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		header string
		want   string
	}{
		{"", "zh"},                        // 缺失头 → 中文
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh"}, // 中文优先
		{"en-US,en;q=0.9", "en"},          // 英文
		{"en", "en"},
		{"zh", "zh"},
		{"ja,de;q=0.9", "zh"},      // 无对应资源 → 回退中文
		{"ja,en;q=0.9", "en"},      // 首个可识别项（en）胜过不可识别的 ja
		{"*", "zh"},                // 通配 → 回退中文
		{"EN-GB", "en"},            // 大小写不敏感
		{"zh-Hant,en;q=0.5", "zh"}, // 繁体归中文
	}
	for _, tc := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			c.Request.Header.Set("Accept-Language", tc.header)
		}
		if got := requestLang(c); got != tc.want {
			t.Errorf("Accept-Language=%q：期望 %q，实际 %q", tc.header, tc.want, got)
		}
	}
}
