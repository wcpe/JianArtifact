package main

import (
	"context"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// registerStorageCleanupJob 把 FR-41 存储治理清理注册为调度器作业：
// 按固定间隔执行、启动不立即清理；间隔 <= 0 或清理函数为空时不注册
// （禁用语义，对齐 JIAN_STORAGE_CLEANUP_INTERVAL=0）。
//
// 与 registerBlobGCJob 的分工：blob-gc 只处理活动目录中的无引用 blob，
// storage-cleanup 只处理隔离区、终态元数据、过期上传临时文件与代理缓存保留，
// 两者按目录命名空间与元数据状态天然互斥。
func registerStorageCleanupJob(sched *scheduler.Scheduler, interval time.Duration, cleanup func() (domain.StorageCleanupResult, error)) {
	if interval <= 0 || cleanup == nil {
		return
	}
	sched.Register("storage-cleanup", interval, func(context.Context) error {
		result, err := cleanup()
		if err != nil {
			// 子步骤可能部分成功：把已完成的部分写进日志再上报失败，便于定位卡在哪一步。
			if !result.Empty() {
				log.Printf("存储治理清理部分完成：%s", result.Summary())
			}
			return err
		}
		if result.Empty() {
			return nil
		}
		log.Printf("存储治理清理完成：%s", result.Summary())
		return nil
	})
}
