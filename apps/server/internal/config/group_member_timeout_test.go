package config

import (
	"testing"
	"time"
)

// TestGroupMemberTimeoutParsing 覆盖 JIAN_GROUP_MEMBER_TIMEOUT 的解析口径。
//
// 本项**无禁用语义**：0 秒确认预算等于「每个成员都立刻超时」，整条 group 读会退化成
// 不可判定失败，只因一处手误就会让整个 group 仓库不可用。因此缺省 / 非数字 / 负数 /
// 显式 0 一律回落默认 3s，口径同 upstreamTimeout / autoBlockBase / searchTimeout。
func TestGroupMemberTimeoutParsing(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为三秒", value: "", want: 3 * time.Second},
		{name: "显式零回落默认", value: "0", want: 3 * time.Second},
		{name: "负数回落默认", value: "-3", want: 3 * time.Second},
		{name: "非数字回落默认", value: "3s", want: 3 * time.Second},
		{name: "空白回落默认", value: "   ", want: 3 * time.Second},
		{name: "自定义秒数", value: "8", want: 8 * time.Second},
		{name: "两侧空白被裁剪", value: "  10  ", want: 10 * time.Second},
		// 上限夹取：调大本项会把缺失路径的响应时间按上限放大（成员不可达时每个成员都要等满），
		// 因此超限夹取到 60s 并打警告，而不是原样生效。
		{name: "恰好上限原样生效", value: "60", want: 60 * time.Second},
		{name: "超上限夹取到一小时", value: "7200", want: 3600 * time.Second},
		{name: "恰好等于上限不夹取", value: "3600", want: 3600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvGroupMemberTimeout, tc.value)
			if got := groupMemberTimeout(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvGroupMemberTimeout, tc.value, got, tc.want)
			}
		})
	}
}

// TestLoadCarriesGroupMemberTimeout 验证配置值确实被装配到 Config 上。
// 解析函数单测绿了但字段没赋值，是这类改动最典型的静默失灵（口径同
// TestLoadCarriesCapacityCacheTTL）。
func TestLoadCarriesGroupMemberTimeout(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "默认值", value: "", want: 3 * time.Second},
		{name: "显式值", value: "8", want: 8 * time.Second},
		{name: "超上限被夹取", value: "7200", want: 3600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDataDir, t.TempDir())
			t.Setenv(EnvGroupMemberTimeout, tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load：%v", err)
			}
			if cfg.GroupMemberTimeout != tc.want {
				t.Fatalf("Config.GroupMemberTimeout = %s，期望 %s", cfg.GroupMemberTimeout, tc.want)
			}
		})
	}
}
