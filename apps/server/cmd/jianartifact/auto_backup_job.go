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
// 保留策略的阈值取自本轮的 settings 快照，而非注册时固定的值：这样界面上改完
// 份数/天数，下一轮就按新阈值淘汰，无需重启。
//
// 生成中已有其他备份任务时（ErrBackupInProgress）**跳过本轮且不算失败**：
// 手动备份与自动备份共用同一个生成槽位，撞车是正常竞争而不是故障，
// 把它记成失败会让 /maintenance/jobs 长期显示失败计数，掩盖真正的问题。
func runAutoBackup(ctx context.Context, backupSvc *domain.BackupService, retentionSvc *domain.BackupRetentionService, settings autoBackupSettings) (autoBackupRun, error) {
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
	// 就会立即应用一次，不需要等下一轮。阈值用本轮快照现算。
	retentionSvc.SetOptions(domain.BackupRetentionOptions{
		KeepLast:      settings.KeepLast,
		RetentionDays: settings.RetentionDays,
	})
	result, err := retentionSvc.Run()
	run.Retention = result
	if err != nil {
		return run, err
	}
	return run, nil
}

// autoBackupSettings 是作业每轮重读的一份自动备份配置快照。
type autoBackupSettings struct {
	// IntervalSecs 是自动备份间隔（秒）；<= 0 表示关闭。
	IntervalSecs int
	// KeepLast / RetentionDays 是保留策略的两个上限。
	KeepLast      int
	RetentionDays int
}

// autoBackupSettingsSource 读一份当前生效的配置。
//
// 抽成函数类型而不是直接传 SettingService：作业不关心配置存哪（setting 表、
// 环境变量、还是测试里的固定值），只要求"每次调用都能拿到当下最新的值"。
// 这正是热生效的全部依赖——作业每轮重新调用它，而不是在注册时读一次。
type autoBackupSettingsSource func() autoBackupSettings

// registerAutoBackupJob 把周期化自动备份注册为调度器作业。
//
// # 为什么用"心跳 + 每轮判到点"而不是把间隔直接交给调度器
//
// scheduler.Register 的间隔在注册时固定、运行中改不了。若把配置间隔直接
// 交给它，界面上改完必须重启进程才生效——那正是本次要修掉的问题。
// 因此这里注册一个**固定心跳**（心跳粒度即最小可配粒度），每轮唤醒后重读
// 配置、自行判断是否到点：配置改了下一轮就生效，无需重启。
//
// # 与既有作业的语义差异（必须知道）
//
//   - 既有作业（storage-cleanup / blob-gc）间隔 <= 0 时**不注册**；
//     本作业**始终注册**，间隔为 0 时每轮空转跳过。原因是间隔现在可以在界面上
//     从 0 改为非 0，作业必须先存在才能被唤醒；代价是「维护作业」页始终能看到
//     backup-auto，界面与文档必须如实说明"看到它不代表已启用，0 表示不执行"。
//   - 失败隔离仍由调度器承载（单轮失败只记计数、不影响后续周期），所以生成与
//     保留策略的错误原样上报，不吞掉——吞掉会让 /maintenance/jobs 显示一切正常。
func registerAutoBackupJob(
	sched *scheduler.Scheduler,
	heartbeat time.Duration,
	source autoBackupSettingsSource,
	fn func(ctx context.Context, settings autoBackupSettings) (autoBackupRun, error),
) {
	if heartbeat <= 0 || source == nil || fn == nil {
		return
	}
	// lastRun 初始为"现在"而不是零值：这样首次执行也要等满一个配置间隔，
	// 保持既有语义「启动不立即备份」（调度器自己的注释也写明"启动不立即执行"）。
	// 若留零值，第一轮心跳就会立刻生成备份——升级重启即产生一个包，
	// 与"不在无人确认时突然占磁盘"的设计意图相悖。
	//
	// 同时记录配置间隔：间隔被改小时（如 3600 → 30），必须立刻按新间隔判到点，
	// 否则要等满旧间隔（最长一小时）才生效，"热生效"就成了空话。
	lastRun := time.Now()
	var lastInterval time.Duration
	sched.Register("backup-auto", heartbeat, func(ctx context.Context) error {
		settings := source()
		if settings.IntervalSecs <= 0 {
			// 关闭态：不生成、不淘汰，也不记失败（"关着"不是故障）。
			return nil
		}
		interval := time.Duration(settings.IntervalSecs) * time.Second
		// 间隔被调小时，把基准时间前移，使新间隔立即生效：
		// 例如 3600s 改成 30s，已经等过的时长按新间隔重新计（不必再等一小时）。
		if lastInterval > 0 && interval < lastInterval {
			elapsed := time.Since(lastRun)
			if scaled := elapsed * interval / lastInterval; scaled < elapsed {
				lastRun = time.Now().Add(-scaled)
			}
		}
		lastInterval = interval
		// 到点判断按"距上次跑完的间隔"算，而不是按固定时钟对齐：
		// 后者要么引入时间漂移，要么在心跳与配置间隔不成整数比时漏跑。
		if time.Since(lastRun) < interval {
			return nil
		}
		run, err := fn(ctx, settings)
		if err != nil {
			// 保留策略可能已部分完成：把它写进日志再上报失败，便于定位卡在哪一步
			// （与 storage-cleanup 的部分完成日志同一口径）。
			if !run.Retention.Empty() {
				log.Printf("自动备份保留策略部分完成：%s", run.Retention.Summary())
			}
			// 失败也更新时间戳：否则下一轮心跳会立刻重试，一个持续失败（如磁盘满）
			// 会变成每个心跳都试一次的高频重试，把日志刷爆。
			lastRun = time.Now()
			return err
		}
		lastRun = time.Now()
		if run.PackageID != "" {
			log.Printf("自动备份完成：包 %s；保留策略：%s", run.PackageID, run.Retention.Summary())
		}
		return nil
	})
}

// autoBackupHeartbeat 是作业的心跳粒度，也是最小编程可配的自动备份间隔。
//
// 30 秒是"改配置后最多等这么久生效"与"空转开销"之间的折中：空转只读一次
// setting 表（SQLite 点查），成本可忽略；而更细的心跳对备份这种分钟级任务没有意义。
const autoBackupHeartbeat = 30 * time.Second
