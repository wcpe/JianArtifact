package domain

import (
	"testing"
	"time"
)

// TestParseProcNetDevReturnsPerInterfaceSkippingLoopback 验证 /proc/net/dev 解析口径：
// 返回**逐网卡**累计计数，跳过表头行与回环 lo；接收/发送字节取自冒号后第 1、第 9 个字段。
func TestParseProcNetDevReturnsPerInterfaceSkippingLoopback(t *testing.T) {
	const content = "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo:  123456     1000    0    0    0     0          0         0   123456     1000    0    0    0     0       0          0\n" +
		"  eth0: 9876543    54321    7    0    0     0          0         0  4567890    43210    3    0    0     0       0          0\n" +
		" wlan0:  111111     2222    0    0    0     0          0         0    33333     2222    0    0    0     0       0          0\n"
	interfaces := parseProcNetDev(content)
	if len(interfaces) != 2 {
		t.Fatalf("网卡数 = %d，期望 2（跳过 lo 与表头）：%+v", len(interfaces), interfaces)
	}
	if interfaces[0].Name != "eth0" || interfaces[0].ReceiveBytes != 9876543 || interfaces[0].TransmitBytes != 4567890 {
		t.Fatalf("eth0 解析错误：%+v", interfaces[0])
	}
	if interfaces[1].Name != "wlan0" || interfaces[1].ReceiveBytes != 111111 || interfaces[1].TransmitBytes != 33333 {
		t.Fatalf("wlan0 解析错误：%+v", interfaces[1])
	}
}

// TestParseProcNetDevSkipsMalformedLines 畸形行（字段不足或计数非法）必须整行跳过，
// 不得污染其他网卡的计数，也不得把解析失败伪装成 0。
func TestParseProcNetDevSkipsMalformedLines(t *testing.T) {
	const content = "  eth0: 1 2 3\n" +
		"  eth1: notanumber 1 1 1 1 1 1 1 2 2 2 2\n" +
		"  eth2: 5 0 0 0 0 0 0 0 7 0 0 0 0 0 0\n"
	interfaces := parseProcNetDev(content)
	if len(interfaces) != 1 || interfaces[0].Name != "eth2" || interfaces[0].ReceiveBytes != 5 || interfaces[0].TransmitBytes != 7 {
		t.Fatalf("非法行应被跳过：%+v", interfaces)
	}
}

// TestSumNetworkInterfacesAggregates 验证逐网卡累计计数求和为「全部网卡」聚合值，空列表为 0。
func TestSumNetworkInterfacesAggregates(t *testing.T) {
	received, transmitted := sumNetworkInterfaces([]HostNetworkInterface{
		{Name: "eth0", ReceiveBytes: 100, TransmitBytes: 10},
		{Name: "wlan0", ReceiveBytes: 200, TransmitBytes: 20},
	})
	if received != 300 || transmitted != 30 {
		t.Fatalf("聚合 = (%d, %d)，期望 (300, 30)", received, transmitted)
	}
	if received, transmitted := sumNetworkInterfaces(nil); received != 0 || transmitted != 0 {
		t.Fatalf("空网卡列表应聚合为 0：(%d, %d)", received, transmitted)
	}
}

// TestSelectNetworkInterfacesSkipsLoopbackRows 验证 Windows/通用路径的回环过滤与透传，
// 使平台侧只剩无法跨平台验证的系统调用。
func TestSelectNetworkInterfacesSkipsLoopbackRows(t *testing.T) {
	interfaces := selectNetworkInterfaces([]netRawInterface{
		{Name: "Loopback", Loopback: true, ReceiveBytes: 999, TransmitBytes: 999},
		{Name: "以太网", ReceiveBytes: 10, TransmitBytes: 20},
	})
	if len(interfaces) != 1 || interfaces[0].Name != "以太网" || interfaces[0].ReceiveBytes != 10 || interfaces[0].TransmitBytes != 20 {
		t.Fatalf("应跳过回环网卡：%+v", interfaces)
	}
	if empty := selectNetworkInterfaces(nil); len(empty) != 0 {
		t.Fatalf("空输入应返回空列表：%+v", empty)
	}
}

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
