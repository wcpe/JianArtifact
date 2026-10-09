package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// autoBackupLabel 是自动备份包的固定备注：列表里靠 origin 列分辨来源，
// 备注再补一句人读上下文，便于与手动包在日志中对照。
const autoBackupLabel = "周期自动备份"

// autoBackupRun 是一轮自动备份作业的执行结果，供日志与测试断言。
type autoBackupRun struct {
	// PackageID 是本轮生成的备份包标识。
	PackageID string
	// Retention 是本轮收尾时应用的保留策略计数。
	Retention domain.BackupRetentionResult
}

// runAutoBackup 执行一轮自动备份：生成一份**完整热备份包**，成功后应用保留策略。
//
// 两处刻意的固定选择（规格 §2.1）：
//   - **hot 模式**：frozen 需要人工控制停机窗口，不适合无人值守；
//   - **不传 BasePackageID**：差包依赖指定基线，无人值守下基线选择不可控且易形成长链，
//     因此自动备份每次都生成完整包（磁盘代价由随批交付的保留策略兜住）。
//
// 生成中已有其他备份任务时（ErrBackupInProgress）**跳过本轮且不算失败**：
// 手动备份与自动备份共用同一个生成槽位，撞车是正常竞争而不是故障，
// 把它记成失败会让 /maintenance/jobs 长期显示失败计数，掩盖真正的问题。
func runAutoBackup(ctx context.Context, backupSvc *domain.BackupService, retentionSvc *domain.BackupRetentionService) (autoBackupRun, error) {
	var run autoBackupRun
	rec, err := backupSvc.Generate(ctx, domain.CreateBackupOptions{
		Mode:   archive.ModeHot,
		Label:  autoBackupLabel,
		Origin: repository.BackupOriginAuto,
	})
	if err != nil {
		if errors.Is(err, domain.ErrBackupInProgress) {
			log.Printf("自动备份跳过本轮：已有备份生成任务在进行中")
			return run, nil
		}
		return run, err
	}
	run.PackageID = rec.PackageID

	// 保留策略是作业的收尾步骤，随本作业一起跑（规格 §2.2）：首个自动备份生成后
	// 就会立即应用一次，不需要等下一轮。
	result, err := retentionSvc.Run()
	run.Retention = result
	if err != nil {
		return run, err
	}
	return run, nil
}

// registerAutoBackupJob 把 FR-40 周期化自动备份注册为调度器作业：
// 按固定间隔执行、启动不立即备份；间隔 <= 0 或执行函数为空时不注册
// （禁用语义，对齐 JIAN_BACKUP_AUTO_INTERVAL=0）。
//
// 与 registerStorageCleanupJob / registerBlobGCJob 的差别只有一点：
// 本作业**默认不注册**（默认间隔 0）。自动备份会在无人确认的情况下持续占用磁盘，
// 必须是运维显式开启的能力，不在升级后悄悄开始。
//
// 失败隔离由调度器承载（单轮失败只记计数、不影响后续周期），因此这里把
// 生成与保留策略的错误原样上报，不吞掉——吞掉会让 /maintenance/jobs 显示一切正常。
func registerAutoBackupJob(sched *scheduler.Scheduler, interval time.Duration, fn func(ctx context.Context) (autoBackupRun, error)) {
	if interval <= 0 || fn == nil {
		return
	}
	sched.Register("backup-auto", interval, func(ctx context.Context) error {
		run, err := fn(ctx)
		if err != nil {
			// 保留策略可能已部分完成：把它写进日志再上报失败，便于定位卡在哪一步
			// （与 storage-cleanup 的部分完成日志同一口径）。
			if !run.Retention.Empty() {
				log.Printf("自动备份保留策略部分完成：%s", run.Retention.Summary())
			}
			return err
		}
		if run.PackageID != "" {
			log.Printf("自动备份完成：包 %s；保留策略：%s", run.PackageID, run.Retention.Summary())
		}
		return nil
	})
}
