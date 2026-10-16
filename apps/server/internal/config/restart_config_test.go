package config

import "testing"

// 一键重启的配置解析（FR-40 追加七）：单元名缺省 jianartifact，作用域缺省 system。
func TestRestartUnitDefaultsToJianartifact(t *testing.T) {
	t.Setenv(EnvRestartUnit, "")
	if got := envOr(EnvRestartUnit, defaultRestartUnit); got != "jianartifact" {
		t.Fatalf("缺省单元名应为 jianartifact，得 %q", got)
	}
	t.Setenv(EnvRestartUnit, "jianartifact-prod")
	if got := envOr(EnvRestartUnit, defaultRestartUnit); got != "jianartifact-prod" {
		t.Fatalf("自定义单元名应被采纳，得 %q", got)
	}
}

// 作用域只接受三个合法取值，其余（含空、大小写变体、拼写错误）一律回落 system。
//
// 回落而不是报错是刻意的：作用域写错时应当退化成最保守的 system 再交给
// 请求期同步预检判定可用性，由预检如实回 supported:false + reason，
// 而不是让服务因为一个拼错的枚举值拒绝启动。
func TestRestartScopeFallsBackToSystem(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "缺省为 system", value: "", want: "system"},
		{name: "system 原样", value: "system", want: "system"},
		{name: "systemd-user 原样", value: "systemd-user", want: "systemd-user"},
		{name: "docker 原样", value: "docker", want: "docker"},
		{name: "两侧空白被裁剪", value: "  docker  ", want: "docker"},
		{name: "拼写错误回落", value: "systemctl", want: "system"},
		{name: "大小写变体回落", value: "System", want: "system"},
		{name: "未知取值回落", value: "kubernetes", want: "system"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvRestartScope, tc.value)
			if got := restartScope(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%q，期望=%q", EnvRestartScope, tc.value, got, tc.want)
			}
		})
	}
}
