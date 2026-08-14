package domain_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTestReplSvc 打开临时 SQLite、迁移并装配 ReplicationService 及其依赖 Repo。
func newTestReplSvc(t *testing.T) (*domain.ReplicationService, *persistence.DB, *repository.AssetRepo, *repository.RepoRepo, *repository.AclRepo, *repository.UserRepo, *repository.TokenRepo) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repl-svc.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	assetRepo := repository.NewAssetRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	aclRepo := repository.NewAclRepo(db)
	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	repl := repository.NewReplChangeRepo(db)
	svc := domain.NewReplicationService(repl, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(db), mustBlobStore(t))
	return svc, db, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo
}

// TestReplicationNodeID 节点 ID 优先环境变量，其次持久化到 setting，重启后一致。
func TestReplicationNodeID(t *testing.T) {
	svc, db, _, _, _, _, _ := newTestReplSvc(t)

	// 未设环境变量：生成随机 ID 并持久化。
	if id := svc.NodeID(); len(id) != 32 {
		t.Fatalf("NodeID 应 32 hex，得 %q（len=%d）", id, len(id))
	}
	first := svc.NodeID()
	// 重建服务实例（同一 DB）：应读到持久化的相同 ID。
	repl := repository.NewReplChangeRepo(db)
	svc2 := domain.NewReplicationService(repl, repository.NewAssetRepo(db), repository.NewRepoRepo(db), repository.NewAclRepo(db), repository.NewUserRepo(db), repository.NewTokenRepo(db), repository.NewSettingRepo(db), mustBlobStore(t))
	if second := svc2.NodeID(); second != first {
		t.Errorf("重启后 NodeID 应一致，得 %q != %q", second, first)
	}

	// 环境变量优先。
	t.Setenv("JIAN_NODE_ID", "explicit-node")
	repl3 := repository.NewReplChangeRepo(db)
	svc3 := domain.NewReplicationService(repl3, repository.NewAssetRepo(db), repository.NewRepoRepo(db), repository.NewAclRepo(db), repository.NewUserRepo(db), repository.NewTokenRepo(db), repository.NewSettingRepo(db), mustBlobStore(t))
	if id := svc3.NodeID(); id != "explicit-node" {
		t.Errorf("环境变量应优先，得 %q", id)
	}
}

// TestReplicationRecordAndApplyAsset 制品写路径：Record 落日志，Apply 后元数据一致。
func TestReplicationRecordAndApplyAsset(t *testing.T) {
	svc, _, assetRepo, repoRepo, _, _, _ := newTestReplSvc(t)

	// 本地已有仓库（模拟对端先同步了 repository）。
	if _, err := repoRepo.Create("raw", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("建仓库：%v", err)
	}

	// 本节点 Record 一条 asset put。
	if err := svc.Record(domain.EntityAsset, domain.AssetKey("raw", "a/b.txt"), domain.OpPut,
		domain.AssetChangeData{Path: "a/b.txt", BlobHash: "h1", Size: 10, ContentType: "text/plain"}); err != nil {
		t.Fatalf("Record：%v", err)
	}

	// 从变更日志取到该变更并 Apply（同 DB：svc 即应用端），验证业务表落库。
	ch := mustLastChange(t, svc, domain.EntityAsset, domain.AssetKey("raw", "a/b.txt"))
	if err := svc.Apply(ch); err != nil {
		t.Fatalf("Apply：%v", err)
	}
	repo, err := repoRepo.GetByName("raw")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	got, err := assetRepo.GetByPath(repo.ID, "a/b.txt")
	if err != nil {
		t.Fatalf("查 asset：%v", err)
	}
	if got.BlobHash != "h1" || got.Size != 10 || got.ContentType != "text/plain" {
		t.Errorf("asset 元数据不符：%+v", got)
	}
}

// mustLastChange 取指定实体最近一条变更。
func mustLastChange(t *testing.T, svc *domain.ReplicationService, typ, key string) repository.Change {
	t.Helper()
	changes, err := svc.ListSince(0, 0)
	if err != nil {
		t.Fatalf("ListSince：%v", err)
	}
	for i := len(changes) - 1; i >= 0; i-- {
		if changes[i].EntityType == typ && changes[i].EntityKey == key {
			return changes[i]
		}
	}
	t.Fatalf("未找到变更 %s %s", typ, key)
	return repository.Change{}
}

// mustBlobStore 返回指向临时目录的 blob 存储（测试辅助）。
func mustBlobStore(t *testing.T) *blobstore.Store {
	t.Helper()
	return blobstore.NewStore(filepath.Join(t.TempDir(), "blobs"))
}

// TestReplicationApplyLWW 后写覆盖：本地已有更晚写入时，Apply 更早的对端变更跳过；更晚的应用。
func TestReplicationApplyLWW(t *testing.T) {
	db := newTestDB(t)
	userRepo := repository.NewUserRepo(db)
	repl := repository.NewReplChangeRepo(db)
	replSvc := domain.NewReplicationService(repl, repository.NewAssetRepo(db), repository.NewRepoRepo(db), repository.NewAclRepo(db), userRepo, repository.NewTokenRepo(db), repository.NewSettingRepo(db), mustBlobStore(t))
	userSvc := domain.NewUserService(userRepo)
	userSvc.SetChangeRecorder(replSvc)

	// 本地写入：建 bob（role=user，Record ts≈now），随后本地把角色改为 admin（再次 Record，ts 更晚）。
	u, err := userSvc.Create("bob", "pw12345", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	if _, err := userSvc.Update(u.ID, "admin", ""); err != nil {
		t.Fatalf("本地改 admin：%v", err)
	}

	// 对端两条变更：early（本地写入之前，应被 LWW 跳过）、late（本地写入之后，应覆盖）。
	now := time.Now().UTC()
	early := repository.Change{TS: now.Add(-time.Hour).Format(time.RFC3339Nano), NodeID: "node-remote", Op: domain.OpPut, EntityType: domain.EntityUser, EntityKey: domain.UserKey("bob"), Data: `{"username":"bob","role":"user","status":"active","passwordHash":"h1"}`}
	late := repository.Change{TS: now.Add(time.Hour).Format(time.RFC3339Nano), NodeID: "node-remote", Op: domain.OpPut, EntityType: domain.EntityUser, EntityKey: domain.UserKey("bob"), Data: `{"username":"bob","role":"user","status":"active","passwordHash":"h1"}`}

	// Apply early：本地更晚，应跳过，bob 保持 admin。
	if err := replSvc.Apply(early); err != nil {
		t.Fatalf("Apply early：%v", err)
	}
	bob, err := userRepo.GetByUsername("bob")
	if err != nil {
		t.Fatalf("GetByUsername：%v", err)
	}
	if bob.Role != "admin" {
		t.Errorf("LWW 后本地应保持 admin，得 role=%s", bob.Role)
	}

	// Apply late：对端更晚，应覆盖为 user。
	if err := replSvc.Apply(late); err != nil {
		t.Fatalf("Apply late：%v", err)
	}
	bob, _ = userRepo.GetByUsername("bob")
	if bob.Role != "user" {
		t.Errorf("Apply late 后应覆盖为 user，得 role=%s", bob.Role)
	}
}

// TestReplicationApplyTombstone 删除 tombstone：Apply delete 删本地实体；更晚 put 可复活。
func TestReplicationApplyTombstone(t *testing.T) {
	svc, _, _, repoRepo, _, userRepo, _ := newTestReplSvc(t)
	if _, err := repoRepo.Create("raw", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	uid, err := userRepo.Create("carol", "hash1", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}

	put := repository.Change{TS: "2026-08-13T00:00:01Z", NodeID: "remote", Op: domain.OpPut, EntityType: domain.EntityUser, EntityKey: domain.UserKey("carol"), Data: `{"username":"carol","role":"admin","status":"active","passwordHash":"h2"}`}
	tomb := repository.Change{TS: "2026-08-13T00:00:02Z", NodeID: "remote", Op: domain.OpDelete, EntityType: domain.EntityUser, EntityKey: domain.UserKey("carol"), Data: `{"deleted":true}`}
	revive := repository.Change{TS: "2026-08-13T00:00:03Z", NodeID: "remote", Op: domain.OpPut, EntityType: domain.EntityUser, EntityKey: domain.UserKey("carol"), Data: `{"username":"carol","role":"user","status":"active","passwordHash":"h3"}`}

	// put 应用 → 用户存在。
	if err := svc.Apply(put); err != nil {
		t.Fatalf("Apply put：%v", err)
	}
	if _, err := userRepo.GetByUsername("carol"); err != nil {
		t.Errorf("put 后用户应存在：%v", err)
	}
	// tombstone 应用 → 删除。
	if err := svc.Apply(tomb); err != nil {
		t.Fatalf("Apply tomb：%v", err)
	}
	if _, err := userRepo.GetByUsername("carol"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("tombstone 后用户应删除，得 %v", err)
	}
	// 更晚 put → 复活。
	if err := svc.Apply(revive); err != nil {
		t.Fatalf("Apply revive：%v", err)
	}
	carol, err := userRepo.GetByUsername("carol")
	if err != nil || carol.Role != "user" {
		t.Errorf("更晚 put 应复活为 user，得 err=%v carol=%+v", err, carol)
	}
	_ = uid
}

// TestReplicationRecordAllWritePaths 各写路径 service 接入后 Record 产生变更日志（FR-83 验收①）。
func TestReplicationRecordAllWritePaths(t *testing.T) {
	db := newTestDB(t)
	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	aclRepo := repository.NewAclRepo(db)
	repl := repository.NewReplChangeRepo(db)
	replSvc := domain.NewReplicationService(repl, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo, repository.NewSettingRepo(db), mustBlobStore(t))

	// 各写路径 service 注入 recorder。
	userSvc := domain.NewUserService(userRepo)
	userSvc.SetChangeRecorder(replSvc)
	tokenSvc := domain.NewTokenService(tokenRepo, userRepo)
	tokenSvc.SetChangeRecorder(replSvc)
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(db))
	settingSvc.SetChangeRecorder(replSvc)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, settingSvc, userRepo)
	repoSvc.SetChangeRecorder(replSvc)
	assetSvc := domain.NewAssetService(repoRepo, assetRepo, mustBlobStore(t), nil)
	assetSvc.SetChangeRecorder(replSvc)

	// user create → repl_change 有 user put。
	u, err := userSvc.Create("dave", "pw12345", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	// token create → user 存在，token put。
	if _, _, err := tokenSvc.Create(u.ID, "ci"); err != nil {
		t.Fatalf("签发 Token：%v", err)
	}
	// setting 写入。
	if err := settingSvc.SetAnonymousAccessEnabled(false); err != nil {
		t.Fatalf("写设置：%v", err)
	}
	// repository create + acl + asset put。
	r, err := repoSvc.Create("raw", "raw", "hosted", "private", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := repoSvc.SetAcl("raw", []repository.Acl{{SubjectID: u.ID, Action: "read"}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	if _, err := assetSvc.Put("raw", "x/y.txt", strings.NewReader("hi"), "text/plain"); err != nil {
		t.Fatalf("传制品：%v", err)
	}
	// user delete → tombstone。
	if err := userSvc.Delete(u.ID); err != nil {
		t.Fatalf("删用户：%v", err)
	}
	// repository delete → tombstone。
	if err := repoSvc.Delete("raw"); err != nil {
		t.Fatalf("删仓库：%v", err)
	}

	// 断言变更日志覆盖各实体类型。
	changes, err := repl.ListSince(0, 0)
	if err != nil {
		t.Fatalf("ListSince：%v", err)
	}
	types := map[string]bool{}
	ops := map[string]bool{}
	for _, c := range changes {
		types[c.EntityType] = true
		ops[c.Op] = true
		// 校验 data 是合法 JSON。
		if !json.Valid([]byte(c.Data)) {
			t.Errorf("data 非法 JSON：%s", c.Data)
		}
	}
	for _, want := range []string{domain.EntityUser, domain.EntityToken, domain.EntitySetting, domain.EntityRepository, domain.EntityAcl, domain.EntityAsset} {
		if !types[want] {
			t.Errorf("变更日志缺实体类型 %s", want)
		}
	}
	if !ops[domain.OpPut] || !ops[domain.OpDelete] {
		t.Errorf("变更日志应含 put 与 delete 操作，得 %v", ops)
	}
	_ = r
}

// seedHistoryData 构造历史回填所需的存量数据：普通用户 alice（内置 anonymous 由迁移创建）、
// 一枚未吊销与一枚已吊销令牌、raw 仓库、ACL 与制品。
func seedHistoryData(t *testing.T, userRepo *repository.UserRepo, tokenRepo *repository.TokenRepo, repoRepo *repository.RepoRepo, aclRepo *repository.AclRepo, assetRepo *repository.AssetRepo) {
	t.Helper()
	aliceID, err := userRepo.Create("alice", "argon2-hash-alice", "admin")
	if err != nil {
		t.Fatalf("建用户 alice：%v", err)
	}
	// 未吊销令牌。
	if _, err := tokenRepo.Create(aliceID, "active-token", "digest-active"); err != nil {
		t.Fatalf("建令牌：%v", err)
	}
	// 已吊销令牌。
	revokedID, err := tokenRepo.Create(aliceID, "revoked-token", "digest-revoked")
	if err != nil {
		t.Fatalf("建待吊销令牌：%v", err)
	}
	if err := tokenRepo.Delete(revokedID, aliceID); err != nil {
		t.Fatalf("吊销令牌：%v", err)
	}
	repoID, err := repoRepo.Create("raw", "raw", "hosted", "private", `{"remoteUrl":""}`)
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := aclRepo.Replace(repoID, []repository.Acl{{SubjectID: aliceID, Action: "read"}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	if err := assetRepo.Upsert(repoID, "a/b.txt", "blob-hash-1", 10, "text/plain", "sha1-1", "md5-1"); err != nil {
		t.Fatalf("写制品：%v", err)
	}
}

// TestBackfillHistory 历史回填生成 put 日志，顺序为用户 → 令牌 → 仓库 → ACL → 制品。
func TestBackfillHistory(t *testing.T) {
	svc, db, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo := newTestReplSvc(t)
	repl := repository.NewReplChangeRepo(db)
	seedHistoryData(t, userRepo, tokenRepo, repoRepo, aclRepo, assetRepo)

	if err := svc.BackfillHistory(); err != nil {
		t.Fatalf("BackfillHistory：%v", err)
	}

	// 完成标记。
	v, err := repository.NewSettingRepo(db).Get(domain.SettingKeyReplBackfill)
	if err != nil || v != "true" {
		t.Errorf("回填后应写完成标记 repl:backfill_done=true，得 %q err=%v", v, err)
	}

	changes, err := repl.ListSince(0, 0)
	if err != nil {
		t.Fatalf("ListSince：%v", err)
	}

	// 实体类型首次出现顺序 = 用户 → 令牌 → 仓库 → ACL → 制品（满足 Apply 依赖）。
	var order []string
	seen := map[string]bool{}
	for _, c := range changes {
		if !seen[c.EntityType] {
			seen[c.EntityType] = true
			order = append(order, c.EntityType)
		}
		if c.Op != domain.OpPut {
			t.Errorf("回填日志应为 put，得 %s（%s:%s）", c.Op, c.EntityType, c.EntityKey)
		}
		if !json.Valid([]byte(c.Data)) {
			t.Errorf("回填 data 非法 JSON：%s", c.Data)
		}
	}
	wantOrder := []string{domain.EntityUser, domain.EntityToken, domain.EntityRepository, domain.EntityAcl, domain.EntityAsset}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("回填顺序应 %v，得 %v", wantOrder, order)
	}

	// 各实体日志存在（按自然键）。
	for _, key := range []string{
		domain.UserKey("alice"),
		domain.TokenKey("digest-active"),
		domain.RepoKey("raw"),
		domain.AclKey("raw"),
		domain.AssetKey("raw", "a/b.txt"),
	} {
		if !hasChangeKey(t, changes, key) {
			t.Errorf("回填缺实体日志：%s", key)
		}
	}
}

// TestBackfillHistoryIdempotent 重复回填不新增日志（完成标记生效）。
func TestBackfillHistoryIdempotent(t *testing.T) {
	svc, db, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo := newTestReplSvc(t)
	repl := repository.NewReplChangeRepo(db)
	seedHistoryData(t, userRepo, tokenRepo, repoRepo, aclRepo, assetRepo)

	if err := svc.BackfillHistory(); err != nil {
		t.Fatalf("首次回填：%v", err)
	}
	first, err := repl.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq：%v", err)
	}
	if err := svc.BackfillHistory(); err != nil {
		t.Fatalf("重复回填：%v", err)
	}
	second, _ := repl.LatestSeq()
	if second != first {
		t.Errorf("重复回填不应新增日志：seq %d → %d", first, second)
	}
}

// TestBackfillHistoryExcludes 回填排除内置 anonymous 用户与已吊销令牌。
func TestBackfillHistoryExcludes(t *testing.T) {
	svc, db, assetRepo, repoRepo, aclRepo, userRepo, tokenRepo := newTestReplSvc(t)
	repl := repository.NewReplChangeRepo(db)
	seedHistoryData(t, userRepo, tokenRepo, repoRepo, aclRepo, assetRepo)

	if err := svc.BackfillHistory(); err != nil {
		t.Fatalf("BackfillHistory：%v", err)
	}
	changes, err := repl.ListSince(0, 0)
	if err != nil {
		t.Fatalf("ListSince：%v", err)
	}
	for _, c := range changes {
		if c.EntityType == domain.EntityUser && strings.TrimPrefix(c.EntityKey, "user:") == "anonymous" {
			t.Errorf("回填不应含内置 anonymous 用户：%s", c.EntityKey)
		}
		if c.EntityType == domain.EntityToken && strings.Contains(c.EntityKey, "digest-revoked") {
			t.Errorf("回填不应含已吊销令牌：%s", c.EntityKey)
		}
	}
}

// hasChangeKey 判断变更列表中是否存在指定实体键。
func hasChangeKey(t *testing.T, changes []repository.Change, key string) bool {
	t.Helper()
	for _, c := range changes {
		if c.EntityKey == key {
			return true
		}
	}
	return false
}
