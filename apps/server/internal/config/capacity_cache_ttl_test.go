package config

import (
	"testing"
	"time"
)

// TestCapacityCacheTTLParsing 覆盖 JIAN_CAPACITY_CACHE_TTL_SECONDS 的解析口径。
//
// 本项**有禁用语义**（与 searchTimeout / autoBlockBase 的「0 也回落默认」相反）：
// 容量缓存不是授权数据（鉴权与 ACL 逐请求读库），禁用它的代价只是每次多跑一次有界聚合，
// 并不会像关闭搜索超时那样放开一个可无限占用数据库连接的入口；而「要实时数字」是真实的
// 运维需求（批量导入 / 迁移核对期间）。因此显式 0 原样生效，只有缺省 / 非数字 / 负数回落 30s。
func TestCapacityCacheTTLParsing(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为三十秒", value: "", want: 30 * time.Second},
		{name: "显式零即禁用", value: "0", want: 0},
		{name: "负数回落默认", value: "-1", want: 30 * time.Second},
		{name: "非数字回落默认", value: "30s", want: 30 * time.Second},
		{name: "空白回落默认", value: "   ", want: 30 * time.Second},
		{name: "自定义秒数", value: "5", want: 5 * time.Second},
		{name: "两侧空白被裁剪", value: "  60  ", want: 60 * time.Second},
		// 不设上限夹取：超长 TTL 只影响展示数字的新鲜度，静默改写运维显式给出的值是更糟的语义
		// （口径同拒绝给 auto-block 退避加上限）。
		{name: "超大值原样生效", value: "86400", want: 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCapacityCacheTTLSeconds, tc.value)
			if got := capacityCacheTTL(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvCapacityCacheTTLSeconds, tc.value, got, tc.want)
			}
		})
	}
}

// TestLoadCarriesCapacityCacheTTL 验证配置值确实被装配到 Config 上：
// 解析函数单测绿了但字段没赋值，是这类改动最典型的静默失灵。
func TestLoadCarriesCapacityCacheTTL(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "显式值", value: "5", want: 5 * time.Second},
		{name: "默认值", value: "", want: 30 * time.Second},
		{name: "零即禁用", value: "0", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDataDir, t.TempDir())
			t.Setenv(EnvCapacityCacheTTLSeconds, tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load：%v", err)
			}
			if cfg.CapacityCacheTTL != tc.want {
				t.Fatalf("Config.CapacityCacheTTL = %s，期望 %s", cfg.CapacityCacheTTL, tc.want)
			}
		})
	}
}
