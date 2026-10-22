package main

import (
	"context"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// registerBackupOrphanCleanupJob 把 的备份孤儿回收注册为调度器作业：
// 按固定间隔执行、启动不立即回收；间隔 <= 0 或回收函数为空时不注册
// （禁用语义，对齐 JIAN_BACKUP_ORPHAN_CLEANUP_INTERVAL=0）。
//
// 与另外两个清理作业的命名空间分工：
//   - blob-gc 只处理活动 blob 目录中的无引用 blob；
//   - storage-cleanup 只处理隔离区、终态元数据、过期上传临时文件与代理缓存保留；
//   - 本作业只处理 `${JIAN_DATA_DIR}/backups/` 下"登记表里没有"的包体与侧车索引。
//
// 三者目录互不重叠。本作业不解包、不改数据库，只做文件删除，因此不引入任何
// 跨命名空间的副作用。
func registerBackupOrphanCleanupJob(sched *scheduler.Scheduler, interval time.Duration, cleanup func() (domain.BackupOrphanCleanupResult, error)) {
	if interval <= 0 || cleanup == nil {
		return
	}
	sched.Register("backup-orphan-cleanup", interval, func(context.Context) error {
		result, err := cleanup()
		if err != nil {
			// 单包删除失败不中断整轮：把已完成的部分写进日志再上报失败，便于定位。
			if !result.Empty() {
				log.Printf("备份孤儿回收部分完成：%s", result.Summary())
			}
			return err
		}
		if result.Empty() {
			return nil
		}
		log.Printf("备份孤儿回收完成：%s", result.Summary())
		return nil
	})
}
