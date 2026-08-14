package domain_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// mockSyncServer 模拟"被拉取方"：真实 ReplChangeRepo + blobstore，暴露 pull/blob 端点，
// 并记录收到的全部请求方法（断言全程 GET）。
type mockSyncServer struct {
	repl     *repository.ReplChangeRepo
	blobs    *blobstore.Store
	reqs     []string // 收到的 "METHOD path" 记录
	mu       sync.Mutex
	blobGets int // blob 端点命中次数（验证只传缺失）
}

func (m *mockSyncServer) handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.GET("/api/v1/cluster/sync/pull", func(c *gin.Context) {
		m.record(c.Request.Method, c.Request.URL.Path)
		since, _ := strconv.ParseInt(c.DefaultQuery("since", "0"), 10, 64)
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "500"))
		changes, err := m.repl.ListSince(since, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		latest, _ := m.repl.LatestSeq()
		c.JSON(http.StatusOK, gin.H{"changes": changes, "latestSeq": latest})
	})
	r.GET("/api/v1/cluster/sync/blob/:hash", func(c *gin.Context) {
		m.record(c.Request.Method, c.Request.URL.Path)
		m.mu.Lock()
		m.blobGets++
		m.mu.Unlock()
		rc, _, err := m.blobs.Open(c.Param("hash"))
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		defer func() { _ = rc.Close() }()
		c.DataFromReader(http.StatusOK, -1, "application/octet-stream", rc, nil)
	})
	return r
}

func (m *mockSyncServer) record(method, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, method+" "+path)
}

// TestReplicationClientSync 覆盖验收①②③④：首启全量、双向同步、blob 只传缺失、全程 GET。
func TestReplicationClientSync(t *testing.T) {
	// ===== 被拉取方：有数据（user/repo/asset 变更日志 + blob） =====
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	userRepo := repository.NewUserRepo(srvDB)
	repoRepo := repository.NewRepoRepo(srvDB)
	assetRepo := repository.NewAssetRepo(srvDB)
	aclRepo := repository.NewAclRepo(srvDB)
	tokenRepo := repository.NewTokenRepo(srvDB)
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "src-blobs"))
	srvRepl := repository.NewReplChangeRepo(srvDB)
	srvReplSvc := domain.NewReplicationService(srvRepl, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(srvDB), srvBlobs)

	// 源节点经写路径产生真实变更日志与 blob。
	userSvc := domain.NewUserService(userRepo)
	userSvc.SetChangeRecorder(srvReplSvc)
	u, err := userSvc.Create("alice", "pw12345", "user")
	if err != nil {
		t.Fatalf("源建用户：%v", err)
	}
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(srvDB))
	settingSvc.SetChangeRecorder(srvReplSvc)
	if err := settingSvc.SetAnonymousAccessEnabled(false); err != nil {
		t.Fatalf("源写设置：%v", err)
	}
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, settingSvc, userRepo)
	repoSvc.SetChangeRecorder(srvReplSvc)
	if _, err := repoSvc.Create("raw", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("源建仓库：%v", err)
	}
	if _, err := repoSvc.SetAcl("raw", []repository.Acl{{SubjectID: u.ID, Action: "read"}}); err != nil {
		t.Fatalf("源写 ACL：%v", err)
	}
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, srvBlobs, nil)
	assetSvc.SetChangeRecorder(srvReplSvc)
	if _, err := assetSvc.Put("raw", "dir/a.txt", strings.NewReader("hello blob content"), "text/plain"); err != nil {
		t.Fatalf("源传制品：%v", err)
	}

	server := &mockSyncServer{repl: srvRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	// ===== 拉取方：空库，Sync(0) 首启全量 =====
	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "dst.db"))
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
	dstBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs"))
	dstRepl := repository.NewReplChangeRepo(dstDB)
	dstReplSvc := domain.NewReplicationService(dstRepl, dstAssetRepo, dstRepoRepo, dstAclRepo, dstUserRepo, dstTokenRepo, repository.NewSettingRepo(dstDB), dstBlobs)

	client := domain.NewReplicationClient(ts.URL, "sync-token", dstReplSvc, dstBlobs)
	latest, err := client.Sync(0)
	if err != nil {
		t.Fatalf("Sync(0)：%v", err)
	}
	if latest == 0 {
		t.Fatalf("Sync 后 latestSeq 应为正数，得 %d", latest)
	}

	// 断言：用户 / 仓库 / ACL / 设置 / asset 元数据全部同步。
	if _, err := dstUserRepo.GetByUsername("alice"); err != nil {
		t.Errorf("目标库缺用户：%v", err)
	}
	repo, err := dstRepoRepo.GetByName("raw")
	if err != nil {
		t.Fatalf("目标库缺仓库：%v", err)
	}
	acls, err := dstAclRepo.ListByRepo(repo.ID)
	if err != nil || len(acls) != 1 || acls[0].Action != "read" {
		t.Errorf("目标库 ACL 不符：%v %+v", err, acls)
	}
	asset, err := dstAssetRepo.GetByPath(repo.ID, "dir/a.txt")
	if err != nil {
		t.Fatalf("目标库缺 asset：%v", err)
	}
	// blob 应被补拉落盘。
	if !dstBlobs.Exists(asset.BlobHash) {
		t.Errorf("目标库缺 blob：%s", asset.BlobHash)
	}
	// 设置同步。
	enabled, err := domain.NewSettingService(repository.NewSettingRepo(dstDB)).AnonymousAccessEnabled()
	if err != nil || enabled {
		t.Errorf("目标库设置应同步为 false，得 enabled=%v err=%v", enabled, err)
	}

	// 断言：全程 GET（无 PUT/POST/PATCH/DELETE）。
	server.mu.Lock()
	for _, req := range server.reqs {
		method := strings.Fields(req)[0]
		if method != http.MethodGet {
			t.Errorf("复制请求非 GET：%s", req)
		}
	}
	blobGetsAfterFirst := server.blobGets
	server.mu.Unlock()

	// 第二次 Sync：无新变更，blob 不应重复拉取（只传缺失）。
	if _, err := client.Sync(latest); err != nil {
		t.Fatalf("第二次 Sync：%v", err)
	}
	server.mu.Lock()
	if server.blobGets != blobGetsAfterFirst {
		t.Errorf("blob 重复拉取：首次后 %d，二次后 %d", blobGetsAfterFirst, server.blobGets)
	}
	server.mu.Unlock()
}

// TestReplicationClientSyncIncremental 增量同步：新写入变更可被后续 Sync 拉到。
func TestReplicationClientSyncIncremental(t *testing.T) {
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "src2.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	userRepo := repository.NewUserRepo(srvDB)
	repoRepo := repository.NewRepoRepo(srvDB)
	assetRepo := repository.NewAssetRepo(srvDB)
	aclRepo := repository.NewAclRepo(srvDB)
	tokenRepo := repository.NewTokenRepo(srvDB)
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "src-blobs"))
	srvRepl := repository.NewReplChangeRepo(srvDB)
	srvReplSvc := domain.NewReplicationService(srvRepl, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(srvDB), srvBlobs)

	server := &mockSyncServer{repl: srvRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	// 拉取方空库先 Sync 一次（无数据）。
	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "dst2.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstReplSvc := domain.NewReplicationService(
		repository.NewReplChangeRepo(dstDB), repository.NewAssetRepo(dstDB), repository.NewRepoRepo(dstDB),
		repository.NewAclRepo(dstDB), repository.NewUserRepo(dstDB), repository.NewTokenRepo(dstDB),
		repository.NewSettingRepo(dstDB), blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs")))
	client := domain.NewReplicationClient(ts.URL, "t", dstReplSvc, blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs2")))
	first, err := client.Sync(0)
	if err != nil {
		t.Fatalf("首次 Sync：%v", err)
	}

	// 源节点新增用户（增量变更）。
	userSvc := domain.NewUserService(userRepo)
	userSvc.SetChangeRecorder(srvReplSvc)
	if _, err := userSvc.Create("bob", "pw12345", "user"); err != nil {
		t.Fatalf("源新增用户：%v", err)
	}

	// 增量 Sync：从 first 续拉。
	if _, err := client.Sync(first); err != nil {
		t.Fatalf("增量 Sync：%v", err)
	}
	var n int
	if err := dstDB.Get(&n, `SELECT COUNT(*) FROM user WHERE username='bob'`); err != nil {
		t.Errorf("增量同步查询失败：%v", err)
	} else if n != 1 {
		t.Errorf("增量同步后目标库缺 bob，count=%d", n)
	}
}
