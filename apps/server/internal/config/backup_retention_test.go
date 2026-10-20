package config

import (
	"testing"
	"time"
)

// TestBackupAutoIntervalDefaultsToEnabled 覆盖 FR-40 追加十一 的默认值变更。
//
// 默认值从 0（关闭）改为 1 天（开启）：实测反馈是多数实例期望"系统自己会备份"，
// 而默认关闭意味着**用户想起要配置之前实例没有任何备份**，出事时无可恢复。
// 保留策略（7 份 / 30 天）把占用钉住了上限，因此默认开的风险可控。
//
// 关闭能力**没有改变**：显式 0 仍是关闭，且非法值回落到默认（现在是开启）——
// 这一条要特别守住：如果非法输入也变成"关闭"，一个笔误就会静默停掉备份。
func TestBackupAutoIntervalDefaultsToEnabled(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为开启", value: "", want: 24 * time.Hour},
		{name: "显式零仍是关闭", value: "0", want: 0},
		{name: "非数字回落默认（开启）", value: "每天", want: 24 * time.Hour},
		{name: "负数回落默认（开启）", value: "-3600", want: 24 * time.Hour},
		{name: "自定义间隔", value: "86400", want: 24 * time.Hour},
		{name: "自定义更短间隔", value: "1800", want: 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvBackupAutoInterval, tc.value)
			if got := backupAutoInterval(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvBackupAutoInterval, tc.value, got, tc.want)
			}
		})
	}
}

func TestBackupKeepLastDefaultsAndZeroMeansUnlimited(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int
	}{
		{name: "缺省为七份", value: "", want: 7},
		// 0 = 不按份数限制（只受天数约束），绝不是"一份都不留"。
		{name: "零表示不限制", value: "0", want: 0},
		{name: "非数字取默认", value: "seven", want: 7},
		{name: "负数取默认", value: "-3", want: 7},
		{name: "自定义份数", value: "30", want: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvBackupKeepLast, tc.value)
			if got := backupKeepLast(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%d，期望=%d", EnvBackupKeepLast, tc.value, got, tc.want)
			}
		})
	}
}

func TestBackupRetentionDaysDefaultsAndZeroMeansUnlimited(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int
	}{
		{name: "缺省为三十天", value: "", want: 30},
		// 0 = 不限天数（只受份数约束），与 JIAN_BACKUP_KEEP_LAST 的 0 语义同构。
		{name: "零表示不限制", value: "0", want: 0},
		{name: "非数字取默认", value: "30d", want: 30},
		{name: "负数取默认", value: "-30", want: 30},
		{name: "自定义天数", value: "90", want: 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvBackupRetentionDays, tc.value)
			if got := backupRetentionDays(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%d，期望=%d", EnvBackupRetentionDays, tc.value, got, tc.want)
			}
		})
	}
}
