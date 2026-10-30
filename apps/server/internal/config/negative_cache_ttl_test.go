package config

import (
	"testing"
	"time"
)

// TestNegativeCacheTTLParsing 覆盖 JIAN_NEGATIVE_CACHE_TTL 的解析口径。
//
// 本项**有禁用语义**：负缓存是纯性能优化，关掉它最坏只是多几次回源，而排查
// 「制品明明有却报 404」时运维需要一个不看记忆、只跑真实链路的开关。因此显式 0
// 原样生效（关闭），只有缺省 / 非数字 / 负数回落 60s。
//
// 负数**不**当成关闭：关闭是明确的运维意图，应当用 0 表达；把 -600 这类笔误解读成
// 关闭，等于一次手误就静默放大上游压力（每次缺失请求都探测全部 group 成员）。
func TestNegativeCacheTTLParsing(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为六十秒", value: "", want: 60 * time.Second},
		{name: "显式零即关闭", value: "0", want: 0},
		{name: "负数回落默认", value: "-1", want: 60 * time.Second},
		{name: "非数字回落默认", value: "60s", want: 60 * time.Second},
		{name: "空白回落默认", value: "   ", want: 60 * time.Second},
		{name: "自定义秒数", value: "600", want: 600 * time.Second},
		{name: "两侧空白被裁剪", value: "  120  ", want: 120 * time.Second},
		// 上限夹取：group 层负缓存不因成员后续上传而失效，TTL 笔误成一天会让新发布的制品
		// 长期 404 且不自愈，因此超限夹取到 3600s 并打警告（与 capacityCacheTTL 不夹取的取舍不同）。
		{name: "恰好上限原样生效", value: "3600", want: 3600 * time.Second},
		{name: "超上限夹取到一小时", value: "86400", want: 3600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvNegativeCacheTTL, tc.value)
			if got := negativeCacheTTL(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvNegativeCacheTTL, tc.value, got, tc.want)
			}
		})
	}
}

// TestLoadCarriesNegativeCacheTTL 验证配置值确实被装配到 Config 上；
// 特别注意 0（关闭）必须原样带到字段上，不能被「零值即未设置」的写法吞掉。
func TestLoadCarriesNegativeCacheTTL(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "默认值", value: "", want: 60 * time.Second},
		{name: "显式值", value: "120", want: 120 * time.Second},
		{name: "零即关闭", value: "0", want: 0},
		{name: "超上限被夹取", value: "86400", want: 3600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDataDir, t.TempDir())
			t.Setenv(EnvNegativeCacheTTL, tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load：%v", err)
			}
			if cfg.NegativeCacheTTL != tc.want {
				t.Fatalf("Config.NegativeCacheTTL = %s，期望 %s", cfg.NegativeCacheTTL, tc.want)
			}
		})
	}
}
