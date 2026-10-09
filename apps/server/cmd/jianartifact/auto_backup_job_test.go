package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
}

// newAutoBackupAssembly 建一个真实装配；interval <= 0 时作业不会注册（禁用语义）。
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
	sched := scheduler.New()
	registerAutoBackupJob(sched, interval, func(ctx context.Context) (autoBackupRun, error) {
		return runAutoBackup(ctx, backupSvc, retentionSvc)
	})
	return &autoBackupAssembly{
		scheduler: sched, backupSvc: backupSvc, retentionSvc: retentionSvc,
		repo: repo, db: db, blobs: blobs, dataDir: dataDir,
	}
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
// 间隔跑几轮后，自动包数量被压到 KEEP_LAST 上限内。
func TestAutoBackupJobAppliesRetentionOnEachRun(t *testing.T) {
	assembly := newAutoBackupAssembly(t, 80*time.Millisecond, domain.BackupRetentionOptions{KeepLast: 2})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assembly.scheduler.Start(ctx)

	// 等至少 4 轮跑完。Runs 在每轮**开始时**计入，因此还必须等这一轮真正结束
	// （Running 落回 false），否则会读到"策略尚未应用"的中间态。
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := assembly.scheduler.Status()
		if status[0].Runs >= 4 && !status[0].Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("自动备份作业未跑够轮次：%+v", assembly.scheduler.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

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
	registerAutoBackupJob(sched, 60*time.Millisecond, func(context.Context) (autoBackupRun, error) {
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

// TestRegisterAutoBackupJobDisabledSemantics 间隔 <= 0 或执行函数为空时不注册（禁用语义）。
// 本作业的默认间隔就是 0，因此这同时守护了"升级后不会悄悄开始自动备份"。
func TestRegisterAutoBackupJobDisabledSemantics(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		fn       func(context.Context) (autoBackupRun, error)
	}{
		{name: "间隔为零（默认即关闭）", interval: 0, fn: func(context.Context) (autoBackupRun, error) { return autoBackupRun{}, nil }},
		{name: "间隔为负", interval: -time.Second, fn: func(context.Context) (autoBackupRun, error) { return autoBackupRun{}, nil }},
		{name: "执行函数为空", interval: time.Hour, fn: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := scheduler.New()
			registerAutoBackupJob(sched, tc.interval, tc.fn)
			if status := sched.Status(); len(status) != 0 {
				t.Fatalf("禁用语义下不得注册作业，实际=%+v", status)
			}
		})
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
	run, err := runAutoBackup(context.Background(), assembly.backupSvc, assembly.retentionSvc)
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
