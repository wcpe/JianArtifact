package domain_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func newApplyAuditService(t *testing.T, name string) (*persistence.DB, *domain.ReplicationService, *blobstore.Store, *repository.ReplicationApplyLogRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	blobs := blobstore.NewStore(filepath.Join(t.TempDir(), name+"-blobs"))
	repl := domain.NewReplicationService(
		repository.NewReplChangeRepo(db), repository.NewAssetRepo(db), repository.NewRepoRepo(db),
		repository.NewAclRepo(db), repository.NewUserRepo(db), repository.NewTokenRepo(db),
		repository.NewSettingRepo(db), blobs,
	)
	return db, repl, blobs, repository.NewReplicationApplyLogRepo(db)
}

func TestReplicationClientApplyAuditFailedThenApplied(t *testing.T) {
	_, repl, blobs, logs := newApplyAuditService(t, "failed-applied")
	var mu sync.Mutex
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		first := requests == 1
		mu.Unlock()
		change := repository.Change{Seq: 1, NodeID: "peer-node", Op: domain.OpPut, EntityType: domain.EntityUser, EntityKey: domain.UserKey("alice"), TS: "2026-08-18T00:00:00Z"}
		if first {
			change.EntityType = "unknown"
			change.EntityKey = "unknown:key"
		} else {
			change.Data = `{"username":"alice","role":"user","status":"active","passwordHash":"hash"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"changes": []repository.Change{change}, "latestSeq": 1})
	}))
	t.Cleanup(ts.Close)

	client := domain.NewReplicationClient(ts.URL, "token", repl, blobs, logs)
	if _, err := client.Sync(0); err == nil {
		t.Fatal("首次应用失败时应返回错误")
	}
	first, err := logs.Get("peer-node", 1)
	if err != nil {
		t.Fatalf("读取失败审计：%v", err)
	}
	if first.Result != domain.ApplyResultFailed || first.LastError == "" {
		t.Fatalf("失败审计状态不符：%+v", first)
	}
	stats, err := client.Sync(0)
	if err != nil {
		t.Fatalf("修复后重试：%v", err)
	}
	if stats.ToSeq != 1 {
		t.Fatalf("成功重试应推进水位：%+v", stats)
	}
	last, err := logs.Get("peer-node", 1)
	if err != nil {
		t.Fatalf("读取成功审计：%v", err)
	}
	if last.Result != domain.ApplyResultApplied || last.AttemptCount != 2 || last.LastError == "" {
		t.Fatalf("失败后成功状态不符且错误证据丢失：%+v", last)
	}
}

func TestReplicationClientAuditUpdatesAllBlobReferences(t *testing.T) {
	srcDB, err := persistence.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatalf("打开源库：%v", err)
	}
	t.Cleanup(func() { _ = srcDB.Close() })
	if err := srcDB.Migrate(); err != nil {
		t.Fatalf("源库迁移：%v", err)
	}
	srcBlobs := blobstore.NewStore(filepath.Join(t.TempDir(), "source-blobs"))
	hash, _, _, _, err := srcBlobs.Put(strings.NewReader("same blob"))
	if err != nil {
		t.Fatalf("写入源 blob：%v", err)
	}
	srcRepl := repository.NewReplChangeRepo(srcDB)
	repoData, _ := json.Marshal(domain.RepoChangeData{Name: "raw", Format: "raw", Type: "hosted", Visibility: "public"})
	if _, err := srcRepl.Append("peer-node", domain.OpPut, domain.EntityRepository, domain.RepoKey("raw"), string(repoData), "2026-08-18T00:00:00Z"); err != nil {
		t.Fatalf("写入仓库变更：%v", err)
	}
	for _, path := range []string{"a.txt", "b.txt"} {
		data, _ := json.Marshal(domain.AssetChangeData{Path: path, BlobHash: hash, Size: 9, ContentType: "text/plain"})
		if _, err := srcRepl.Append("peer-node", domain.OpPut, domain.EntityAsset, domain.AssetKey("raw", path), string(data), "2026-08-18T00:00:01Z"); err != nil {
			t.Fatalf("写入资产变更：%v", err)
		}
	}
	var blobGets int
	var mu sync.Mutex
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/cluster/sync/blob/") {
			mu.Lock()
			blobGets++
			mu.Unlock()
			rc, _, err := srcBlobs.Open(hash)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer func() { _ = rc.Close() }()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.Copy(w, rc)
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		changes, _ := srcRepl.ListSince(since, 500)
		_ = json.NewEncoder(w).Encode(map[string]any{"changes": changes, "latestSeq": 3})
	}))
	t.Cleanup(src.Close)

	_, dstRepl, dstBlobs, logs := newApplyAuditService(t, "destination")
	client := domain.NewReplicationClient(src.URL, "token", dstRepl, dstBlobs, logs)
	if _, err := client.Sync(0); err != nil {
		t.Fatalf("同步共享 blob：%v", err)
	}
	for _, seq := range []int64{2, 3} {
		entry, err := logs.Get("peer-node", seq)
		if err != nil {
			t.Fatalf("读取 seq=%d 审计：%v", seq, err)
		}
		if entry.Result != domain.ApplyResultApplied {
			t.Errorf("seq=%d 应更新为 applied：%+v", seq, entry)
		}
	}
	mu.Lock()
	gotBlobGets := blobGets
	mu.Unlock()
	if gotBlobGets != 1 {
		t.Fatalf("同一 blob 应只补拉一次，实际 %d 次", gotBlobGets)
	}
}

// TestReplicationClientPendingGroupsIsolatedByPeer 验证同一客户端切换两个对端时，
// 相同父键与序号的未就绪变更分别计数，不能提前触发永久跳过。
func TestReplicationClientPendingGroupsIsolatedByPeer(t *testing.T) {
	_, repl, blobs, logs := newApplyAuditService(t, "pending-peer-isolation")
	pending := func(node string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			changes := []repository.Change{}
			if since < 1 {
				changes = []repository.Change{{
					Seq: 1, NodeID: node, Op: domain.OpPut, EntityType: domain.EntityAsset,
					EntityKey: domain.AssetKey("shared-parent", "a.txt"),
					Data:      `{"path":"a.txt","size":1}`, TS: "2026-08-18T00:00:00Z",
				}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"changes": changes, "latestSeq": 1})
		}))
	}
	peerA := pending("node-a")
	peerB := pending("node-b")
	t.Cleanup(peerA.Close)
	t.Cleanup(peerB.Close)

	client := domain.NewReplicationClient(peerA.URL, "token-a", repl, blobs, logs)
	statsA, err := client.Sync(0)
	if err != nil {
		t.Fatalf("对端 A 首轮同步：%v", err)
	}
	client.SetPeer(peerB.URL, "token-b")
	statsB, err := client.Sync(0)
	if err != nil {
		t.Fatalf("对端 B 首轮同步：%v", err)
	}
	if statsA.Pending != 1 || statsB.Pending != 2 {
		t.Fatalf("首轮待重试统计不符：A=%+v B=%+v", statsA, statsB)
	}

	for i := 0; i < 9; i++ {
		client.SetPeer(peerA.URL, "token-a")
		if _, err := client.Sync(1); err != nil {
			t.Fatalf("对端 A 第 %d 轮同步：%v", i+2, err)
		}
		client.SetPeer(peerB.URL, "token-b")
		if _, err := client.Sync(1); err != nil {
			t.Fatalf("对端 B 第 %d 轮同步：%v", i+2, err)
		}
	}
	for name, peerURL := range map[string]string{"A": peerA.URL, "B": peerB.URL} {
		skipped, err := repl.ReadSkipped(peerURL)
		if err != nil {
			t.Fatalf("读取对端 %s 跳过集合：%v", name, err)
		}
		if _, ok := skipped[1]; ok {
			t.Fatalf("对端 %s 的 seq=1 不应在 10 轮时被永久跳过：%v", name, skipped)
		}
	}
}

// TestReplicationClientPendingReplayUsesOriginPeer 验证父就绪后的重放与审计继续使用
// 产生分组的对端，而不是使用随后 SetPeer 切换到的当前对端。
func TestReplicationClientPendingReplayUsesOriginPeer(t *testing.T) {
	db, repl, blobs, logs := newApplyAuditService(t, "pending-replay-peer")
	asset := repository.Change{
		Seq: 1, NodeID: "node-a", Op: domain.OpPut, EntityType: domain.EntityAsset,
		EntityKey: domain.AssetKey("shared-parent", "a.txt"),
		Data:      `{"path":"a.txt","size":1}`, TS: "2026-08-18T00:00:00Z",
	}
	peerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"changes": []repository.Change{asset}, "latestSeq": 1})
	}))
	peerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"changes": []repository.Change{}, "latestSeq": 1})
	}))
	t.Cleanup(peerA.Close)
	t.Cleanup(peerB.Close)

	client := domain.NewReplicationClient(peerA.URL, "token-a", repl, blobs, logs)
	stats, err := client.Sync(0)
	if err != nil {
		t.Fatalf("对端 A 首轮同步：%v", err)
	}
	if stats.Pending != 1 {
		t.Fatalf("父未就绪时应保留 1 条待重试：%+v", stats)
	}
	if _, err := repository.NewRepoRepo(db).Create("shared-parent", "raw", "hosted", "public", ""); err != nil {
		t.Fatalf("创建父仓库：%v", err)
	}

	client.SetPeer(peerB.URL, "token-b")
	if _, err := client.Sync(1); err != nil {
		t.Fatalf("切换对端后重放：%v", err)
	}
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM asset WHERE path='a.txt'`); err != nil {
		t.Fatalf("查询重放资产：%v", err)
	}
	if count != 1 {
		t.Fatalf("父就绪后应从产生分组的对端重放资产，count=%d", count)
	}
	entry, err := logs.Get("node-a", 1)
	if err != nil {
		t.Fatalf("读取重放审计：%v", err)
	}
	if entry.PeerURL != peerA.URL || entry.Result != domain.ApplyResultApplied {
		t.Fatalf("重放审计应保留来源对端 A：%+v", entry)
	}
}
