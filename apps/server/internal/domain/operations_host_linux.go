//go:build linux

package domain

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type linuxHostCollector struct {
	dataDir string
	ready   func() bool
}

func NewHostCollector(dataDir string, ready func() bool) HostCollector {
	return &linuxHostCollector{dataDir: dataDir, ready: ready}
}

func (c *linuxHostCollector) Collect(at time.Time) HostRawSample {
	item := HostRawSample{At: at, HostState: repository.MetricStateOK, NetworkState: repository.MetricStateOK,
		ProcessState: repository.MetricStateOK, ReadinessState: repository.MetricStateOK}
	if c.ready != nil && !c.ready() {
		item.ReadinessState, item.ReadinessErrorCode = repository.MetricStateError, "not_ready"
	}
	if idle, total, ok := readLinuxCPU(); ok {
		item.CPUIdleTicks, item.CPUTotalTicks = idle, total
	} else {
		item.HostState, item.HostErrorCode = repository.MetricStateUnavailable, "cpu_unavailable"
	}
	if total, available, ok := readLinuxMemory(); ok {
		item.MemoryTotalBytes, item.MemoryAvailableBytes = int64Ptr(total), int64Ptr(available)
	} else {
		item.HostState, item.HostErrorCode = repository.MetricStateUnavailable, "memory_unavailable"
	}
	if free, total, ok := readLinuxDisk(c.dataDir); ok {
		// 数据目录为磁盘采集根，总量与可用量取自同一次 statfs，与 diskAvailableBytes 口径一致。
		item.DiskAvailableBytes, item.DiskTotalBytes = int64Ptr(free), int64Ptr(total)
	} else {
		item.HostState, item.HostErrorCode = repository.MetricStateUnavailable, "disk_unavailable"
	}
	if in, out, ok := readLinuxNetwork(); ok {
		item.NetworkReceiveBytes, item.NetworkTransmitBytes = in, out
	} else {
		item.NetworkState, item.NetworkErrorCode = repository.MetricStateUnavailable, "network_unavailable"
	}
	if rss, ticks, ok := readLinuxProcess(); ok {
		item.ProcessRSSBytes, item.ProcessCPUTicks, item.GoroutineCount = int64Ptr(rss), ticks, int64Ptr(int64(runtime.NumGoroutine()))
	} else {
		item.ProcessState, item.ProcessErrorCode = repository.MetricStateUnavailable, "process_unavailable"
	}
	// 进程运行时长（秒）：仅当前进程（/proc/self starttime 起算），不做系统进程枚举；
	// 读取失败只置空该附加指标，不影响 process 组状态（RSS/CPU 仍可用）。
	if uptime, ok := readLinuxProcessUptime(at); ok {
		item.ProcessUptimeSeconds = int64Ptr(uptime)
	}
	if files, err := os.ReadDir("/proc/self/fd"); err == nil {
		item.OpenFileDescriptors = int64Ptr(int64(len(files)))
	} else {
		item.OpenFileDescriptors = nil
	}
	return item
}

func clockTicksPerSecond() uint64 { return 100 }

func readLinuxCPU() (uint64, uint64, bool) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0, 0, false
	}
	parts := strings.Fields(scanner.Text())
	if len(parts) < 5 || parts[0] != "cpu" {
		return 0, 0, false
	}
	var values []uint64
	for _, raw := range parts[1:] {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		values = append(values, value)
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	return values[3] + values[4], total, total > 0
}

// readLinuxMemory 读取 /proc/meminfo 并交由纯函数 parseMeminfo 解析
// （抽纯函数后非 Linux 平台也能对解析口径做单元测试）。
func readLinuxMemory() (int64, int64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	return parseMeminfo(string(data))
}

// readLinuxDisk 返回数据目录所在文件系统的可用量与总量（字节）。
// 总量 = Statfs.Blocks × Bsize；可用量为非特权用户可用的 Bavail × Bsize。
// 两者取自同一次 statfs，与 diskAvailableBytes 保持同一数据目录、同一次调用的口径。
func readLinuxDisk(dataDir string) (int64, int64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &stat); err != nil {
		return 0, 0, false
	}
	return int64(stat.Bavail) * int64(stat.Bsize), int64(stat.Blocks) * int64(stat.Bsize), true
}

func readLinuxNetwork() (uint64, uint64, bool) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()
	var received, transmitted uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		in, inErr := strconv.ParseUint(fields[0], 10, 64)
		out, outErr := strconv.ParseUint(fields[8], 10, 64)
		if inErr != nil || outErr != nil {
			continue
		}
		received += in
		transmitted += out
	}
	return received, transmitted, true
}

func readLinuxProcess() (int64, uint64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0, false
	}
	var rss int64
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[0] == "VmRSS:" {
			value, parseErr := strconv.ParseInt(parts[1], 10, 64)
			if parseErr == nil {
				rss = value * 1024
			}
		}
	}
	stat, err := os.ReadFile(filepath.Clean("/proc/self/stat"))
	if err != nil {
		return rss, 0, rss > 0
	}
	// utime/stime 与 starttime 由纯函数 parseProcSelfStat 统一解析（口径见该函数注释）。
	cpuTicks, _, cpuOK, _ := parseProcSelfStat(string(stat))
	return rss, cpuTicks, cpuOK
}

// readLinuxProcessUptime 计算当前进程运行时长（秒）：
// 启动绝对时刻 = /proc/stat 的 btime（系统启动 Unix 秒）+ /proc/self/stat 的 starttime
// （距系统启动的时钟滴答数）换算，详见 processUptimeSeconds 的口径与精度说明。
// 任一步读取或解析失败返回 false，由上层给出空值而不是伪造零。
func readLinuxProcessUptime(at time.Time) (int64, bool) {
	statData, err := os.ReadFile(filepath.Clean("/proc/self/stat"))
	if err != nil {
		return 0, false
	}
	_, startTicks, _, startOK := parseProcSelfStat(string(statData))
	if !startOK {
		return 0, false
	}
	bootData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	bootUnix, ok := parseProcStatBtime(string(bootData))
	if !ok {
		return 0, false
	}
	return processUptimeSeconds(at.Unix(), bootUnix, startTicks, clockTicksPerSecond()), true
}

func int64Ptr(value int64) *int64 { return &value }
