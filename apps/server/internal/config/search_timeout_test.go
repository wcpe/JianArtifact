package config

import (
	"testing"
	"time"
)

// TestSearchTimeoutDefaultsAndRejectsNonPositive 覆盖的搜索预算解析：
// 本项**无禁用语义**——搜索是无界查询，关掉超时等于开放一个可以无限占用唯一数据库连接的
// 入口，因此缺省、非数字、负数与显式 0 一律回落默认 10s（口径同 autoBlockBase）。
func TestSearchTimeoutDefaultsAndRejectsNonPositive(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为十秒", value: "", want: 10 * time.Second},
		{name: "零回落默认", value: "0", want: 10 * time.Second},
		{name: "负数回落默认", value: "-10", want: 10 * time.Second},
		{name: "非数字回落默认", value: "10s", want: 10 * time.Second},
		{name: "自定义秒数", value: "3", want: 3 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvSearchTimeoutSeconds, tc.value)
			if got := searchTimeout(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvSearchTimeoutSeconds, tc.value, got, tc.want)
			}
		})
	}
}
