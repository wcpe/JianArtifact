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
	if free, ok := readLinuxDisk(c.dataDir); ok {
		item.DiskAvailableBytes = int64Ptr(free)
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

func readLinuxMemory() (int64, int64, bool) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()
	values := map[string]int64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
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

func readLinuxDisk(dataDir string) (int64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &stat); err != nil {
		return 0, false
	}
	return int64(stat.Bavail) * int64(stat.Bsize), true
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
	parts := strings.Fields(string(stat))
	if len(parts) < 15 {
		return rss, 0, rss > 0
	}
	user, userErr := strconv.ParseUint(parts[13], 10, 64)
	kernel, kernelErr := strconv.ParseUint(parts[14], 10, 64)
	return rss, user + kernel, userErr == nil && kernelErr == nil
}

func int64Ptr(value int64) *int64 { return &value }
