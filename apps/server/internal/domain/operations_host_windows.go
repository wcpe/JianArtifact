//go:build windows

package domain

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

type windowsHostCollector struct {
	dataDir string
	ready   func() bool
}

func NewHostCollector(dataDir string, ready func() bool) HostCollector {
	return &windowsHostCollector{dataDir: dataDir, ready: ready}
}

func (c *windowsHostCollector) Collect(at time.Time) HostRawSample {
	item := HostRawSample{At: at, HostState: repository.MetricStateOK, NetworkState: repository.MetricStateOK, ProcessState: repository.MetricStateOK, ReadinessState: repository.MetricStateOK}
	if c.ready != nil && !c.ready() {
		item.ReadinessState, item.ReadinessErrorCode = repository.MetricStateError, "not_ready"
	}
	if idle, total, ok := windowsCPU(); ok {
		item.CPUIdleTicks, item.CPUTotalTicks = idle, total
	} else {
		item.HostState, item.HostErrorCode = repository.MetricStateUnavailable, "cpu_unavailable"
	}
	if total, available, ok := windowsMemory(); ok {
		item.MemoryTotalBytes, item.MemoryAvailableBytes = int64Ptr(total), int64Ptr(available)
	} else {
		item.HostState, item.HostErrorCode = repository.MetricStateUnavailable, "memory_unavailable"
	}
	if free, ok := windowsDisk(c.dataDir); ok {
		item.DiskAvailableBytes = int64Ptr(free)
	} else {
		item.HostState, item.HostErrorCode = repository.MetricStateUnavailable, "disk_unavailable"
	}
	if in, out, ok := windowsNetwork(); ok {
		item.NetworkReceiveBytes, item.NetworkTransmitBytes = in, out
	} else {
		item.NetworkState, item.NetworkErrorCode = repository.MetricStateUnavailable, "network_unavailable"
	}
	if rss, ticks, ok := windowsProcess(); ok {
		item.ProcessRSSBytes, item.ProcessCPUTicks, item.GoroutineCount = int64Ptr(rss), ticks, int64Ptr(int64(runtime.NumGoroutine()))
	} else {
		item.ProcessState, item.ProcessErrorCode = repository.MetricStateUnavailable, "process_unavailable"
	}
	item.OpenFileDescriptors = nil
	return item
}

func clockTicksPerSecond() uint64 { return 10000000 }

func windowsCPU() (uint64, uint64, bool) {
	var idle, kernel, user windows.Filetime
	if err := getSystemTimes(&idle, &kernel, &user); err != nil {
		return 0, 0, false
	}
	idleTicks, kernelTicks, userTicks := filetimeTicks(idle), filetimeTicks(kernel), filetimeTicks(user)
	return idleTicks, kernelTicks + userTicks, kernelTicks+userTicks > 0
}

func windowsMemory() (int64, int64, bool) {
	status := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if err := globalMemoryStatusEx(&status); err != nil {
		return 0, 0, false
	}
	return int64(status.TotalPhys), int64(status.AvailPhys), true
}

func windowsDisk(dir string) (int64, bool) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(path, &available, nil, nil); err != nil {
		return 0, false
	}
	return int64(available), true
}

func windowsNetwork() (uint64, uint64, bool) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil || table == nil {
		return 0, 0, false
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	rows := unsafe.Slice(&table.Table[0], int(table.NumEntries))
	var received, transmitted uint64
	for _, row := range rows {
		if row.Type != windows.IF_TYPE_SOFTWARE_LOOPBACK {
			received += row.InOctets
			transmitted += row.OutOctets
		}
	}
	return received, transmitted, true
}

func windowsProcess() (int64, uint64, bool) {
	process := windows.CurrentProcess()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return 0, 0, false
	}
	info := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	if err := getProcessMemoryInfo(process, &info); err != nil {
		return 0, 0, false
	}
	return int64(info.WorkingSetSize), filetimeTicks(kernel) + filetimeTicks(user), true
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

func getSystemTimes(idle, kernel, user *windows.Filetime) error {
	result, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(idle)), uintptr(unsafe.Pointer(kernel)), uintptr(unsafe.Pointer(user)))
	if result == 0 {
		return err
	}
	return nil
}
func globalMemoryStatusEx(status *memoryStatusEx) error {
	result, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(status)))
	if result == 0 {
		return err
	}
	return nil
}
func getProcessMemoryInfo(process windows.Handle, info *processMemoryCounters) error {
	result, _, err := procGetProcessMemoryInfo.Call(uintptr(process), uintptr(unsafe.Pointer(info)), uintptr(info.CB))
	if result == 0 {
		return err
	}
	return nil
}
func filetimeTicks(value windows.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}
func int64Ptr(value int64) *int64 { return &value }
