package config

import (
	"testing"
	"time"
)

func TestBlobGCIntervalDefaultsAndAllowsExplicitDisable(t *testing.T) {
	t.Run("缺省为二十四小时", func(t *testing.T) {
		t.Setenv(EnvBlobGCInterval, "")
		if got := blobGCInterval(); got != 24*time.Hour {
			t.Fatalf("缺省 blob GC 间隔=%s，期望=%s", got, 24*time.Hour)
		}
	})
	t.Run("零禁用", func(t *testing.T) {
		t.Setenv(EnvBlobGCInterval, "0")
		if got := blobGCInterval(); got != 0 {
			t.Fatalf("JIAN_BLOB_GC_INTERVAL=0 应禁用，实际=%s", got)
		}
	})
}
