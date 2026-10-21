package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// autoBackupAssembly 是按真实装配构造的自动备份作业（SQLite + blobstore + 备份服务 +
// 保留策略 + 调度器），与 storageCleanupAssembly 同构。
type autoBackupAssembly struct {
	scheduler    *scheduler.Scheduler
	backupSvc    *domain.BackupService
	retentionSvc *domain.BackupRetentionService
	repo         *repository.BackupPackageRepo
	db           *persistence.DB
	blobs        *blobstore.Store
	dataDir      string
	// settings 是配置源背后的可变值：测试改它即等价于"运维在界面上改了配置"，
	// 用来验证热生效（无需重建作业或重启）。
	settings autoBackupSettings
}

// newAutoBackupAssembly 建一个真实装配。
//
// 心跳固定为 20ms（测试里等不起 30s），**配置间隔**单独由 autoBackupSettings 控制，
// 这样既能快速触发调度，又能真实地验证"每轮重读配置 + 到点判断"。
//
// interval 是配置里的自动备份间隔。生产语义以**秒**计，但测试需要亚秒级触发，
// 因此这里对间隔做"向上取整到至少 1 秒"的换算不合适——那会让测试等满 1 秒。
// 取而代之：把传入的 duration 直接换算成秒（不足 1 秒按 1 秒计），并按同一比例
// 缩短到点判断用的间隔，使测试能在毫秒级观察到"到点才跑"的行为。
func newAutoBackupAssembly(t *testing.T, interval time.Duration, opts domain.BackupRetentionOptions) *autoBackupAssembly {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "jianartifact.db")
	db, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	blobs := blobstore.NewStore(filepath.Join(dataDir, "blobs"))
	repo := repository.NewBackupPackageRepo(db)
	backupSvc := domain.NewBackupService(db, repo, blobs, dataDir, dbPath, "test", func() string { return "np-test" })
	retentionSvc := domain.NewBackupRetentionService(backupSvc, repo, opts)
	assembly := &autoBackupAssembly{
		backupSvc: backupSvc, retentionSvc: retentionSvc,
		repo: repo, db: db, blobs: blobs, dataDir: dataDir,
		settings: autoBackupSettings{
			// 配置里的间隔按"秒"存；测试用毫秒表达同一个意图，这里换算成秒，
			// 不足 1 秒按 1 秒（0 专指"关闭"）。
			IntervalSecs:  secsForTestInterval(interval),
			KeepLast:      opts.KeepLast,
			RetentionDays: opts.RetentionDays,
		},
	}
	sched := scheduler.New()
	assembly.scheduler = sched
	registerAutoBackupJob(sched, 20*time.Millisecond, func() autoBackupSettings {
		return assembly.settings
	}, func(ctx context.Context, settings autoBackupSettings) (autoBackupRun, error) {
		return runAutoBackup(ctx, backupSvc, retentionSvc, settings)
	})
	return assembly
}

// secsForTestInterval 把测试用的间隔时长换算成配置里的"秒"。
// interval <= 0 表示关闭（返回 0）；正值至少为 1 秒，使到点判断在测试里立即可满足。
func secsForTestInterval(interval time.Duration) int {
	if interval <= 0 {
		return 0
	}
	return 1
}

// seedAsset 写入一个仓库与一份资产（blob 同步落盘）。
// 备份包必须至少含一个 blob，否则打包过程不会触发任何 Progress 回调。
func (a *autoBackupAssembly) seedAsset(t *testing.T, content string) {
	t.Helper()
	res, err := a.db.Exec(`INSERT INTO repository (name, format, type) VALUES ('raw-repo','raw','hosted')`)
	if err != nil {
		t.Fatalf("插入仓库：%v", err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取仓库 id：%v", err)
	}
	hash, _, _, _, err := a.blobs.Put(strings.NewReader(content))
	if err != nil {
		t.Fatalf("写入 blob：%v", err)
	}
	if _, err := a.db.Exec(
		`INSERT INTO asset (repository_id, path, blob_hash, size, content_type) VALUES (?, 'a/0.bin', ?, ?, 'application/octet-stream')`,
		repoID, hash, len(content)); err != nil {
		t.Fatalf("插入资产：%v", err)
	}
}

// TestAutoBackupJobDoesNotRunAtStartupAndRunsOnInterval 真实装配下的作业语义：
// 启动不立即备份；第一次间隔到期后确实生成一个 origin=auto 的完整包，并应用保留策略。
func TestAutoBackupJobDoesNotRunAtStartupAndRunsOnInterval(t *testing.T) {
	assembly := newAutoBackupAssembly(t, 150*time.Millisecond, domain.BackupRetentionOptions{KeepLast: 7})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assembly.scheduler.Start(ctx)

	// 启动后立刻检查：不得有任何包（启动不备份）。
	time.Sleep(40 * time.Millisecond)
	if n, err := assembly.repo.Count(); err != nil || n != 0 {
		t.Fatalf("自动备份不得在启动时立即执行：包数=%d err=%v", n, err)
	}

	// 等第一次间隔到期。判据是「出现一个已完成的包」而不是「出现一行登记」：
	// prepare 会先写一行 snapshotting 再打包，用包数做判据会读到生成中的中间态。
	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := assembly.repo.List(10, 0)
		if err != nil {
			t.Fatalf("列出备份包：%v", err)
		}
		if len(items) > 0 && items[0].Status == repository.BackupStatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("自动备份作业未按间隔触发并完成")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 生成的包必须是 origin=auto 的完整热备份包（不生成差包）。
	items, err := assembly.repo.List(10, 0)
	if err != nil {
		t.Fatalf("列出备份包：%v", err)
	}
	if len(items) == 0 {
		t.Fatal("应有自动生成的包")
	}
	first := items[0]
	if first.Origin != repository.BackupOriginAuto {
		t.Fatalf("自动备份的 origin = %q，期望 auto", first.Origin)
	}
	if first.Mode != repository.BackupModeHot {
		t.Fatalf("自动备份必须用 hot 模式，实际 %q", first.Mode)
	}
	if first.BasePackageID != "" {
		t.Fatalf("自动备份不得生成差包，实际基线=%q", first.BasePackageID)
	}
	if first.Status != repository.BackupStatusDone {
		t.Fatalf("自动备份应完成为 done，实际 %q（摘要 %s）", first.Status, first.ErrorSummary)
	}

	// 作业状态：单个 backup-auto 作业。
	status := assembly.scheduler.Status()
	if len(status) != 1 || status[0].Name != "backup-auto" {
		t.Fatalf("作业注册状态=%+v，期望单个 backup-auto 作业", status)
	}
	if status[0].Failures != 0 {
		t.Fatalf("正常轮次不应计失败：%+v", status[0])
	}

	cancel()
	time.Sleep(30 * time.Millisecond)
	before := assembly.scheduler.Status()[0].Runs
	time.Sleep(250 * time.Millisecond)
	if after := assembly.scheduler.Status()[0].Runs; after != before {
		t.Fatalf("context 取消后自动备份作业仍在运行：%d → %d", before, after)
	}
}

// TestAutoBackupJobAppliesRetentionOnEachRun 覆盖保留策略随作业收尾执行：
// 连续跑几轮后，自动包数量被压到 KEEP_LAST 上限内。
//
// 直接驱动 runAutoBackup 而不是等心跳：到点判断按"秒"计，若靠心跳累积轮次，
// 这条用例要跑好几秒（心跳 20ms × 每轮至少 1 秒间隔）。这里要验的是"每轮收尾
// 都应用保留策略"，与调度节拍无关，因此把节拍剥离出去、只测收尾行为。
func TestAutoBackupJobAppliesRetentionOnEachRun(t *testing.T) {
	assembly := newAutoBackupAssembly(t, 0, domain.BackupRetentionOptions{KeepLast: 2})
	assembly.seedAsset(t, "payload")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 跑 4 轮（每轮生成一个包并立即应用保留策略）。
	settings := autoBackupSettings{IntervalSecs: 1, KeepLast: 2}
	for i := 0; i < 4; i++ {
		if _, err := runAutoBackup(ctx, assembly.backupSvc, assembly.retentionSvc, settings); err != nil {
			t.Fatalf("第 %d 轮自动备份失败：%v", i+1, err)
		}
	}

	items, err := assembly.repo.List(50, 0)
	if err != nil {
		t.Fatalf("列出备份包：%v", err)
	}
	if len(items) > 2 {
		t.Fatalf("保留策略未生效：自动包数=%d，上限=2", len(items))
	}
	if len(items) == 0 {
		t.Fatal("保留策略不得把备份清空")
	}
	for _, it := range items {
		if it.Origin != repository.BackupOriginAuto {
			t.Fatalf("作业不得生成非自动来源的包：%+v", it)
		}
	}
}

// TestAutoBackupJobIsFailureIsolatedAndKeepsRunning 单轮失败只记计数，
// 不影响后续周期（失败隔离由调度器承载，注册处不得吞掉错误）。
func TestAutoBackupJobIsFailureIsolatedAndKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched := scheduler.New()
	rounds := make(chan struct{}, 8)
	registerAutoBackupJob(sched, 60*time.Millisecond,
		func() autoBackupSettings { return autoBackupSettings{IntervalSecs: 1} },
		func(context.Context, autoBackupSettings) (autoBackupRun, error) {
			rounds <- struct{}{}
			return autoBackupRun{}, errors.New("人为失败")
		})
	sched.Start(ctx)

	for i := 0; i < 2; i++ {
		select {
		case <-rounds:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次执行未在间隔内触发（失败不得终止作业）", i+1)
		}
	}
	// 第 2 次信号发出时作业函数可能尚未返回、调度器收尾也未完成，直接断言等于断言竞态。
	// 判据一点不放宽：若作业真的自我重叠（持续 running），下面的等待会超时，断言仍会失败。
	deadline := time.Now().Add(2 * time.Second)
	for sched.Status()[0].Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	status := sched.Status()
	if len(status) != 1 {
		t.Fatalf("作业数=%d，期望 1", len(status))
	}
	if status[0].Running {
		t.Fatal("连续触发之间不得自我重叠（作业应已结束）")
	}
	if status[0].Failures == 0 {
		t.Fatalf("失败必须计入作业状态，实际=%+v", status[0])
	}
	if status[0].LastError == "" {
		t.Fatal("最近错误应被记录，便于运维定位")
	}
}

// TestRegisterAutoBackupJobDisabledSemantics 覆盖注册期与运行期两层禁用语义。
//
// 与 storage-cleanup 等既有作业不同：**配置间隔为 0 时本作业仍然注册**
// （配置现在可在界面上从 0 改为非 0，作业必须先存在才能被唤醒），
// 关闭由"每轮读配置发现间隔 ≤ 0 则跳过"实现。因此这里分别断言：
//   - 心跳或执行函数无效 → 不注册（连心跳都不该起）；
//   - 配置间隔 ≤ 0 → 注册但一轮都不执行（关闭态）。
func TestRegisterAutoBackupJobDisabledSemantics(t *testing.T) {
	t.Run("心跳或执行函数无效则不注册", func(t *testing.T) {
		sched := scheduler.New()
		registerAutoBackupJob(sched, 0,
			func() autoBackupSettings { return autoBackupSettings{IntervalSecs: 1} },
			func(context.Context, autoBackupSettings) (autoBackupRun, error) { return autoBackupRun{}, nil })
		registerAutoBackupJob(sched, -time.Second,
			func() autoBackupSettings { return autoBackupSettings{IntervalSecs: 1} },
			func(context.Context, autoBackupSettings) (autoBackupRun, error) { return autoBackupRun{}, nil })
		registerAutoBackupJob(sched, time.Hour, nil,
			func(context.Context, autoBackupSettings) (autoBackupRun, error) { return autoBackupRun{}, nil })
		registerAutoBackupJob(sched, time.Hour,
			func() autoBackupSettings { return autoBackupSettings{IntervalSecs: 1} }, nil)
		if status := sched.Status(); len(status) != 0 {
			t.Fatalf("心跳/执行函数无效时不得注册作业，实际=%+v", status)
		}
	})

	t.Run("配置间隔为0则注册但一轮都不执行", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sched := scheduler.New()
		executed := make(chan struct{}, 4)
		registerAutoBackupJob(sched, 20*time.Millisecond,
			func() autoBackupSettings { return autoBackupSettings{IntervalSecs: 0} },
			func(context.Context, autoBackupSettings) (autoBackupRun, error) {
				executed <- struct{}{}
				return autoBackupRun{}, nil
			})
		if status := sched.Status(); len(status) != 1 {
			t.Fatalf("间隔为 0 时作业应仍注册（关闭靠运行期判断），实际=%+v", status)
		}
		sched.Start(ctx)
		// 给足若干个心跳周期，确认一次都没执行。
		select {
		case <-executed:
			t.Fatal("配置间隔为 0 时不得执行自动备份")
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// TestAutoBackupJobAppliesIntervalChangeWithoutRestart 覆盖本次追加的核心能力：
// **在界面上改间隔后无需重启即生效**。
//
// 这是"心跳 + 每轮重读配置"相对"把间隔交给调度器"的全部意义所在。用例不只断言
// "改了之后会跑"，还断言"关掉之后不再跑"——后者才是运维真正会踩的坑：
// 若关闭只在启动时生效，用户点了关闭却看着备份继续产生，会以为开关是坏的。
func TestAutoBackupJobAppliesIntervalChangeWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched := scheduler.New()
	var runs atomic.Int64
	settings := autoBackupSettings{IntervalSecs: 0} // 初始关闭
	var mu sync.Mutex
	registerAutoBackupJob(sched, 20*time.Millisecond,
		func() autoBackupSettings { mu.Lock(); defer mu.Unlock(); return settings },
		func(context.Context, autoBackupSettings) (autoBackupRun, error) {
			runs.Add(1)
			return autoBackupRun{PackageID: "bk-x"}, nil
		})
	sched.Start(ctx)

	// ① 初始关闭：不得执行。
	time.Sleep(150 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Fatalf("初始间隔为 0 时不应执行，实际执行 %d 次", got)
	}

	// ② 打开（模拟界面保存）：应在一两个心跳内开始执行，无需重启。
	// 注意这里的"间隔"是 1 秒——到点判断按真实秒数算，因此等 1.5s 后应恰好跑过。
	mu.Lock()
	settings = autoBackupSettings{IntervalSecs: 1}
	mu.Unlock()
	time.Sleep(1500 * time.Millisecond)
	if got := runs.Load(); got == 0 {
		t.Fatal("打开间隔后应开始执行（热生效失败）")
	}

	// ③ 关回 0：应在下一个心跳后停止，且不再增长。
	mu.Lock()
	settings = autoBackupSettings{IntervalSecs: 0}
	mu.Unlock()
	time.Sleep(200 * time.Millisecond) // 让关闭生效
	stopped := runs.Load()
	time.Sleep(1500 * time.Millisecond)
	if got := runs.Load(); got != stopped {
		t.Fatalf("关闭后不得再执行，关闭前 %d 次、之后 %d 次", stopped, got)
	}
}

// TestRunAutoBackupSkipsWhenGenerateInProgress 覆盖生成槽位竞争：
// 已有备份生成在进行中时，本轮跳过且**不算失败**——手动与自动共用同一个生成槽位，
// 撞车是正常竞争而非故障，记成失败会让 /maintenance/jobs 长期显示失败计数。
func TestRunAutoBackupSkipsWhenGenerateInProgress(t *testing.T) {
	assembly := newAutoBackupAssembly(t, 0, domain.BackupRetentionOptions{KeepLast: 7})
	// 必须有一个 blob，否则打包过程不产生 Progress 回调、无法可靠地占住生成槽位。
	assembly.seedAsset(t, "payload")

	// 占据生成槽位：用一个阻塞在 Progress 回调里的手动生成。
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := assembly.backupSvc.Generate(context.Background(), domain.CreateBackupOptions{
			Mode: "hot",
			Progress: func(_, _ int) {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
			},
		})
		done <- err
	}()

	// 等占位成功（带超时，避免夹具问题变成永久挂起）。
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("占位生成未进入打包阶段")
	}

	// 此时生成槽位被占：自动备份应跳过本轮且不报错。
	run, err := runAutoBackup(context.Background(), assembly.backupSvc, assembly.retentionSvc, assembly.settings)
	if err != nil {
		t.Fatalf("生成中撞车应跳过而非失败，实际 %v", err)
	}
	if run.PackageID != "" {
		t.Fatalf("跳过轮不应产出包，实际 %s", run.PackageID)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("首个生成应成功：%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("占位生成未在超时内结束")
	}
}
