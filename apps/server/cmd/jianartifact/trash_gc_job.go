package main

import (
	"context"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// registerTrashGCJob 把回收站保留期回收注册为调度器作业。
//
// 与另外三个清理作业的命名空间分工：
//   - blob-gc 处理活动 blob 目录中的无引用 blob；
//   - storage-cleanup 处理隔离区、终态元数据、过期上传临时文件与代理缓存保留；
//   - backup-orphan-cleanup 处理 backups/ 下未登记的包体；
//   - **本作业只处理回收站（data/trash/）里超过保留期的条目**。
//
// 四者目录互不重叠。本作业只删回收站副本，绝不触碰 original_path——那是 restore 的职责，
// 与"到期清理"是两件事。
//
// 间隔 <= 0 不注册（禁用语义，对齐其余清理作业）；保留期 <= 0 时本作业即使注册也每轮空转
// （不自动清理），因此不需要两处都判。
func registerTrashGCJob(sched *scheduler.Scheduler, interval, retention time.Duration, gc func(time.Duration) (domain.TrashPurgeResult, error)) {
	if interval <= 0 || gc == nil {
		return
	}
	sched.Register("trash-gc", interval, func(context.Context) error {
		result, err := gc(retention)
		if err != nil {
			// 单条失败不中断整轮：把已完成的部分写进日志再上报失败，便于定位。
			if result.Purged > 0 {
				log.Printf("回收站保留期回收部分完成：%d 条 / %d 字节", result.Purged, result.FreedBytes)
			}
			return err
		}
		if result.Purged == 0 {
			return nil
		}
		log.Printf("回收站保留期回收完成：%d 条 / %d 字节", result.Purged, result.FreedBytes)
		return nil
	})
}
