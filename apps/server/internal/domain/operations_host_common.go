package domain

import (
	"strconv"
	"strings"
	"time"
)

// parseMeminfo 解析 /proc/meminfo 文本，返回 MemTotal 与 MemAvailable（单位字节）。
// meminfo 以 kB 记录，统一乘 1024 换算；抽为无平台标签的纯函数，
// 便于在非 Linux 平台对解析口径做单元测试。
func parseMeminfo(content string) (total int64, available int64, ok bool) {
	values := map[string]int64{}
	for _, line := range strings.Split(content, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		value, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		values[strings.TrimSuffix(parts[0], ":")] = value * 1024
	}
	total, totalOK := values["MemTotal"]
	available, availableOK := values["MemAvailable"]
	return total, available, totalOK && availableOK
}

// parseProcStatBtime 解析 /proc/stat 文本中的 btime 行，返回系统启动的 Unix 秒。
func parseProcStatBtime(content string) (int64, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

// parseProcSelfStat 解析 /proc/self/stat 文本：
// cpuTicks 为 utime+stime（字段 14/15，索引 13/14），单位为时钟滴答；
// startTicks 为 starttime（字段 22，索引 21），即进程启动时刻距系统启动的时钟滴答数。
// cpuOK 表示 CPU 滴答解析成功；startOK 表示 starttime 解析成功——
// 运行时长计算依赖 startOK，失败时调用方必须给出空值而不是伪造零。
// 已知假设：comm 字段（第 2 列）不含空格，与既有采集实现一致。
func parseProcSelfStat(content string) (cpuTicks uint64, startTicks uint64, cpuOK bool, startOK bool) {
	parts := strings.Fields(content)
	if len(parts) < 15 {
		return 0, 0, false, false
	}
	user, userErr := strconv.ParseUint(parts[13], 10, 64)
	kernel, kernelErr := strconv.ParseUint(parts[14], 10, 64)
	cpuTicks, cpuOK = user+kernel, userErr == nil && kernelErr == nil
	if len(parts) < 22 {
		return cpuTicks, 0, cpuOK, false
	}
	start, startErr := strconv.ParseUint(parts[21], 10, 64)
	return cpuTicks, start, cpuOK, startErr == nil
}

// processUptimeSeconds 计算进程运行时长（秒）：当前 Unix 秒 − 进程启动绝对时刻。
// 启动时刻 = 系统启动 Unix 秒（Linux 取 /proc/stat 的 btime）+ starttime/每秒滴答数。
// 口径：挂钟时间差，包含系统休眠时段；精度受 btime 秒级取整与滴答粒度
// （Linux USER_HZ=100 即 10ms）限制；btime 粗糙估算导致的负值钳制为 0。
func processUptimeSeconds(nowUnix, bootUnix int64, startTicks, ticksPerSecond uint64) int64 {
	if ticksPerSecond == 0 {
		return 0
	}
	uptime := nowUnix - (bootUnix + int64(startTicks/ticksPerSecond))
	if uptime < 0 {
		return 0
	}
	return uptime
}

// windowsEpochToUnix100ns 是 1601-01-01 到 1970-01-01 的 100ns 计数（FILETIME 纪元差）。
const windowsEpochToUnix100ns = 116444736000000000

// filetimeUnixSeconds 把 Windows FILETIME（自 1601-01-01 UTC 起的 100ns 计数）换算为 Unix 秒；
// 小于纪元差的非法值按 0 处理（当前进程创建时间不可能早于 1970 年）。
func filetimeUnixSeconds(ticks uint64) int64 {
	if ticks < windowsEpochToUnix100ns {
		return 0
	}
	return int64((ticks - windowsEpochToUnix100ns) / 10000000)
}

// processUptimeFromCreated 按进程创建时间（FILETIME 计数，取自 GetProcessTimes）计算运行时长（秒）。
// 口径：挂钟时间差，包含系统休眠时段；标称精度 100ns，实际受系统时钟分辨率与
// NTP 调整影响；时钟回拨导致的负值钳制为 0。
func processUptimeFromCreated(now time.Time, createdTicks uint64) int64 {
	uptime := now.Unix() - filetimeUnixSeconds(createdTicks)
	if uptime < 0 {
		return 0
	}
	return uptime
}
