package main

import (
	"context"
	"testing"
	"time"
)

func TestStartBlobGCTaskDoesNotScanAtStartupAndRunsOnInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 2)
	startBlobGCTask(ctx, 80*time.Millisecond, func() (int, error) {
		calls <- struct{}{}
		return 1, nil
	})

	select {
	case <-calls:
		t.Fatal("blob 定时清理不得在启动时立即扫描")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-calls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("primary blob 定时清理未按间隔触发")
	}

	cancel()
	select {
	case <-calls:
		t.Fatal("context 取消后 blob 定时清理不得继续运行")
	case <-time.After(120 * time.Millisecond):
	}
}
