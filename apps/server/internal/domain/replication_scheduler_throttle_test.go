package domain

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// TestReplicationSchedulerErrorThrottleIsolatedByPeer 验证不同对端的成功/失败互不重置收缩状态。
func TestReplicationSchedulerErrorThrottleIsolatedByPeer(t *testing.T) {
	s := NewReplicationScheduler(nil, nil, nil, time.Millisecond)
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	peerA := "http://peer-a"
	peerB := "http://peer-b"
	err := errors.New("拉取复制变更失败：HTTP 521")
	s.logSyncErr(peerA, 0, err)
	s.logSyncErr(peerA, 0, err)

	buf.Reset()
	s.logSyncOK(peerB)
	if strings.Contains(buf.String(), "同步恢复") {
		t.Fatalf("对端 B 成功不得汇总对端 A 的失败：%s", buf.String())
	}

	s.logSyncErr(peerA, 0, err)
	if strings.Contains(buf.String(), "同步失败（对端") {
		t.Fatalf("对端 A 的连续失败状态不应被对端 B 重置：%s", buf.String())
	}
}

// TestReplicationSchedulerErrorThrottle 验证连续相同错误的日志收缩：
//   - 首次失败打印一条完整日志；
//   - 后续相同失败不逐条打印（仅每 errProgressEvery 次打一条进度）；
//   - 错误变化时先汇总上一次连续失败（起始→终止 + 次数）；
//   - 恢复时打印汇总并清空状态。
func TestReplicationSchedulerErrorThrottle(t *testing.T) {
	s := NewReplicationScheduler(nil, nil, nil, time.Millisecond)

	// 重定向标准日志到 buffer。
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	defer func() { _ = os.Stdout.Sync() }()

	peer := "http://127.0.0.1:1"
	err := errors.New("拉取复制变更失败：HTTP 521")

	// 1) 首次失败：打印一条完整日志。
	s.logSyncErr(peer, 0, err)
	if n := strings.Count(buf.String(), "复制调度：同步失败（对端"); n != 1 {
		t.Fatalf("首次失败应打印 1 条完整日志，实际 %d 条", n)
	}

	// 2) 相同错误连续 19 次（累计 20 次）：除首条外，仅在第 20 次打一条进度。
	for i := 0; i < errProgressEvery-1; i++ {
		s.logSyncErr(peer, 0, err)
	}
	// 累计 errProgressEvery 次：应有 1 条首错 + 1 条进度（第 20 次触发）。
	if got := strings.Count(buf.String(), "复制调度：同步失败（对端"); got != 1 {
		t.Fatalf("连续相同错误不应逐条打印，首错应保持 1 条，实际 %d 条", got)
	}
	if !strings.Contains(buf.String(), "复制调度：同步持续失败") {
		t.Fatal("第 20 次连续失败应打一条进度日志")
	}

	// 3) 错误变化：先汇总上一次（起始→终止 + 次数），再打新错误。
	buf.Reset()
	err2 := errors.New("拉取复制变更失败：HTTP 500")
	s.logSyncErr(peer, 0, err2)
	if !strings.Contains(buf.String(), "复制调度：同步失败告一段落") {
		t.Fatal("错误变化应先打印上一次连续失败的汇总")
	}
	if !strings.Contains(buf.String(), "共 20 次") {
		t.Fatalf("汇总应包含连续次数 20，实际：%s", buf.String())
	}
	if !strings.Contains(buf.String(), "自 ") || !strings.Contains(buf.String(), " 至 ") {
		t.Fatalf("汇总应包含起始→终止时间，实际：%s", buf.String())
	}
	if !strings.Contains(buf.String(), "HTTP 500") {
		t.Fatalf("错误变化后应打印新错误，实际：%s", buf.String())
	}

	// 4) 恢复：打印汇总并清空状态（后续失败视为新的首错）。
	buf.Reset()
	s.logSyncOK(peer)
	if !strings.Contains(buf.String(), "复制调度：同步恢复") {
		t.Fatalf("恢复应打印汇总，实际：%s", buf.String())
	}
	if _, exists := s.errStates[peer]; exists {
		t.Fatal("恢复后应清空连续失败状态")
	}

	// 5) 清空后再次失败：视为新的首错（打印完整日志而非进度/汇总）。
	buf.Reset()
	s.logSyncErr(peer, 0, err)
	if n := strings.Count(buf.String(), "复制调度：同步失败（对端"); n != 1 {
		t.Fatalf("清空后的首次失败应打印 1 条完整日志，实际 %d 条", n)
	}
	if strings.Contains(buf.String(), "告一段落") || strings.Contains(buf.String(), "持续失败") {
		t.Fatal("清空后的首次失败不应含汇总/进度")
	}
}
