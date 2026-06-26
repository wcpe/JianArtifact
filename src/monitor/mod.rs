//! 主机 / 系统监控采集（FR-98，ADR-0023）。
//!
//! 经 `sysinfo` 跨平台采集运行制品库的**这台主机**的基础资源画像：CPU 使用率 + 核数、
//! 内存 / 交换的已用 / 总量、磁盘挂载点 / 总量 / 可用及汇总、系统 uptime。
//!
//! 设计要点：
//! - **按请求采样**：单进程共享一份 `sysinfo::System`（refresh 需 `&mut`），由调用方经
//!   `Mutex` 串行化；本模块只负责「刷新 + 读数 → DTO」的组装。不做后台轮询、不落库、不留历史。
//! - **纯映射可测**：把「sysinfo 读数 → DTO」的纯计算（磁盘列表映射与汇总等）抽成无副作用纯
//!   函数（`map_disks`），便于穷举单测；带 IO 副作用的刷新与读数集中在 `collect`。
//! - **本机内部、不外发**：纯本地采样，绝不向外部上报 / 导出 / phone-home（守 ADR-0009 / 0015 基调）。
//! - **CPU 首样取舍**：`sysinfo` 的 CPU 使用率需两次采样间隔才有非零值；本期单次 refresh，
//!   首次 / 间隔过近的采样 CPU 使用率可能为 `0`（合法值），不为它引后台轮询（见 ADR-0023）。

use serde::Serialize;
use sysinfo::{Disks, MemoryRefreshKind, RefreshKind, System};

/// 主机指标快照（对外 DTO）。
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct HostMetrics {
    /// CPU 指标。
    pub cpu: CpuMetrics,
    /// 内存与交换分区指标。
    pub memory: MemoryMetrics,
    /// 磁盘指标（逐盘 + 汇总）。
    pub disk: DiskMetrics,
    /// 系统运行时长（秒）。
    pub uptime_secs: u64,
}

/// CPU 指标。
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct CpuMetrics {
    /// 全局 CPU 使用率百分比（0~100）。首次 / 间隔过近的采样可能为 0（见模块说明）。
    pub usage_percent: f32,
    /// 逻辑核数。
    pub logical_cores: usize,
}

/// 内存与交换分区指标（单位：字节）。
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct MemoryMetrics {
    /// 物理内存总量。
    pub total_bytes: u64,
    /// 物理内存已用量。
    pub used_bytes: u64,
    /// 交换分区总量。
    pub swap_total_bytes: u64,
    /// 交换分区已用量。
    pub swap_used_bytes: u64,
}

/// 磁盘指标：逐盘明细 + 总量 / 可用汇总（单位：字节）。
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct DiskMetrics {
    /// 全部磁盘总容量汇总。
    pub total_bytes: u64,
    /// 全部磁盘可用容量汇总。
    pub available_bytes: u64,
    /// 逐盘明细。
    pub disks: Vec<DiskEntry>,
}

/// 单块磁盘明细（单位：字节）。
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct DiskEntry {
    /// 挂载点（如 `/` 或 `C:\`）。
    pub mount_point: String,
    /// 该盘总容量。
    pub total_bytes: u64,
    /// 该盘可用容量。
    pub available_bytes: u64,
}

/// 采集一份主机指标快照：刷新给定 `System` 的 CPU 使用率与内存，刷新磁盘列表，组装 DTO。
///
/// `system` 由调用方持有并串行化（refresh 需 `&mut`）；`disks` 按请求刷新当前挂载。
/// 纯映射部分委托 `map_disks`；本函数承载刷新与读数的 IO 副作用。
pub fn collect(system: &mut System, disks: &mut Disks) -> HostMetrics {
    // 仅刷新本期需要的维度（CPU 使用率 + 内存 / 交换），不触发进程 / 网络等无关采集
    system.refresh_specifics(
        RefreshKind::nothing()
            .with_cpu(sysinfo::CpuRefreshKind::nothing().with_cpu_usage())
            .with_memory(MemoryRefreshKind::nothing().with_ram().with_swap()),
    );
    // 刷新磁盘列表（移除已不在列表中的盘），取当前挂载快照
    disks.refresh(true);

    HostMetrics {
        cpu: CpuMetrics {
            usage_percent: system.global_cpu_usage(),
            logical_cores: system.cpus().len(),
        },
        memory: MemoryMetrics {
            total_bytes: system.total_memory(),
            used_bytes: system.used_memory(),
            swap_total_bytes: system.total_swap(),
            swap_used_bytes: system.used_swap(),
        },
        disk: map_disks(disks),
        // uptime 为关联函数（与具体 System 实例无关），直接取系统运行时长
        uptime_secs: System::uptime(),
    }
}

/// 纯映射：把磁盘列表映射为逐盘明细并汇总总量 / 可用（无副作用，便于穷举单测）。
pub fn map_disks(disks: &Disks) -> DiskMetrics {
    let mut total_bytes = 0u64;
    let mut available_bytes = 0u64;
    let entries: Vec<DiskEntry> = disks
        .list()
        .iter()
        .map(|d| {
            let total = d.total_space();
            let available = d.available_space();
            // 用 saturating 累加，避免极端情况下的溢出
            total_bytes = total_bytes.saturating_add(total);
            available_bytes = available_bytes.saturating_add(available);
            DiskEntry {
                mount_point: d.mount_point().to_string_lossy().into_owned(),
                total_bytes: total,
                available_bytes: available,
            }
        })
        .collect();
    DiskMetrics {
        total_bytes,
        available_bytes,
        disks: entries,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 纯映射：空磁盘列表汇总为 0、明细为空。
    #[test]
    fn map_disks_空列表汇总为零() {
        let disks = Disks::new();
        let m = map_disks(&disks);
        assert_eq!(m.total_bytes, 0);
        assert_eq!(m.available_bytes, 0);
        assert!(m.disks.is_empty());
    }

    /// 采集真机一次：内存总量应 > 0、逻辑核数 ≥ 1、磁盘汇总不小于任一单盘（合理范围断言）。
    #[test]
    fn collect_读数在合理范围() {
        let mut system = System::new();
        let mut disks = Disks::new();
        let m = collect(&mut system, &mut disks);

        // 内存总量必然为正
        assert!(m.memory.total_bytes > 0, "内存总量应大于 0");
        // 已用不超过总量
        assert!(
            m.memory.used_bytes <= m.memory.total_bytes,
            "已用内存不应超过总量"
        );
        // 至少一个逻辑核
        assert!(m.cpu.logical_cores >= 1, "逻辑核数应至少为 1");
        // CPU 使用率为合法百分比（首样可能为 0，属已知取舍）
        assert!(
            (0.0..=100.0).contains(&m.cpu.usage_percent),
            "CPU 使用率应在 0~100：{}",
            m.cpu.usage_percent
        );
        // 磁盘汇总等于逐盘累加（自洽性）
        let sum_total: u64 = m.disk.disks.iter().map(|d| d.total_bytes).sum();
        let sum_avail: u64 = m.disk.disks.iter().map(|d| d.available_bytes).sum();
        assert_eq!(m.disk.total_bytes, sum_total, "磁盘总量汇总应等于逐盘累加");
        assert_eq!(
            m.disk.available_bytes, sum_avail,
            "磁盘可用汇总应等于逐盘累加"
        );
    }
}
