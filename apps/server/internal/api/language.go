package api

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// requestLang 从 Accept-Language 解析请求语言，只支持 zh / en 两种。
//
// 与前端语言策略保持一致：逐个语言标签取**第一个能识别的**，`zh*` 归中文、`en*` 归英文；
// 其余语言（没有对应资源）与缺失请求头一律回退中文——避免把看不懂英文的访问者推进英文内容。
// 刻意不解析 q 权重：浏览器本就按偏好降序排列，取首个可识别项即可，且少一处解析逻辑。
func requestLang(c *gin.Context) string {
	for _, part := range strings.Split(c.GetHeader("Accept-Language"), ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		lower := strings.ToLower(tag)
		if strings.HasPrefix(lower, "zh") {
			return "zh"
		}
		if strings.HasPrefix(lower, "en") {
			return "en"
		}
	}
	return "zh"
}
