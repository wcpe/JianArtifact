package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/scheduler"
)

// storageCleanupAssembly 是按真实装配构造的存储治理清理作业（SQLite + blobstore + 协调器 + 调度器）。
type storageCleanupAssembly struct {
	scheduler *scheduler.Scheduler
	service   *domain.StorageCleanupService
	blobRoot  string
}

func newStorageCleanupAssembly(t *testing.T, interval time.Duration) *storageCleanupAssembly {
	t.Helper()
	dataDir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dataDir, "jianartifact.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	blobRoot := filepath.Join(dataDir, "blobs")
	if err := os.MkdirAll(blobRoot, 0o750); err != nil {
		t.Fatalf("创建 blob 根目录：%v", err)
	}
	blobs := blobstore.NewStore(blobRoot)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	mutator, err := domain.NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	assetSvc := domain.NewAssetService(repos, assets, blobs, nil)
	assetSvc.SetMutationCoordinator(mutator)
	service := domain.NewStorageCleanupService(
		repository.NewStorageCleanupRepo(db),
		assetSvc,
		blobs,
		domain.StorageCleanupOptions{
			MetadataRetention: 7 * 24 * time.Hour,
			TempMaxAge:        24 * time.Hour,
		},
	)
	sched := scheduler.New()
	registerStorageCleanupJob(sched, interval, service.Run)
	return &storageCleanupAssembly{scheduler: sched, service: service, blobRoot: blobRoot}
}

// TestStorageCleanupJobDoesNotScanAtStartupAndRunsOnInterval 真实装配下的作业语义：
// 启动不扫描；第一次间隔到期后确实执行并清理过期上传临时文件。
func TestStorageCleanupJobDoesNotScanAtStartupAndRunsOnInterval(t *testing.T) {
	assembly := newStorageCleanupAssembly(t, 120*time.Millisecond)
	uploadDir := filepath.Join(assembly.blobRoot, "tmp", "oci-upload")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatalf("创建上传暂存目录：%v", err)
	}
	expired := filepath.Join(uploadDir, "upload-expired")
	if err := os.WriteFile(expired, []byte("partial"), 0o600); err != nil {
		t.Fatalf("写入过期上传临时文件：%v", err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(expired, stale, stale); err != nil {
		t.Fatalf("回填时间：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assembly.scheduler.Start(ctx)

	time.Sleep(40 * time.Millisecond)
	if _, err := os.Stat(expired); err != nil {
		t.Fatalf("清理作业不得在启动时立即执行：%v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(expired); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("清理作业未按间隔触发并清理过期上传临时文件")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status := assembly.scheduler.Status(); len(status) != 1 || status[0].Name != "storage-cleanup" {
		t.Fatalf("作业注册状态=%+v，期望单个 storage-cleanup 作业", status)
	}

	cancel()
	time.Sleep(30 * time.Millisecond)
	before := assembly.scheduler.Status()[0].Runs
	time.Sleep(200 * time.Millisecond)
	if after := assembly.scheduler.Status()[0].Runs; after != before {
		t.Fatalf("context 取消后清理作业仍在运行：%d → %d", before, after)
	}
}

// TestStorageCleanupJobIsFailureIsolatedAndKeepsRunning 单轮失败只记计数，
// 不影响后续周期（失败隔离由调度器承载，注册处不得吞掉错误）。
func TestStorageCleanupJobIsFailureIsolatedAndKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched := scheduler.New()
	rounds := make(chan struct{}, 8)
	registerStorageCleanupJob(sched, 60*time.Millisecond, func() (domain.StorageCleanupResult, error) {
		rounds <- struct{}{}
		return domain.StorageCleanupResult{}, errors.New("人为失败")
	})
	sched.Start(ctx)

	for i := 0; i < 2; i++ {
		select {
		case <-rounds:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次执行未在间隔内触发（失败不得终止作业）", i+1)
		}
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

// TestRegisterStorageCleanupJobDisabledSemantics 间隔 <= 0 或清理函数为空时不注册（禁用语义）。
func TestRegisterStorageCleanupJobDisabledSemantics(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		cleanup  func() (domain.StorageCleanupResult, error)
	}{
		{name: "间隔为零", interval: 0, cleanup: func() (domain.StorageCleanupResult, error) { return domain.StorageCleanupResult{}, nil }},
		{name: "间隔为负", interval: -time.Second, cleanup: func() (domain.StorageCleanupResult, error) { return domain.StorageCleanupResult{}, nil }},
		{name: "清理函数为空", interval: time.Hour, cleanup: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := scheduler.New()
			registerStorageCleanupJob(sched, tc.interval, tc.cleanup)
			if status := sched.Status(); len(status) != 0 {
				t.Fatalf("禁用语义下不得注册作业，实际=%+v", status)
			}
		})
	}
}
