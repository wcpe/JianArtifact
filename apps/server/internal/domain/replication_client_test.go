package domain_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	stats, err := client.Sync(0)
	if err != nil {
		t.Fatalf("Sync(0)：%v", err)
	}
	latest := stats.ToSeq
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
	firstStats, err := client.Sync(0)
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
	if _, err := client.Sync(firstStats.ToSeq); err != nil {
		t.Fatalf("增量 Sync：%v", err)
	}
	var n int
	if err := dstDB.Get(&n, `SELECT COUNT(*) FROM user WHERE username='bob'`); err != nil {
		t.Errorf("增量同步查询失败：%v", err)
	} else if n != 1 {
		t.Errorf("增量同步后目标库缺 bob，count=%d", n)
	}
}

// newEmptyReplicationClient 创建空目标节点客户端，供失败水位测试复用。
func newEmptyReplicationClient(t *testing.T, peerURL string) (*domain.ReplicationClient, *blobstore.Store) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "failure-dst.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), "failure-dst-blobs"))
	replSvc := domain.NewReplicationService(
		repository.NewReplChangeRepo(db), repository.NewAssetRepo(db), repository.NewRepoRepo(db),
		repository.NewAclRepo(db), repository.NewUserRepo(db), repository.NewTokenRepo(db),
		repository.NewSettingRepo(db), blobs)
	return domain.NewReplicationClient(peerURL, "t", replSvc, blobs), blobs
}

// TestReplicationClientSyncApplyFailureKeepsWatermark 应用失败时整轮失败且水位保持原值。
func TestReplicationClientSyncApplyFailureKeepsWatermark(t *testing.T) {
	invalid := repository.Change{Seq: 1, NodeID: "peer", Op: domain.OpPut, EntityType: "unknown", EntityKey: "unknown:k", Data: `{}`, TS: "2026-08-13T00:00:00Z"}
	valid := repository.Change{Seq: 1, NodeID: "peer", Op: domain.OpPut, EntityType: domain.EntityUser, EntityKey: domain.UserKey("retry-user"), Data: `{"username":"retry-user","role":"user","status":"active","passwordHash":"hash"}`, TS: "2026-08-13T00:00:00Z"}
	var mu sync.Mutex
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		change := valid
		if requests == 1 {
			change = invalid
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pullResponseForTest{Changes: []repository.Change{change}, LatestSeq: 1})
	}))
	t.Cleanup(ts.Close)

	client, _ := newEmptyReplicationClient(t, ts.URL)
	stats, err := client.Sync(0)
	if err == nil {
		t.Fatal("应用失败时 Sync 应返回错误")
	}
	if stats.ToSeq != 0 {
		t.Fatalf("应用失败不得推进水位，得 %d", stats.ToSeq)
	}
	retryStats, err := client.Sync(0)
	if err != nil {
		t.Fatalf("下一轮应从原水位补齐：%v", err)
	}
	if retryStats.ToSeq != 1 || retryStats.Applied != 1 {
		t.Fatalf("重试统计不符：%+v", retryStats)
	}
}

// TestReplicationClientSyncBlobFailureKeepsWatermark blob 缺失或哈希不匹配时不得计成功或推进水位。
func TestReplicationClientSyncBlobFailureKeepsWatermark(t *testing.T) {
	goodPayload := []byte("expected blob")
	badPayload := []byte("damaged blob")
	wantHash := sha256.Sum256(goodPayload)
	for _, tc := range []struct {
		name       string
		blobStatus int
		blobBody   []byte
	}{
		{name: "blob不存在", blobStatus: http.StatusNotFound},
		{name: "blob哈希不匹配", blobStatus: http.StatusOK, blobBody: badPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assetData, err := json.Marshal(domain.AssetChangeData{Path: "a.txt", BlobHash: fmt.Sprintf("%x", wantHash), Size: 13, ContentType: "text/plain"})
			if err != nil {
				t.Fatalf("编码 asset：%v", err)
			}
			changes := []repository.Change{
				{Seq: 1, NodeID: "peer", Op: domain.OpPut, EntityType: domain.EntityRepository, EntityKey: domain.RepoKey("raw"), Data: `{"name":"raw","format":"raw","type":"hosted","visibility":"private"}`, TS: "2026-08-13T00:00:01Z"},
				{Seq: 2, NodeID: "peer", Op: domain.OpPut, EntityType: domain.EntityAsset, EntityKey: domain.AssetKey("raw", "a.txt"), Data: string(assetData), TS: "2026-08-13T00:00:02Z"},
			}
			var blobMu sync.Mutex
			blobAttempts := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/v1/cluster/sync/blob/") {
					blobMu.Lock()
					blobAttempts++
					attempt := blobAttempts
					blobMu.Unlock()
					if attempt == 1 {
						w.WriteHeader(tc.blobStatus)
						_, _ = w.Write(tc.blobBody)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(goodPayload)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(pullResponseForTest{Changes: changes, LatestSeq: 2})
			}))
			t.Cleanup(ts.Close)

			client, blobs := newEmptyReplicationClient(t, ts.URL)
			stats, err := client.Sync(0)
			if err == nil {
				t.Fatal("blob 失败时 Sync 应返回错误")
			}
			if stats.ToSeq != 0 {
				t.Fatalf("blob 失败不得推进水位，得 %d", stats.ToSeq)
			}
			if stats.Blobs != 0 {
				t.Fatalf("blob 失败不得计成功，得 %d", stats.Blobs)
			}
			wantHashText := fmt.Sprintf("%x", wantHash)
			if blobs.Exists(wantHashText) {
				t.Fatal("目标哈希内容不正确时不得视为已补齐")
			}
			retryStats, err := client.Sync(0)
			if err != nil {
				t.Fatalf("下一轮应从原水位补齐 blob：%v", err)
			}
			if retryStats.ToSeq != 2 || retryStats.Blobs != 1 || !blobs.Exists(wantHashText) {
				t.Fatalf("blob 重试统计不符：%+v", retryStats)
			}
		})
	}
}

type pullResponseForTest struct {
	Changes   []repository.Change `json:"changes"`
	LatestSeq int64               `json:"latestSeq"`
}

// TestReplicationClientPullRetriesOnReset 对端首次请求中断连接（模拟 CDN/隧道偶发
// connection reset），Pull 应通过有界重试最终成功，而不是直接失败（修复间歇同步失败）。
func TestReplicationClientPullRetriesOnReset(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		first := attempts == 1
		mu.Unlock()
		if first {
			// 中断连接：hijack 后立即关闭，客户端读响应时收到连接重置。
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("测试服务端不支持 Hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changes":[],"latestSeq":101417}`))
	}))
	t.Cleanup(ts.Close)

	client := domain.NewReplicationClient(ts.URL, "t", nil, nil)
	changes, latest, err := client.Pull(101417, 500)
	if err != nil {
		t.Fatalf("Pull 应在重试后成功，得 %v", err)
	}
	if len(changes) != 0 || latest != 101417 {
		t.Errorf("返回内容不符：changes=%d latest=%d", len(changes), latest)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Errorf("应至少请求 2 次（1 次中断 + 重试），实际 %d 次", attempts)
	}
}

// TestReplicationClientParentNotReadyBuffered 父未就绪缓冲重试：资产变更先于仓库创建变更到达，
// Sync 不应卡死；父创建应用后，后续 Sync 重放资产成功（不再因父未就绪永久阻塞）。
func TestReplicationClientParentNotReadyBuffered(t *testing.T) {
	// ===== 被拉取方：手动构造乱序变更流（资产先于仓库创建） =====
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "src-parent.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srvRepos := repository.NewRepoRepo(srvDB)
	srvAssets := repository.NewAssetRepo(srvDB)
	srvAcls := repository.NewAclRepo(srvDB)
	srvUsers := repository.NewUserRepo(srvDB)
	srvTokens := repository.NewTokenRepo(srvDB)
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "src-blobs"))
	srvRepl := repository.NewReplChangeRepo(srvDB)
	_ = domain.NewReplicationService(srvRepl, srvAssets, srvRepos, srvAcls, srvUsers, srvTokens, repository.NewSettingRepo(srvDB), srvBlobs)

	// 预写 blob（内容寻址，拿真实哈希用于资产变更）。
	blobHash, _, _, _, err := srvBlobs.Put(strings.NewReader("blob"))
	if err != nil {
		t.Fatalf("写入源 blob：%v", err)
	}
	// 资产变更（seq=1）先于其仓库创建变更（seq=2）到达：模拟历史乱序数据。
	assetData, _ := json.Marshal(domain.AssetChangeData{Path: "dir/a.txt", BlobHash: blobHash, Size: 4})
	if _, err := srvRepl.Append("node-a", "put", domain.EntityAsset, domain.AssetKey("late-repo", "dir/a.txt"), string(assetData), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("Append 资产：%v", err)
	}
	repoData, _ := json.Marshal(domain.RepoChangeData{Name: "late-repo", Format: "raw", Type: "hosted", Visibility: "public"})
	if _, err := srvRepl.Append("node-a", "put", domain.EntityRepository, domain.RepoKey("late-repo"), string(repoData), "2026-01-01T00:00:01Z"); err != nil {
		t.Fatalf("Append 仓库：%v", err)
	}

	server := &mockSyncServer{repl: srvRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	// ===== 拉取方：空库 =====
	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "dst-parent.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstRepos := repository.NewRepoRepo(dstDB)
	dstAssets := repository.NewAssetRepo(dstDB)
	dstBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs"))
	dstRepl := repository.NewReplChangeRepo(dstDB)
	dstReplSvc := domain.NewReplicationService(dstRepl, dstAssets, dstRepos, repository.NewAclRepo(dstDB), repository.NewUserRepo(dstDB), repository.NewTokenRepo(dstDB), repository.NewSettingRepo(dstDB), dstBlobs)
	client := domain.NewReplicationClient(ts.URL, "t", dstReplSvc, dstBlobs)

	// 首次 Sync：资产父未就绪入待重试，但同批仓库创建随后应用 → 父就绪后重放资产成功，
	// 全程不卡死，仓库与资产最终都同步（验证父未就绪缓冲重放而非永久阻塞）。
	stats, err := client.Sync(0)
	if err != nil {
		t.Fatalf("首次 Sync 不应报错：%v", err)
	}
	var n int
	if err := dstDB.Get(&n, `SELECT COUNT(*) FROM repository WHERE name='late-repo'`); err != nil {
		t.Fatalf("查询目标仓库：%v", err)
	}
	if n != 1 {
		t.Fatalf("仓库创建应已同步，count=%d", n)
	}
	var an int
	if err := dstDB.Get(&an, `SELECT COUNT(*) FROM asset WHERE path='dir/a.txt'`); err != nil {
		t.Fatalf("查询目标资产：%v", err)
	}
	if an != 1 {
		t.Fatalf("父就绪后资产应重放同步，count=%d", an)
	}
	if stats.Pending != 0 {
		t.Fatalf("同批父就绪后不应有残留待重试，Pending=%d", stats.Pending)
	}
	_ = stats

	// 第二次 Sync：无新变更，水位稳定（幂等，不产生重复应用）。
	stats2, err := client.Sync(stats.ToSeq)
	if err != nil {
		t.Fatalf("二次 Sync：%v", err)
	}
	if err := dstDB.Get(&an, `SELECT COUNT(*) FROM asset WHERE path='dir/a.txt'`); err != nil {
		t.Fatalf("查询目标资产：%v", err)
	}
	if an != 1 {
		t.Fatalf("父就绪后资产应重放同步，count=%d", an)
	}
	_ = stats2
}

// TestReplicationClientParentNeverReadySkipped 父实体已删除（永久未就绪）：
// 连续 parentRetryMax 轮后应判定为永久脏数据并跳过，不再卡死、水位推进。
func TestReplicationClientParentNeverReadySkipped(t *testing.T) {
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "src-never.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srvRepos := repository.NewRepoRepo(srvDB)
	srvAcls := repository.NewAclRepo(srvDB)
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "src-blobs"))
	srvRepl := repository.NewReplChangeRepo(srvDB)
	_ = domain.NewReplicationService(srvRepl, repository.NewAssetRepo(srvDB), srvRepos, srvAcls, repository.NewUserRepo(srvDB), repository.NewTokenRepo(srvDB), repository.NewSettingRepo(srvDB), srvBlobs)

	// 仅 ACL 变更，父仓库从未存在（模拟已删除实体残留）。
	aclData, _ := json.Marshal(domain.AclChangeData{RepoName: "gone-repo", Entries: []domain.AclEntryData{{Username: "alice", Action: "read"}}})
	if _, err := srvRepl.Append("node-a", "put", domain.EntityAcl, domain.AclKey("gone-repo"), string(aclData), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("Append ACL：%v", err)
	}

	server := &mockSyncServer{repl: srvRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "dst-never.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstSettings := repository.NewSettingRepo(dstDB)
	dstRepl := repository.NewReplChangeRepo(dstDB)
	dstReplSvc := domain.NewReplicationService(dstRepl, repository.NewAssetRepo(dstDB), repository.NewRepoRepo(dstDB), repository.NewAclRepo(dstDB), repository.NewUserRepo(dstDB), repository.NewTokenRepo(dstDB), dstSettings, blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs")))
	client := domain.NewReplicationClient(ts.URL, "t", dstReplSvc, blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs2")))

	// 反复 Sync 达重试上限（每轮父不存在 attemptCount++，上限 20）。
	var to int64
	for i := 0; i <= 22; i++ {
		stats, err := client.Sync(to)
		if err != nil {
			t.Fatalf("第 %d 轮 Sync：%v", i, err)
		}
		to = stats.ToSeq
	}
	// 达上限后，该 seq 应被跳过并持久化。
	skipped, err := dstReplSvc.ReadSkipped(ts.URL)
	if err != nil {
		t.Fatalf("读 skipped：%v", err)
	}
	found := false
	for seq := range skipped {
		if seq == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("父永久未就绪的 seq=1 应进入跳过集合，得 %v", skipped)
	}
}

// TestReplicationClientParentAcrossBatches 父创建变更在失败条之后很远（>500，跨多批）：
// Sync 多轮后水位推进到父创建被应用、早期资产重放成功，不形成新死锁。
func TestReplicationClientParentAcrossBatches(t *testing.T) {
	srvDB, err := persistence.Open(filepath.Join(t.TempDir(), "src-cross.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srvDB.Close() })
	if err := srvDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srvBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "src-blobs"))
	blobHash, _, _, _, err := srvBlobs.Put(strings.NewReader("blob"))
	if err != nil {
		t.Fatalf("写入源 blob：%v", err)
	}
	srvRepos := repository.NewRepoRepo(srvDB)
	srvRepl := repository.NewReplChangeRepo(srvDB)
	_ = domain.NewReplicationService(srvRepl, repository.NewAssetRepo(srvDB), srvRepos, repository.NewAclRepo(srvDB), repository.NewUserRepo(srvDB), repository.NewTokenRepo(srvDB), repository.NewSettingRepo(srvDB), srvBlobs)

	// seq=1：资产变更，父仓库 far-repo 创建变更在 seq=502（跨 500 批边界）。
	assetData, _ := json.Marshal(domain.AssetChangeData{Path: "d.txt", BlobHash: blobHash, Size: 4})
	if _, err := srvRepl.Append("node-a", "put", domain.EntityAsset, domain.AssetKey("far-repo", "d.txt"), string(assetData), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("Append 资产：%v", err)
	}
	// seq=2..501：填充用户变更（无父依赖，正常应用），把仓库创建推到 502。
	for i := 0; i < 500; i++ {
		ud, _ := json.Marshal(domain.UserChangeData{Username: fmt.Sprintf("u%d", i), Role: "user", Status: "active", PasswordHash: "h"})
		if _, err := srvRepl.Append("node-a", "put", domain.EntityUser, domain.UserKey(fmt.Sprintf("u%d", i)), string(ud), "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("Append 填充用户：%v", err)
		}
	}
	// seq=502：仓库创建。
	repoData, _ := json.Marshal(domain.RepoChangeData{Name: "far-repo", Format: "raw", Type: "hosted", Visibility: "public"})
	if _, err := srvRepl.Append("node-a", "put", domain.EntityRepository, domain.RepoKey("far-repo"), string(repoData), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("Append 仓库：%v", err)
	}

	server := &mockSyncServer{repl: srvRepl, blobs: srvBlobs}
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)

	dstDB, err := persistence.Open(filepath.Join(t.TempDir(), "dst-cross.db"))
	if err != nil {
		t.Fatalf("打开目标库：%v", err)
	}
	t.Cleanup(func() { _ = dstDB.Close() })
	if err := dstDB.Migrate(); err != nil {
		t.Fatalf("目标库迁移：%v", err)
	}
	dstBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "dst-blobs"))
	dstRepl := repository.NewReplChangeRepo(dstDB)
	dstReplSvc := domain.NewReplicationService(dstRepl, repository.NewAssetRepo(dstDB), repository.NewRepoRepo(dstDB), repository.NewAclRepo(dstDB), repository.NewUserRepo(dstDB), repository.NewTokenRepo(dstDB), repository.NewSettingRepo(dstDB), dstBlobs)
	client := domain.NewReplicationClient(ts.URL, "t", dstReplSvc, dstBlobs)

	// 多轮 Sync 推进；父仓库创建（502）在第二批被应用后，资产重放成功。
	var to int64
	for round := 0; round < 5; round++ {
		stats, err := client.Sync(to)
		if err != nil {
			t.Fatalf("第 %d 轮 Sync：%v", round, err)
		}
		to = stats.ToSeq
	}
	// 断言仓库已同步、资产已同步（跨批父未就绪最终一致，不卡死）。
	var rn int
	if err := dstDB.Get(&rn, `SELECT COUNT(*) FROM repository WHERE name='far-repo'`); err != nil {
		t.Fatalf("查询目标仓库：%v", err)
	}
	if rn != 1 {
		t.Fatalf("跨批后 far-repo 应同步，count=%d", rn)
	}
	var an int
	if err := dstDB.Get(&an, `SELECT COUNT(*) FROM asset WHERE path='d.txt'`); err != nil {
		t.Fatalf("查询目标资产：%v", err)
	}
	if an != 1 {
		t.Fatalf("跨批后资产应重放同步，count=%d", an)
	}
}
