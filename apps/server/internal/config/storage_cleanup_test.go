package config

import (
	"testing"
	"time"
)

func TestStorageCleanupIntervalDefaultsAndAllowsExplicitDisable(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为二十四小时", value: "", want: 24 * time.Hour},
		{name: "零禁用", value: "0", want: 0},
		{name: "非数字取默认", value: "每个小时", want: 24 * time.Hour},
		{name: "负数取默认", value: "-60", want: 24 * time.Hour},
		{name: "自定义间隔", value: "3600", want: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvStorageCleanupInterval, tc.value)
			if got := storageCleanupInterval(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvStorageCleanupInterval, tc.value, got, tc.want)
			}
		})
	}
}

func TestStorageMetadataRetentionDefaultsAndAllowsExplicitDisable(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为七天", value: "", want: 7 * 24 * time.Hour},
		{name: "零禁用", value: "0", want: 0},
		{name: "非数字取默认", value: "seven", want: 7 * 24 * time.Hour},
		{name: "负数取默认", value: "-1", want: 7 * 24 * time.Hour},
		{name: "自定义天数", value: "30", want: 30 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvStorageMetadataRetention, tc.value)
			if got := storageMetadataRetention(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvStorageMetadataRetention, tc.value, got, tc.want)
			}
		})
	}
}

func TestStorageTempMaxAgeDefaultsAndAllowsExplicitDisable(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "缺省为二十四小时", value: "", want: 24 * time.Hour},
		{name: "零禁用", value: "0", want: 0},
		{name: "非数字取默认", value: "1h", want: 24 * time.Hour},
		{name: "负数取默认", value: "-24", want: 24 * time.Hour},
		{name: "自定义小时", value: "6", want: 6 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvStorageTempMaxAge, tc.value)
			if got := storageTempMaxAge(); got != tc.want {
				t.Fatalf("%s=%q 解析结果=%s，期望=%s", EnvStorageTempMaxAge, tc.value, got, tc.want)
			}
		})
	}
}
