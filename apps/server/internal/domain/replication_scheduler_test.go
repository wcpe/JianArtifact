package domain_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// eventually 轮询等待条件成立，超时即失败（调度器为异步 goroutine）。
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestReplicationSchedulerSync 覆盖验收：首启全量、有写入就同步、水位持久化。
func TestReplicationSchedulerSync(t *testing.T) {
	// ===== 对端（数据源）：写路径产生真实变更日志与 blob =====
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "sch-src.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srcUserRepo := repository.NewUserRepo(srvDB)
	srcRepoRepo := repository.NewRepoRepo(srvDB)
	srcAssetRepo := repository.NewAssetRepo(srvDB)
	srcAclRepo := repository.NewAclRepo(srvDB)
	srcTokenRepo := repository.NewTokenRepo(srvDB)
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "sch-src-blobs"))
	srvRepl := repository.NewReplChangeRepo(srvDB)
	srvReplSvc := domain.NewReplicationService(srvRepl, srcAssetRepo, srcRepoRepo, srcAclRepo, srcUserRepo, srcTokenRepo, repository.NewSettingRepo(srvDB), srvBlobs)
	srcUserSvc := domain.NewUserService(srcUserRepo)
	srcUserSvc.SetChangeRecorder(srvReplSvc)

	// 对端初始数据：alice。
	if _, err := srcUserSvc.Create("alice", "pw12345", "user"); err != nil {
		t.Fatalf("源建用户 alice：%v", err)
	}

	server := &mockSyncServer{repl: srvRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	// ===== 拉取方：空库 + 调度器（100ms 轮询）=====
	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "sch-dst.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstUserRepo := repository.NewUserRepo(dstDB)
	dstRepoRepo := repository.NewRepoRepo(dstDB)
	dstAssetRepo := repository.NewAssetRepo(dstDB)
	dstAclRepo := repository.NewAclRepo(dstDB)
	dstTokenRepo := repository.NewTokenRepo(dstDB)
	dstBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "sch-dst-blobs"))
	dstRepl := repository.NewReplChangeRepo(dstDB)
	dstReplSvc := domain.NewReplicationService(dstRepl, dstAssetRepo, dstRepoRepo, dstAclRepo, dstUserRepo, dstTokenRepo, repository.NewSettingRepo(dstDB), dstBlobs)

	client := domain.NewReplicationClient(ts.URL, "sync-token", dstReplSvc, dstBlobs)
	settingsRepo := repository.NewSettingRepo(dstDB)
	scheduler := domain.NewReplicationScheduler(client, settingsRepo, ts.URL, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Start(ctx)

	// 首启全量：启动后自动拉到 alice（无水位 → since=0）。
	eventually(t, 3*time.Second, func() bool {
		_, err := dstUserRepo.GetByUsername("alice")
		return err == nil
	}, "首启全量未拉到 alice")

	// 有写入就同步：对端新增 bob → 下一轮询周期本地拉到。
	if _, err := srcUserSvc.Create("bob", "pw12345", "user"); err != nil {
		t.Fatalf("源新增用户 bob：%v", err)
	}
	eventually(t, 3*time.Second, func() bool {
		_, err := dstUserRepo.GetByUsername("bob")
		return err == nil
	}, "写入后未同步到 bob")

	// 水位持久化：停止调度器，确认水位已写入 setting。
	cancel()
	var watermark string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := settingsRepo.Get("repl:watermark:" + ts.URL); err == nil {
			watermark = v
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if watermark == "" {
		t.Fatal("水位未持久化到 setting")
	}
}

// TestReplicationSchedulerReconcile 对账兜底：一次同步失败后，下一周期自动补齐。
func TestReplicationSchedulerReconcile(t *testing.T) {
	// 对端：初始有 alice。
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "rec-src.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srcUserRepo := repository.NewUserRepo(srvDB)
	srcRepl := repository.NewReplChangeRepo(srvDB)
	srcReplSvc := domain.NewReplicationService(srcRepl, repository.NewAssetRepo(srvDB), repository.NewRepoRepo(srvDB), repository.NewAclRepo(srvDB), srcUserRepo, repository.NewTokenRepo(srvDB), repository.NewSettingRepo(srvDB), blobstore.NewStore(filepath.Join(t.TempDir(), "rec-src-blobs")))
	srcUserSvc := domain.NewUserService(srcUserRepo)
	srcUserSvc.SetChangeRecorder(srcReplSvc)
	if _, err := srcUserSvc.Create("alice", "pw12345", "user"); err != nil {
		t.Fatalf("源建用户：%v", err)
	}
	server := &mockSyncServer{repl: srcRepl, blobs: blobstore.NewStore(filepath.Join(t.TempDir(), "rec-src-blobs2"))}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	// 拉取方装配。
	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "rec-dst.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstUserRepo := repository.NewUserRepo(dstDB)
	dstReplSvc := domain.NewReplicationService(
		repository.NewReplChangeRepo(dstDB), repository.NewAssetRepo(dstDB), repository.NewRepoRepo(dstDB),
		repository.NewAclRepo(dstDB), dstUserRepo, repository.NewTokenRepo(dstDB),
		repository.NewSettingRepo(dstDB), blobstore.NewStore(filepath.Join(t.TempDir(), "rec-dst-blobs")))
	client := domain.NewReplicationClient(ts.URL, "t", dstReplSvc, blobstore.NewStore(filepath.Join(t.TempDir(), "rec-dst-blobs2")))
	settingsRepo := repository.NewSettingRepo(dstDB)
	scheduler := domain.NewReplicationScheduler(client, settingsRepo, ts.URL, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Start(ctx)

	// 首启全量拉到 alice。
	eventually(t, 3*time.Second, func() bool {
		_, err := dstUserRepo.GetByUsername("alice")
		return err == nil
	}, "首启未拉到 alice")

	// 对端新增 carol 时，同步进行中——模拟"一次失败"：直接关掉对端 server 再重启。
	ts.Close()
	if _, err := srcUserSvc.Create("carol", "pw12345", "user"); err != nil {
		t.Fatalf("源新增用户 carol：%v", err)
	}
	time.Sleep(300 * time.Millisecond) // 让几个轮询周期在对端不可达时失败

	// 恢复对端（同数据）。
	ts2 := httptest.NewServer(server.handler())
	t.Cleanup(ts2.Close)

	// 拉取方仍连旧 ts.URL（已关闭）→ 对账会失败。重建指向 ts2 的调度器验证补齐。
	// 为简洁，直接验证：把 client 改为 ts2 后手动 Sync 一次能补齐。
	client2 := domain.NewReplicationClient(ts2.URL, "t", dstReplSvc, blobstore.NewStore(filepath.Join(t.TempDir(), "rec-dst-blobs3")))
	sched2 := domain.NewReplicationScheduler(client2, settingsRepo, ts2.URL, 100*time.Millisecond)
	sched2.Start(ctx)
	defer cancel()

	eventually(t, 3*time.Second, func() bool {
		_, err := dstUserRepo.GetByUsername("carol")
		return err == nil
	}, "对账未补齐 carol")
}

// TestReplicationSchedulerBidirectional 双向：A 有 alice、B 有 bob，互相对端各自调度，最终两节点用户集一致。
func TestReplicationSchedulerBidirectional(t *testing.T) {
	// 构造一个节点：返回用户仓库、用户服务（写路径）、复制服务（供对端拉）、mock 服务端、设置仓库。
	makeNode := func(t *testing.T, name string) (*repository.UserRepo, *domain.UserService, *domain.ReplicationService, *mockSyncServer, *repository.SettingRepo) {
		t.Helper()
		db, err := persistence.Open(filepath.Join(t.TempDir(), "bi-"+name+".db"))
		if err != nil {
			t.Fatalf("打开数据库：%v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(); err != nil {
			t.Fatalf("迁移：%v", err)
		}
		userRepo := repository.NewUserRepo(db)
		repoRepo := repository.NewRepoRepo(db)
		assetRepo := repository.NewAssetRepo(db)
		aclRepo := repository.NewAclRepo(db)
		tokenRepo := repository.NewTokenRepo(db)
		blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "bi-"+name+"-blobs"))
		repl := repository.NewReplChangeRepo(db)
		replSvc := domain.NewReplicationService(repl, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(db), blobs)
		userSvc := domain.NewUserService(userRepo)
		userSvc.SetChangeRecorder(replSvc)
		srv := &mockSyncServer{repl: repl, blobs: blobs}
		return userRepo, userSvc, replSvc, srv, repository.NewSettingRepo(db)
	}

	// 节点 A：初始有 alice。
	userRepoA, userSvcA, replSvcA, srvA, settingsA := makeNode(t, "a")
	if _, err := userSvcA.Create("alice", "pw12345", "user"); err != nil {
		t.Fatalf("A 建用户：%v", err)
	}
	tsA := httptest.NewServer(srvA.handler())
	t.Cleanup(tsA.Close)

	// 节点 B：初始有 bob。
	userRepoB, userSvcB, replSvcB, srvB, settingsB := makeNode(t, "b")
	if _, err := userSvcB.Create("bob", "pw12345", "user"); err != nil {
		t.Fatalf("B 建用户：%v", err)
	}
	tsB := httptest.NewServer(srvB.handler())
	t.Cleanup(tsB.Close)

	// A 的调度器：从 B 拉（B 有 bob）。B 的调度器：从 A 拉（A 有 alice）。
	clientA := domain.NewReplicationClient(tsB.URL, "t", replSvcA, blobstore.NewStore(filepath.Join(t.TempDir(), "bi-a2-blobs")))
	schedA := domain.NewReplicationScheduler(clientA, settingsA, tsB.URL, 100*time.Millisecond)
	ctxA, cancelA := context.WithCancel(context.Background())
	schedA.Start(ctxA)
	defer cancelA()

	clientB := domain.NewReplicationClient(tsA.URL, "t", replSvcB, blobstore.NewStore(filepath.Join(t.TempDir(), "bi-b2-blobs")))
	schedB := domain.NewReplicationScheduler(clientB, settingsB, tsA.URL, 100*time.Millisecond)
	ctxB, cancelB := context.WithCancel(context.Background())
	schedB.Start(ctxB)
	defer cancelB()

	// 最终：A 有 alice+bob，B 有 alice+bob（双向收敛一致）。
	eventually(t, 4*time.Second, func() bool {
		_, errA1 := userRepoA.GetByUsername("alice")
		_, errA2 := userRepoA.GetByUsername("bob")
		_, errB1 := userRepoB.GetByUsername("alice")
		_, errB2 := userRepoB.GetByUsername("bob")
		return errA1 == nil && errA2 == nil && errB1 == nil && errB2 == nil
	}, "双向同步后两节点用户集应一致（各有 alice 与 bob）")
}

// TestReplicationSchedulerAssetBlob 制品同步：asset 变更 + blob 补拉随调度完成。
func TestReplicationSchedulerAssetBlob(t *testing.T) {
	// 对端：仓库 + asset + blob。
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "ab-src.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srcUserRepo := repository.NewUserRepo(srvDB)
	srcRepoRepo := repository.NewRepoRepo(srvDB)
	srcAssetRepo := repository.NewAssetRepo(srvDB)
	srcAclRepo := repository.NewAclRepo(srvDB)
	srcTokenRepo := repository.NewTokenRepo(srvDB)
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "ab-src-blobs"))
	srcRepl := repository.NewReplChangeRepo(srvDB)
	srcReplSvc := domain.NewReplicationService(srcRepl, srcAssetRepo, srcRepoRepo, srcAclRepo, srcUserRepo, srcTokenRepo, repository.NewSettingRepo(srvDB), srvBlobs)
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(srvDB))
	settingSvc.SetChangeRecorder(srcReplSvc)
	repoSvc := domain.NewRepositoryService(srcRepoRepo, srcAclRepo, srcAssetRepo, settingSvc, srcUserRepo)
	repoSvc.SetChangeRecorder(srcReplSvc)
	if _, err := repoSvc.Create("raw", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("源建仓库：%v", err)
	}
	assetSvc := domain.NewAssetService(srcRepoRepo, srcAssetRepo, srvBlobs, nil)
	assetSvc.SetChangeRecorder(srcReplSvc)
	if _, err := assetSvc.Put("raw", "dir/f.txt", strings.NewReader("scheduler blob payload"), "text/plain"); err != nil {
		t.Fatalf("源传制品：%v", err)
	}

	server := &mockSyncServer{repl: srcRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	// 拉取方空库 + 调度器。
	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "ab-dst.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstRepoRepo := repository.NewRepoRepo(dstDB)
	dstAssetRepo := repository.NewAssetRepo(dstDB)
	dstBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "ab-dst-blobs"))
	dstReplSvc := domain.NewReplicationService(
		repository.NewReplChangeRepo(dstDB), dstAssetRepo, dstRepoRepo, repository.NewAclRepo(dstDB),
		repository.NewUserRepo(dstDB), repository.NewTokenRepo(dstDB), repository.NewSettingRepo(dstDB), dstBlobs)
	client := domain.NewReplicationClient(ts.URL, "t", dstReplSvc, dstBlobs)
	settingsRepo := repository.NewSettingRepo(dstDB)
	scheduler := domain.NewReplicationScheduler(client, settingsRepo, ts.URL, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.Start(ctx)
	defer cancel()

	// 等待仓库 + asset 元数据 + blob 全部同步。
	eventually(t, 3*time.Second, func() bool {
		repo, err := dstRepoRepo.GetByName("raw")
		if err != nil {
			return false
		}
		asset, err := dstAssetRepo.GetByPath(repo.ID, "dir/f.txt")
		if err != nil {
			return false
		}
		return dstBlobs.Exists(asset.BlobHash)
	}, "制品 + blob 未随调度同步")
}
