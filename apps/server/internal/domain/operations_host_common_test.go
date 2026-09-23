package domain

import (
	"testing"
	"time"
)

// TestParseMeminfoExtractsTotalAndAvailable 验证 meminfo 解析口径：
// 只认 MemTotal/MemAvailable 且 kB 统一换算为字节。
func TestParseMeminfoExtractsTotalAndAvailable(t *testing.T) {
	const content = "MemTotal:       16384000 kB\nMemFree:         1024000 kB\nMemAvailable:    6144000 kB\n"
	total, available, ok := parseMeminfo(content)
	if !ok {
		t.Fatal("meminfo 应解析成功")
	}
	if total != 16384000*1024 {
		t.Fatalf("内存总量 = %d，期望 %d", total, 16384000*1024)
	}
	if available != 6144000*1024 {
		t.Fatalf("内存可用量 = %d，期望 %d", available, 6144000*1024)
	}
}

// TestParseMeminfoFailsWithoutAvailable 缺少 MemAvailable 时必须整体判失败，
// 由上层给出空值而不是只返回一半数据。
func TestParseMeminfoFailsWithoutAvailable(t *testing.T) {
	if _, _, ok := parseMeminfo("MemTotal: 16384000 kB\n"); ok {
		t.Fatal("缺少 MemAvailable 时应解析失败")
	}
}

// TestParseProcStatBtimeExtractsBootSeconds 验证 /proc/stat 的 btime 行解析。
func TestParseProcStatBtimeExtractsBootSeconds(t *testing.T) {
	const content = "cpu  100 0 100 9900 0 0 0 0 0 0\ncpu0 100 0 100 9900 0 0 0 0 0 0\nbtime 1699990000\n"
	boot, ok := parseProcStatBtime(content)
	if !ok || boot != 1699990000 {
		t.Fatalf("btime = %d（ok=%v），期望 1699990000", boot, ok)
	}
	if _, ok := parseProcStatBtime("cpu 1 2 3\n"); ok {
		t.Fatal("缺少 btime 行时应解析失败")
	}
}

// TestParseProcSelfStatExtractsCpuAndStartTicks 验证字段 14/15（utime/stime）
// 与字段 22（starttime）的位置口径。
func TestParseProcSelfStatExtractsCpuAndStartTicks(t *testing.T) {
	const content = "1234 (gotest) S 0 1 1 0 -1 4194560 100 0 0 0 500 200 0 0 20 0 8 0 98765 10000000 0"
	cpuTicks, startTicks, cpuOK, startOK := parseProcSelfStat(content)
	if !cpuOK || cpuTicks != 700 {
		t.Fatalf("CPU 滴答 = %d（ok=%v），期望 700", cpuTicks, cpuOK)
	}
	if !startOK || startTicks != 98765 {
		t.Fatalf("starttime = %d（ok=%v），期望 98765", startTicks, startOK)
	}
}

// TestParseProcSelfStatFailsClosed 缺 starttime 时 startOK 必须为 false，
// 上层据此给空值而不是把 0 当作启动滴答参与计算（CPU 滴答不受影响仍可解析）。
func TestParseProcSelfStatFailsClosed(t *testing.T) {
	const content = "1234 (gotest) S 0 1 1 0 -1 4194560 100 0 0 0 500 200"
	cpuTicks, _, cpuOK, startOK := parseProcSelfStat(content)
	if !cpuOK || cpuTicks != 700 {
		t.Fatalf("CPU 滴答 = %d（ok=%v），期望 700", cpuTicks, cpuOK)
	}
	if startOK {
		t.Fatal("缺 starttime 时 startOK 应为 false")
	}
}

// TestProcessUptimeSecondsComputesWallClockDuration 验证运行时长口径：
// 当前时刻 −（btime + starttime/每秒滴答），负值与非法滴答率安全处理。
func TestProcessUptimeSecondsComputesWallClockDuration(t *testing.T) {
	// 启动时刻 = 1699990000 + 500/100 = 1699990005，运行时长 = 1700000000 − 1699990005 = 9995。
	if got := processUptimeSeconds(1700000000, 1699990000, 500, 100); got != 9995 {
		t.Fatalf("运行时长 = %d，期望 9995", got)
	}
	if got := processUptimeSeconds(1699990000, 1700000000, 0, 100); got != 0 {
		t.Fatalf("负值应钳制为 0，得 %d", got)
	}
	if got := processUptimeSeconds(1700000000, 1699990000, 500, 0); got != 0 {
		t.Fatalf("每秒滴答为 0 时应返回 0，得 %d", got)
	}
}

// TestFiletimeUnixSecondsConvertsWindowsEpoch 验证 FILETIME（1601 纪元 100ns）
// 到 Unix 秒的换算，含纪元前非法值的兜底。
func TestFiletimeUnixSecondsConvertsWindowsEpoch(t *testing.T) {
	if got := filetimeUnixSeconds(windowsEpochToUnix100ns); got != 0 {
		t.Fatalf("纪元差本身应换算为 0，得 %d", got)
	}
	if got := filetimeUnixSeconds(windowsEpochToUnix100ns + 1700000000*10000000); got != 1700000000 {
		t.Fatalf("换算结果 = %d，期望 1700000000", got)
	}
	if got := filetimeUnixSeconds(1); got != 0 {
		t.Fatalf("纪元前非法值应兜底为 0，得 %d", got)
	}
}

// TestProcessUptimeFromCreatedUsesWallClockNow 验证 Windows 侧运行时长 =
// 采集时刻 − 创建时刻，并对时钟回拨的负值钳制为 0。
func TestProcessUptimeFromCreatedUsesWallClockNow(t *testing.T) {
	now := time.Unix(1700000100, 0)
	created := uint64(windowsEpochToUnix100ns) + 1700000000*10000000
	if got := processUptimeFromCreated(now, created); got != 100 {
		t.Fatalf("运行时长 = %d，期望 100", got)
	}
	future := uint64(windowsEpochToUnix100ns) + 1700000900*10000000
	if got := processUptimeFromCreated(now, future); got != 0 {
		t.Fatalf("创建时间晚于采集时刻时应钳制为 0，得 %d", got)
	}
}
