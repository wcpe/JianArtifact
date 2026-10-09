package config

import (
	"testing"
	"time"
)

func TestBackupAutoIntervalDefaultsToDisabled(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		// 与 storage-cleanup 的「默认开、0 关」不同：自动备份默认关闭，必须显式开启。
		{name: "缺省为关闭", value: "", want: 0},
		{name: "零关闭", value: "0", want: 0},
		{name: "非数字取默认关闭", value: "每天", want: 0},
		{name: "负数取默认关闭", value: "-3600", want: 0},
		{name: "自定义间隔", value: "86400", want: 24 * time.Hour},
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
