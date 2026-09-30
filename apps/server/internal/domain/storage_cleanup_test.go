package domain

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// storageCleanupFixture 是存储治理清理所需的最小真实装配：SQLite + blobstore + 变更协调器。
type storageCleanupFixture struct {
	db       *persistence.DB
	meta     *repository.StorageCleanupRepo
	assets   *repository.AssetRepo
	repos    *repository.RepoRepo
	blobs    *blobstore.Store
	svc      *AssetService
	clean    *StorageCleanupService
	blobRoot string
	now      time.Time
}

func newStorageCleanupFixture(t *testing.T, opts StorageCleanupOptions) *storageCleanupFixture {
	t.Helper()
	db := mutationTestDB(t)
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	if err := os.MkdirAll(blobRoot, 0o750); err != nil {
		t.Fatalf("创建 blob 根目录：%v", err)
	}
	blobs := blobstore.NewStore(blobRoot)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	mutator, err := NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	svc := NewAssetService(repos, assets, blobs, nil)
	svc.SetMutationCoordinator(mutator)
	fixture := &storageCleanupFixture{
		db:       db,
		meta:     repository.NewStorageCleanupRepo(db),
		assets:   assets,
		repos:    repos,
		blobs:    blobs,
		svc:      svc,
		blobRoot: blobRoot,
		// 与 SQLite datetime('now') 的秒精度对齐，避免时间窗边界测试被亚秒噪声干扰。
		now: time.Now().UTC().Truncate(time.Second),
	}
	fixture.clean = NewStorageCleanupService(fixture.meta, svc, blobs, opts)
	fixture.clean.now = func() time.Time { return fixture.now }
	return fixture
}

// seedMutation 直接写入一条资产操作 intent（含 1 条明细与 1 条隔离记录）。
// 直接落库而非走协调器：要构造的正是协调器正常路径不会留下的中间/终态组合。
func (f *storageCleanupFixture) seedMutation(t *testing.T, id, status, quarantineStatus string, updatedAt time.Time) string {
	t.Helper()
	hash := strings.Repeat("a1", 32)
	ts := storageSQLTime(updatedAt)
	if _, err := f.db.Exec(`INSERT INTO asset_mutation (id, status, created_at, updated_at, error)
		VALUES (?, ?, ?, ?, '')`, id, status, ts, ts); err != nil {
		t.Fatalf("写入操作 intent %s：%v", id, err)
	}
	if _, err := f.db.Exec(`INSERT INTO asset_mutation_item (operation_id, ordinal, repository_id, path)
		VALUES (?, 0, 1, ?)`, id, id+".bin"); err != nil {
		t.Fatalf("写入操作明细 %s：%v", id, err)
	}
	quarantinePath := filepath.Join(f.blobRoot, "quarantine", id, hash)
	if _, err := f.db.Exec(`INSERT INTO blob_quarantine (operation_id, blob_hash, quarantine_path, status, updated_at)
		VALUES (?, ?, ?, ?, ?)`, id, hash, quarantinePath, quarantineStatus, ts); err != nil {
		t.Fatalf("写入隔离记录 %s：%v", id, err)
	}
	return quarantinePath
}

func (f *storageCleanupFixture) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.Get(&n, query, args...); err != nil {
		t.Fatalf("统计行数：%v", err)
	}
	return n
}

func (f *storageCleanupFixture) mutationExists(t *testing.T, id string) bool {
	t.Helper()
	return f.countRows(t, `SELECT COUNT(*) FROM asset_mutation WHERE id=?`, id) == 1
}

func (f *storageCleanupFixture) mutationItemCount(t *testing.T, id string) int {
	t.Helper()
	return f.countRows(t, `SELECT COUNT(*) FROM asset_mutation_item WHERE operation_id=?`, id)
}

func (f *storageCleanupFixture) quarantineCount(t *testing.T, id string) int {
	t.Helper()
	return f.countRows(t, `SELECT COUNT(*) FROM blob_quarantine WHERE operation_id=?`, id)
}

// createRepository 建仓并返回仓库 ID；cacheRetentionDays 只对 proxy 有意义。
func (f *storageCleanupFixture) createRepository(t *testing.T, name, typ string, cacheRetentionDays int) int64 {
	t.Helper()
	cfg := repository.RepositoryConfig{CacheRetentionDays: cacheRetentionDays}
	if typ == "proxy" {
		cfg.RemoteURL = "https://proxy.example.org/" + name
	}
	configJSON, err := repository.EncodeRepositoryConfig(cfg)
	if err != nil {
		t.Fatalf("编码仓库配置：%v", err)
	}
	repoID, err := f.repos.Create(name, "raw", typ, "private", configJSON)
	if err != nil {
		t.Fatalf("创建仓库 %s：%v", name, err)
	}
	return repoID
}

// seedAsset 写入一个真实 blob 与对应的资产行，并把时间戳改到指定时刻。
func (f *storageCleanupFixture) seedAsset(t *testing.T, repoID int64, path, payload string, updatedAt time.Time) string {
	t.Helper()
	hash, _, _, size, err := f.blobs.Put(bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatalf("写入 blob：%v", err)
	}
	if err := f.assets.Upsert(repoID, path, hash, size, "application/octet-stream", "", ""); err != nil {
		t.Fatalf("写入资产 %s：%v", path, err)
	}
	ts := storageSQLTime(updatedAt)
	if _, err := f.assets.UpdateTimes(repoID, path, ts, ts); err != nil {
		t.Fatalf("回填资产时间 %s：%v", path, err)
	}
	return hash
}

func (f *storageCleanupFixture) assetCount(t *testing.T, repoID int64) int {
	t.Helper()
	return f.countRows(t, `SELECT COUNT(*) FROM asset WHERE repository_id=?`, repoID)
}

// makeQuarantineDir 在隔离区建一个操作目录，可选择写入若干文件，并把目录时间改到指定时刻。
func (f *storageCleanupFixture) makeQuarantineDir(t *testing.T, operationID string, files []string, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(f.blobRoot, "quarantine", operationID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("创建隔离目录：%v", err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("payload"), 0o600); err != nil {
			t.Fatalf("写入隔离文件：%v", err)
		}
	}
	if err := os.Chtimes(dir, modTime, modTime); err != nil {
		t.Fatalf("回填隔离目录时间：%v", err)
	}
	return dir
}

func mustRunCleanup(t *testing.T, fixture *storageCleanupFixture) StorageCleanupResult {
	t.Helper()
	result, err := fixture.clean.Run()
	if err != nil {
		t.Fatalf("执行清理作业：%v", err)
	}
	return result
}

// TestStorageCleanupPrunesOnlyTerminalMetadataPastRetention 元数据裁剪只认终态行且必须早于保留期；
// 在途行、未过期行以及「仍挂着非终态隔离记录的终态父行」都必须原样保留。
func TestStorageCleanupPrunesOnlyTerminalMetadataPastRetention(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 7 * 24 * time.Hour})
	d := 24 * time.Hour
	fixture.seedMutation(t, "op-old-completed", "completed", "deleted", fixture.now.Add(-8*d))
	fixture.seedMutation(t, "op-old-rolled-back", "rolled_back", "restored", fixture.now.Add(-8*d))
	fixture.seedMutation(t, "op-exact-retention", "completed", "deleted", fixture.now.Add(-7*d))
	fixture.seedMutation(t, "op-recent-completed", "completed", "deleted", fixture.now.Add(-6*d))
	fixture.seedMutation(t, "op-old-staged", "staged", "staged", fixture.now.Add(-8*d))
	fixture.seedMutation(t, "op-old-committing", "committing", "pending_gc", fixture.now.Add(-8*d))
	fixture.seedMutation(t, "op-old-rolling-back", "rolling_back", "staged", fixture.now.Add(-8*d))
	fixture.seedMutation(t, "op-old-prepared", "prepared", "staged", fixture.now.Add(-8*d))
	fixture.seedMutation(t, "op-old-completed-stranded", "completed", "pending_gc", fixture.now.Add(-30*d))

	result := mustRunCleanup(t, fixture)
	if result.MutationRows != 2 || result.ItemRows != 2 || result.QuarantineRows != 2 {
		t.Fatalf("裁剪计数=%+v，期望操作 2 行 / 明细 2 行 / 隔离 2 行", result)
	}
	for _, id := range []string{"op-old-completed", "op-old-rolled-back"} {
		if fixture.mutationExists(t, id) {
			t.Fatalf("应被裁剪的终态操作 %s 仍然存在", id)
		}
		if fixture.mutationItemCount(t, id) != 0 || fixture.quarantineCount(t, id) != 0 {
			t.Fatalf("操作 %s 的明细/隔离记录应随终态父行一并裁剪", id)
		}
	}
	kept := []string{
		"op-exact-retention",        // 边界：恰好等于保留期，严格小于才裁
		"op-recent-completed",       // 未过期
		"op-old-staged",             // 在途
		"op-old-committing",         // 在途
		"op-old-rolling-back",       // 待启动恢复推进
		"op-old-prepared",           // 待启动恢复推进
		"op-old-completed-stranded", // 终态但仍有 pending_gc 隔离记录：父行是唯一记录，必须保留
	}
	for _, id := range kept {
		if !fixture.mutationExists(t, id) {
			t.Fatalf("不得裁剪的操作 %s 被删除了", id)
		}
		if fixture.mutationItemCount(t, id) != 1 || fixture.quarantineCount(t, id) != 1 {
			t.Fatalf("操作 %s 的明细/隔离记录被误删", id)
		}
	}
}

// TestStorageCleanupRetentionZeroDisablesPruning 显式 0 必须完全禁用元数据裁剪。
func TestStorageCleanupRetentionZeroDisablesPruning(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 0, TempMaxAge: 0})
	fixture.seedMutation(t, "op-ancient-completed", "completed", "deleted", fixture.now.Add(-365*24*time.Hour))

	result := mustRunCleanup(t, fixture)
	if result.MutationRows != 0 || result.ItemRows != 0 || result.QuarantineRows != 0 {
		t.Fatalf("保留期为 0 时不得裁剪任何行，实际=%+v", result)
	}
	if !fixture.mutationExists(t, "op-ancient-completed") {
		t.Fatal("保留期为 0 时终态行必须保留")
	}
}

// TestStorageCleanupLeavesPendingQuarantineUntouched 是本批范围收缩的证伪式守护：
// 已被提交但物理回收未完成的滞留件（pending_gc + 隔离文件仍在），即便远超宽限期与保留期，
// 也不得被删除、推进或裁剪——在途判定接口不存在，任何回收动作都会破坏回滚原子性。
func TestStorageCleanupLeavesPendingQuarantineUntouched(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{
		MetadataRetention: time.Nanosecond,
		TempMaxAge:        time.Nanosecond,
	})
	opID := "op-stranded-pending-gc"
	quarantinePath := fixture.seedMutation(t, opID, "completed", "pending_gc", fixture.now.Add(-30*24*time.Hour))
	if err := os.MkdirAll(filepath.Dir(quarantinePath), 0o750); err != nil {
		t.Fatalf("创建隔离目录：%v", err)
	}
	if err := os.WriteFile(quarantinePath, []byte("stranded"), 0o600); err != nil {
		t.Fatalf("写入隔离文件：%v", err)
	}

	result := mustRunCleanup(t, fixture)
	if result.MutationRows != 0 || result.QuarantineRows != 0 || result.EmptyDirs != 0 {
		t.Fatalf("滞留件及其元数据必须原样保留，实际=%+v", result)
	}
	if _, err := os.Stat(quarantinePath); err != nil {
		t.Fatalf("滞留隔离文件不得被删除：%v", err)
	}
	if !fixture.mutationExists(t, opID) || fixture.quarantineCount(t, opID) != 1 {
		t.Fatal("滞留件的元数据必须保留（它是隔离文件可被后续回收的唯一依据）")
	}
}

// TestStorageCleanupRemovesOnlyAgedEmptyQuarantineDirs 空目录清理必须同时满足「目录为空」与「已过宽限期」。
func TestStorageCleanupRemovesOnlyAgedEmptyQuarantineDirs(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 0, TempMaxAge: 0})
	aged := fixture.makeQuarantineDir(t, "op-aged-empty", nil, fixture.now.Add(-2*time.Hour))
	fresh := fixture.makeQuarantineDir(t, "op-fresh-empty", nil, fixture.now.Add(-1*time.Minute))
	pending := fixture.makeQuarantineDir(t, "op-with-quarantine-file", []string{strings.Repeat("b2", 32)}, fixture.now.Add(-48*time.Hour))
	rollback := fixture.makeQuarantineDir(t, "op-with-rollback-only", []string{"rollback.tar"}, fixture.now.Add(-48*time.Hour))

	result := mustRunCleanup(t, fixture)
	if result.EmptyDirs != 1 {
		t.Fatalf("空隔离目录删除数=%d，期望 1", result.EmptyDirs)
	}
	if _, err := os.Stat(aged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("已过宽限期的空隔离目录应被删除：%v", err)
	}
	for _, dir := range []string{fresh, pending, rollback} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("不得删除隔离目录 %s：%v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(pending, strings.Repeat("b2", 32))); err != nil {
		t.Fatalf("非空隔离目录内的文件被误删：%v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.blobRoot, "quarantine")); err != nil {
		t.Fatalf("隔离区根目录本身不得被删除：%v", err)
	}
}

// TestStorageCleanupRemovesOnlyExpiredOCIUploadTemps 过期临时文件清理只碰 tmp/oci-upload 下的过期文件。
func TestStorageCleanupRemovesOnlyExpiredOCIUploadTemps(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 0, TempMaxAge: 24 * time.Hour})
	uploadDir := filepath.Join(fixture.blobRoot, "tmp", "oci-upload")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatalf("创建上传暂存目录：%v", err)
	}
	expired := filepath.Join(uploadDir, "upload-expired")
	fresh := filepath.Join(uploadDir, "upload-fresh")
	blobTmp := filepath.Join(fixture.blobRoot, "tmp", "blob-intermediate")
	for path, modTime := range map[string]time.Time{
		expired: fixture.now.Add(-48 * time.Hour),
		fresh:   fixture.now.Add(-1 * time.Hour),
		blobTmp: fixture.now.Add(-48 * time.Hour),
	} {
		if err := os.WriteFile(path, []byte("partial upload"), 0o600); err != nil {
			t.Fatalf("写入临时文件 %s：%v", path, err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("回填临时文件时间：%v", err)
		}
	}
	// 活动目录里的 blob 也在同一轮里被遍历到：必须毫发无损。
	activeHash, _, _, _, err := fixture.blobs.Put(bytes.NewReader([]byte("active-payload")))
	if err != nil {
		t.Fatalf("写入活动 blob：%v", err)
	}
	repoID := fixture.createRepository(t, "raw-hosted-tmp", "hosted", 0)
	fixture.seedAsset(t, repoID, "keep.bin", "keep-payload", fixture.now.Add(-365*24*time.Hour))

	result := mustRunCleanup(t, fixture)
	if result.TempFiles != 1 {
		t.Fatalf("过期上传临时文件删除数=%d，期望 1", result.TempFiles)
	}
	if _, err := os.Stat(expired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("过期上传临时文件应被删除：%v", err)
	}
	for _, path := range []string{fresh, blobTmp} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("不得删除 %s：%v", path, err)
		}
	}
	if !fixture.blobs.Exists(activeHash) {
		t.Fatal("活动 blob 不得被临时文件清理波及")
	}
	if _, err := fixture.assets.GetByPath(repoID, "keep.bin"); err != nil {
		t.Fatalf("hosted 仓库的资产不得被清理动到：%v", err)
	}
}

// TestStorageCleanupKeepsProxyCacheWhenRetentionDisabled 默认关闭：未配置 cacheRetentionDays 的
// proxy 仓库（以及任何 hosted 仓库）必须完全不受影响。
func TestStorageCleanupKeepsProxyCacheWhenRetentionDisabled(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 0, TempMaxAge: 0})
	proxyID := fixture.createRepository(t, "raw-proxy-default-off", "proxy", 0)
	proxyStaleHash := fixture.seedAsset(t, proxyID, "cached-old.bin", "cached-old", fixture.now.Add(-365*24*time.Hour))
	hostedID := fixture.createRepository(t, "raw-hosted-plain", "hosted", 0)
	hostedHash := fixture.seedAsset(t, hostedID, "old.bin", "hosted-old", fixture.now.Add(-365*24*time.Hour))

	result := mustRunCleanup(t, fixture)
	if result.CacheAssets != 0 {
		t.Fatalf("默认关闭时不得删除任何代理缓存资产，实际删除 %d 个", result.CacheAssets)
	}
	for _, tc := range []struct {
		repoID int64
		path   string
		hash   string
	}{
		{proxyID, "cached-old.bin", proxyStaleHash},
		{hostedID, "old.bin", hostedHash},
	} {
		if _, err := fixture.assets.GetByPath(tc.repoID, tc.path); err != nil {
			t.Fatalf("默认关闭时资产 %s 必须保留：%v", tc.path, err)
		}
		if !fixture.blobs.Exists(tc.hash) {
			t.Fatalf("默认关闭时资产 %s 的 blob 必须保留", tc.path)
		}
	}
}

// TestStorageCleanupPurgesOnlyExpiredProxyCacheWhenEnabled 开启后只删超期代理缓存，
// 且删除走既有资产变更通道（引用归零 → 同步物理回收）。
func TestStorageCleanupPurgesOnlyExpiredProxyCacheWhenEnabled(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 0, TempMaxAge: 0})
	const retentionDays = 7
	proxyID := fixture.createRepository(t, "raw-proxy-retain", "proxy", retentionDays)
	staleHash := fixture.seedAsset(t, proxyID, "stale.bin", "stale-cache", fixture.now.Add(-8*24*time.Hour))
	boundaryHash := fixture.seedAsset(t, proxyID, "boundary.bin", "boundary-cache", fixture.now.Add(-retentionDays*24*time.Hour))
	freshHash := fixture.seedAsset(t, proxyID, "fresh.bin", "fresh-cache", fixture.now.Add(-6*24*time.Hour))
	hostedID := fixture.createRepository(t, "raw-hosted-retain", "hosted", 0)
	hostedHash := fixture.seedAsset(t, hostedID, "old.bin", "hosted-old", fixture.now.Add(-365*24*time.Hour))

	result := mustRunCleanup(t, fixture)
	if result.CacheAssets != 1 {
		t.Fatalf("代理缓存删除数=%d，期望 1", result.CacheAssets)
	}
	if _, err := fixture.assets.GetByPath(proxyID, "stale.bin"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("超期代理缓存资产应被删除，实际 err=%v", err)
	}
	if fixture.blobs.Exists(staleHash) {
		t.Fatal("超期代理缓存引用的 blob 必须同步物理回收（ADR-0024 语义不变）")
	}
	for _, tc := range []struct {
		repoID int64
		path   string
		hash   string
	}{
		{proxyID, "boundary.bin", boundaryHash},
		{proxyID, "fresh.bin", freshHash},
		{hostedID, "old.bin", hostedHash},
	} {
		if _, err := fixture.assets.GetByPath(tc.repoID, tc.path); err != nil {
			t.Fatalf("不得删除未超期或非代理缓存的资产 %s：%v", tc.path, err)
		}
		if !fixture.blobs.Exists(tc.hash) {
			t.Fatalf("资产 %s 的 blob 不得被删除", tc.path)
		}
	}
}

// TestStorageCleanupCapsProxyCacheDeletesPerRun 每轮删除量必须有上限，剩余部分留到后续轮次。
func TestStorageCleanupCapsProxyCacheDeletesPerRun(t *testing.T) {
	fixture := newStorageCleanupFixture(t, StorageCleanupOptions{MetadataRetention: 0, TempMaxAge: 0})
	proxyID := fixture.createRepository(t, "raw-proxy-cap", "proxy", 1)
	total := maxProxyCacheDeletesPerRun + 5
	for i := 0; i < total; i++ {
		fixture.seedAsset(t, proxyID, fmt.Sprintf("cache-%04d.bin", i), fmt.Sprintf("payload-%d", i), fixture.now.Add(-30*24*time.Hour))
	}

	first := mustRunCleanup(t, fixture)
	if first.CacheAssets != maxProxyCacheDeletesPerRun {
		t.Fatalf("首轮删除数=%d，应被限制为 %d", first.CacheAssets, maxProxyCacheDeletesPerRun)
	}
	if remaining := fixture.assetCount(t, proxyID); remaining != total-maxProxyCacheDeletesPerRun {
		t.Fatalf("首轮后剩余资产=%d，期望 %d", remaining, total-maxProxyCacheDeletesPerRun)
	}

	second := mustRunCleanup(t, fixture)
	if second.CacheAssets != 5 {
		t.Fatalf("次轮删除数=%d，期望 5", second.CacheAssets)
	}
	if remaining := fixture.assetCount(t, proxyID); remaining != 0 {
		t.Fatalf("次轮后应清空超期代理缓存，剩余 %d 个", remaining)
	}
}
