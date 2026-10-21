package config

import (
	"testing"
	"time"
)

// TestBackupAutoIntervalDefaultsToDisabled 覆盖自动备份间隔的默认值与回落规则。
//
// 默认关闭是**有意**的：备份是全量包（每份 ≈ 数据目录大小），默认开启会在用户
// 毫无预期时持续消耗磁盘。实测存在"20GB 数据 / 40GB 磁盘"这类配置——默认开几天
// 就能把盘写满，而盘满会连带把服务写挂。反面（默认关闭、出事时没备份）可以由运维
// 事后补配置修复；盘被写满则是运行故障。两害相权，默认关闭。
func TestBackupAutoIntervalDefaultsToDisabled(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为关闭", value: "", want: 0},
		{name: "显式零关闭", value: "0", want: 0},
		// 非法值回落到默认（关闭）。这与"保留份数非法回落 7"的口径一致：
		// 回落目标是本项的默认值，而不是某个看起来更安全的值。
		{name: "非数字回落默认（关闭）", value: "每天", want: 0},
		{name: "负数回落默认（关闭）", value: "-3600", want: 0},
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
