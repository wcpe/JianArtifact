package config

import (
	"testing"
	"time"
)

// TestAutoBlockBaseDefaultsAndRejectsNonPositive 覆盖 FR-43 的退避起始解析：
// 与 storageCleanupInterval 的「0 禁用」不同，本项**无禁用语义**——
// 退避起始为 0 等于失败不阻止，没有意义，因此 0 与负数一律回落默认 40s。
func TestAutoBlockBaseDefaultsAndRejectsNonPositive(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为四十秒", value: "", want: 40 * time.Second},
		{name: "零回落默认", value: "0", want: 40 * time.Second},
		{name: "负数回落默认", value: "-40", want: 40 * time.Second},
		{name: "非数字回落默认", value: "40s", want: 40 * time.Second},
		{name: "自定义秒数", value: "5", want: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAutoBlockBaseSeconds, tc.value)
			if got := autoBlockBase(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvAutoBlockBaseSeconds, tc.value, got, tc.want)
			}
		})
	}
}
