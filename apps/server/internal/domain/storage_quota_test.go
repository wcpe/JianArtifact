package domain_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// rejectionSpy 记录配额服务上报的发布拒绝原因，用于验证指标累加与标签闭集。
type rejectionSpy struct {
	mu      sync.Mutex
	reasons []string
}

func (s *rejectionSpy) PublishRejection(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)
}

func (s *rejectionSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reasons)
}

// quotaAssembly 是按真实装配构造的配额测试环境：SQLite + blobstore + 资产协调器 +
// 仓库存储配额服务，提交点复检已接线，写入走真实的资产变更事务。
type quotaAssembly struct {
	db       *persistence.DB
	store    *blobstore.Store
	blobRoot string
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
	assetSvc *domain.AssetService
	quota    *domain.RepoQuotaService
	spy      *rejectionSpy
}

func newQuotaAssembly(t *testing.T) *quotaAssembly {
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
	store := blobstore.NewStore(blobRoot)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	mutator, err := domain.NewAssetMutationCoordinator(db, store)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	assetSvc := domain.NewAssetService(repos, assets, store, nil)
	assetSvc.SetMutationCoordinator(mutator)
	quota := domain.NewRepoQuotaService(repos, assets)
	spy := &rejectionSpy{}
	quota.SetRejectionRecorder(spy)
	assetSvc.SetStorageQuotaCheck(quota.CheckCommit)
	return &quotaAssembly{db: db, store: store, blobRoot: blobRoot, repos: repos, assets: assets, assetSvc: assetSvc, quota: quota, spy: spy}
}

// createHostedRepo 建一个 hosted 仓库并写入配额上限（0 表示不限）。
func (a *quotaAssembly) createHostedRepo(t *testing.T, name, format string, limits domain.RepoQuotaLimits) *repository.Repository {
	t.Helper()
	encoded, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{
		QuotaBytes:  limits.Bytes,
		QuotaAssets: limits.Assets,
	})
	if err != nil {
		t.Fatalf("编码仓库配置：%v", err)
	}
	id, err := a.repos.Create(name, format, "hosted", "public", encoded)
	if err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
	repo, err := a.repos.GetByID(id)
	if err != nil {
		t.Fatalf("读取仓库：%v", err)
	}
	return repo
}

// usage 读取仓库当前占用的聚合口径（件数与逻辑字节）。
func (a *quotaAssembly) usage(t *testing.T, repoID int64) (int64, int64) {
	t.Helper()
	count, size, err := a.assets.CountAndSizeByRepo(repoID)
	if err != nil {
		t.Fatalf("读取仓库占用：%v", err)
	}
	return count, size
}

// seedAsset 直接经资产服务写入一件制品（走真实提交事务）。
func (a *quotaAssembly) seedAsset(t *testing.T, repoName, path string, body []byte) {
	t.Helper()
	if _, err := a.assetSvc.Put(repoName, path, bytes.NewReader(body), "application/octet-stream"); err != nil {
		t.Fatalf("预置制品 %s：%v", path, err)
	}
}

// countBlobFilesOfSize 统计 blob 根目录下大小恰好为 size 的普通文件数量，
// 用于断言失败写入没有在磁盘上留下暂存内容。
func countBlobFilesOfSize(t *testing.T, root string, size int64) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() == size {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 blob 根目录：%v", err)
	}
	return count
}

// TestRepoQuotaAdmissionKnownLengthRejectsBeforeReadingBody 已知长度的预检：
// 越限时返回可按 ErrQuotaExceeded 识别的错误（协议层据此 429），且不写入任何内容。
func TestRepoQuotaAdmissionKnownLengthRejectsBeforeReadingBody(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-quota", "raw", domain.RepoQuotaLimits{Bytes: 100, Assets: 4})
	a.seedAsset(t, "raw-quota", "big.bin", bytes.Repeat([]byte("x"), 60))

	allowed, err := a.quota.Admission("raw-quota", "new.bin", 41)
	if !errors.Is(err, domain.ErrQuotaExceeded) || !errors.Is(err, domain.ErrRepoStorageQuotaExceeded) {
		t.Fatalf("超出字节上限必须在读取请求体之前拒绝，得 err=%v allowed=%d", err, allowed)
	}
	var quotaErr *domain.StorageQuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("拒绝错误应携带结构化占用信息，得 %T", err)
	}
	if quotaErr.Repository != repo.Name || quotaErr.UsedBytes != 60 || quotaErr.Limits.Bytes != 100 {
		t.Fatalf("拒绝错误携带的占用/上限不正确：%+v", quotaErr)
	}
	message := err.Error()
	if !strings.Contains(message, "当前占用") || !strings.Contains(message, "上限") {
		t.Fatalf("错误消息必须含「当前占用 / 上限」，得 %q", message)
	}
	if strings.Contains(message, string(filepath.Separator)) {
		t.Fatalf("错误消息不得回显文件系统路径，得 %q", message)
	}
	if a.spy.count() != 1 || a.spy.reasons[0] != domain.RejectionReasonQuota {
		t.Fatalf("拒绝必须累加一次 reason=quota，得 %v", a.spy.reasons)
	}
	count, size := a.usage(t, repo.ID)
	if count != 1 || size != 60 {
		t.Fatalf("预检不得改动仓库占用，得 count=%d size=%d", count, size)
	}

	// 正好用满（60 + 40 = 100）应放行，且不产生读取器上限。
	allowed, err = a.quota.Admission("raw-quota", "new.bin", 40)
	if err != nil || allowed != 0 {
		t.Fatalf("正好用满应放行且无需读取器上限，得 err=%v allowed=%d", err, allowed)
	}
}

// TestRepoQuotaAdmissionOverwriteUsesNetIncrement 覆盖写按净增量判定（新大小 − 被覆盖的旧大小）。
func TestRepoQuotaAdmissionOverwriteUsesNetIncrement(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-overwrite", "raw", domain.RepoQuotaLimits{Bytes: 100})
	a.seedAsset(t, "raw-overwrite", "app.jar", bytes.Repeat([]byte("a"), 60))

	if _, err := a.quota.Admission("raw-overwrite", "app.jar", 70); err != nil {
		t.Fatalf("覆盖写净增量 60→70 在 100 上限内应放行，得 %v", err)
	}
	if _, err := a.quota.Admission("raw-overwrite", "app.jar", 101); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("覆盖写净增量 60→101 超出上限应拒绝，得 %v", err)
	}
	if _, err := a.quota.Admission("raw-overwrite", "app.jar", 60); err != nil {
		t.Fatalf("同大小覆盖写（净增量为 0）应放行，得 %v", err)
	}
	count, size := a.usage(t, repo.ID)
	if count != 1 || size != 60 {
		t.Fatalf("预检不得改动仓库占用，得 count=%d size=%d", count, size)
	}
}

// TestRepoQuotaAdmissionAssetCountLimit 制品数上限：只有新增路径才增件，覆盖写不改变件数。
func TestRepoQuotaAdmissionAssetCountLimit(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-assets", "raw", domain.RepoQuotaLimits{Assets: 2})
	a.seedAsset(t, "raw-assets", "one.txt", []byte("1"))
	if _, err := a.quota.Admission("raw-assets", "two.txt", 1); err != nil {
		t.Fatalf("第 2 件（1+1=2，正好用满）应放行，得 %v", err)
	}
	a.seedAsset(t, "raw-assets", "two.txt", []byte("2"))

	if _, err := a.quota.Admission("raw-assets", "three.txt", 1); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("第 3 件超出件数上限应拒绝，得 %v", err)
	}
	if _, err := a.quota.Admission("raw-assets", "one.txt", 8); err != nil {
		t.Fatalf("覆盖既有路径不增件，应放行，得 %v", err)
	}
	count, _ := a.usage(t, repo.ID)
	if count != 2 {
		t.Fatalf("预检不得改动仓库占用，得 count=%d", count)
	}
}

// TestRepoQuotaAdmissionStreamingLimit 长度未知时的流式早拒上限：
// 路径已知按「剩余额度 + 覆盖写抵扣」给出上限；已无余量直接拒绝；路径未解析给安全上限。
func TestRepoQuotaAdmissionStreamingLimit(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-stream", "raw", domain.RepoQuotaLimits{Bytes: 100})
	a.seedAsset(t, "raw-stream", "app.jar", bytes.Repeat([]byte("a"), 60))

	limit, err := a.quota.Admission("raw-stream", "new.bin", -1)
	if err != nil || limit != 40 {
		t.Fatalf("长度未知且路径已知应给出剩余额度上限 40，得 limit=%d err=%v", limit, err)
	}
	limit, err = a.quota.Admission("raw-stream", "app.jar", -1)
	if err != nil || limit != 100 {
		t.Fatalf("覆盖写应抵扣旧大小（100-60+60=100），得 limit=%d err=%v", limit, err)
	}
	// 无字节上限时不返回读取器上限。
	if err := a.repos.UpdateConfig("raw-stream", `{"quotaAssets":1}`); err != nil {
		t.Fatalf("更新配置：%v", err)
	}
	if limit, err := a.quota.Admission("raw-stream", "app.jar", -1); err != nil || limit != 0 {
		t.Fatalf("未配置字节上限时不应返回读取器上限，得 limit=%d err=%v", limit, err)
	}

	// 用满后新增路径无余量：必须以错误早拒，不能返回 0（0 在读取器语义里表示"不限"）。
	if err := a.repos.UpdateConfig("raw-stream", `{"quotaBytes":60}`); err != nil {
		t.Fatalf("更新配置：%v", err)
	}
	if _, err := a.quota.Admission("raw-stream", "new.bin", -1); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("无余量新增应直接拒绝，得 %v", err)
	}
	// 路径未解析：只能给出不会误拒合法覆盖写的安全上限（整仓上限），不报错。
	limit, err = a.quota.Admission("raw-stream", "", -1)
	if err != nil || limit != 60 {
		t.Fatalf("路径未解析应返回安全上限 60 且不误拒，得 limit=%d err=%v", limit, err)
	}
	count, size := a.usage(t, repo.ID)
	if count != 1 || size != 60 {
		t.Fatalf("预检不得改动仓库占用，得 count=%d size=%d", count, size)
	}
}

// TestRepoQuotaCommitPointRejectsCommitBeyondQuota 提交点权威复检：
// 即使预检被绕过（这里直接调用域写入），越限写入也必须整批回滚、不留孤立件。
func TestRepoQuotaCommitPointRejectsCommitBeyondQuota(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-commit", "raw", domain.RepoQuotaLimits{Bytes: 100, Assets: 3})

	_, err := a.assetSvc.Put("raw-commit", "oversize.bin", bytes.NewReader(bytes.Repeat([]byte("x"), 150)), "application/octet-stream")
	if !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("提交点必须拒绝越限写入，得 %v", err)
	}
	count, size := a.usage(t, repo.ID)
	if count != 0 || size != 0 {
		t.Fatalf("被提交点拒绝的写入不得留下资产行，得 count=%d size=%d", count, size)
	}
	// 拒绝后已落盘的暂存 blob 交由既有失败路径回收，不产生新的孤立件：
	// 磁盘上不得残留任何 150 字节的 blob 文件（活动目录与暂存目录都不得有）。
	if leftovers := countBlobFilesOfSize(t, a.blobRoot, 150); leftovers != 0 {
		t.Fatalf("提交点拒绝后不得残留暂存 blob，得 %d 个", leftovers)
	}
	if orphans, err := a.assetSvc.CleanupUnreferencedBlobs(); err != nil || orphans != 0 {
		t.Fatalf("回收孤立 blob：removed=%d err=%v", orphans, err)
	}
	if a.spy.count() != 1 {
		t.Fatalf("提交点拒绝应累加一次拒绝计数，得 %d", a.spy.count())
	}
}

// TestRepoQuotaCommitPointOverwriteNetIncrementKeepsExistingContent 覆盖写的提交点复检：
// 净增量在限内放行；越限回滚且**既有内容未被破坏**。
func TestRepoQuotaCommitPointOverwriteNetIncrementKeepsExistingContent(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-commit-overwrite", "raw", domain.RepoQuotaLimits{Bytes: 100})
	a.seedAsset(t, "raw-commit-overwrite", "app.jar", bytes.Repeat([]byte("a"), 60))

	if _, err := a.assetSvc.Put("raw-commit-overwrite", "app.jar", bytes.NewReader(bytes.Repeat([]byte("b"), 95)), "application/octet-stream"); err != nil {
		t.Fatalf("覆盖写净增量在限内应放行，得 %v", err)
	}
	count, size := a.usage(t, repo.ID)
	if count != 1 || size != 95 {
		t.Fatalf("覆盖写后占用应为 1 件 / 95 字节，得 count=%d size=%d", count, size)
	}
	if _, err := a.assetSvc.Put("raw-commit-overwrite", "app.jar", bytes.NewReader(bytes.Repeat([]byte("c"), 120)), "application/octet-stream"); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("越限覆盖写必须拒绝，得 %v", err)
	}
	count, size = a.usage(t, repo.ID)
	if count != 1 || size != 95 {
		t.Fatalf("越限覆盖写必须回滚，既有内容不得被破坏，得 count=%d size=%d", count, size)
	}
	asset, rc, err := a.assetSvc.Get("raw-commit-overwrite", "app.jar")
	if err != nil {
		t.Fatalf("读取既有制品：%v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取既有制品内容：%v", err)
	}
	if int64(len(body)) != asset.Size {
		t.Fatalf("既有制品大小应保持 %d，得 %d", asset.Size, len(body))
	}
	if !bytes.Equal(body, bytes.Repeat([]byte("b"), 95)) {
		t.Fatal("越限覆盖写后既有内容必须保持不变")
	}
}

// TestRepoQuotaConcurrentUploadsOnlyOneCommits 并发语义（无预留案）：
// 预检可能同时通过，但提交点由单写者串行化，最多一个在途上传被拒，已提交状态永不越界。
func TestRepoQuotaConcurrentUploadsOnlyOneCommits(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-concurrent", "raw", domain.RepoQuotaLimits{Bytes: 100})

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := "a.bin"
			if idx == 1 {
				path = "b.bin"
			}
			_, results[idx] = a.assetSvc.Put("raw-concurrent", path, bytes.NewReader(bytes.Repeat([]byte("x"), 60)), "application/octet-stream")
		}(i)
	}
	wg.Wait()

	success := 0
	rejected := 0
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, domain.ErrQuotaExceeded):
			rejected++
		default:
			t.Fatalf("并发写入出现非配额错误：%v", err)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("并发两个 60 字节写入在 100 字节配额下应恰好一过一拒，得 success=%d rejected=%d", success, rejected)
	}
	count, size := a.usage(t, repo.ID)
	if count != 1 || size != 60 {
		t.Fatalf("已提交状态永不越界，得 count=%d size=%d", count, size)
	}
	if a.spy.count() != 1 {
		t.Fatalf("被拒的那一次应累加一次拒绝计数，得 %d", a.spy.count())
	}
}

// TestRepoQuotaExemptsMigrationImport 豁免：迁移导入（PutWithTimestamps）显式豁免配额，
// 但正常发布路径仍被拦，且豁免发生时会记日志。
func TestRepoQuotaExemptsMigrationImport(t *testing.T) {
	a := newQuotaAssembly(t)
	repo := a.createHostedRepo(t, "raw-migration", "raw", domain.RepoQuotaLimits{Bytes: 10, Assets: 1})

	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	if _, err := a.assetSvc.PutWithTimestamps("raw-migration", "migrated.bin", bytes.NewReader(bytes.Repeat([]byte("m"), 50)), "application/octet-stream", time.Time{}); err != nil {
		t.Fatalf("迁移导入（零源时间）应豁免配额，得 %v", err)
	}
	if _, err := a.assetSvc.PutWithTimestamps("raw-migration", "migrated2.bin", bytes.NewReader(bytes.Repeat([]byte("m"), 50)), "application/octet-stream", time.Date(2024, 5, 1, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatalf("迁移导入（带源时间）应豁免配额，得 %v", err)
	}
	count, size := a.usage(t, repo.ID)
	if count != 2 || size != 100 {
		t.Fatalf("豁免写入应真实落库，得 count=%d size=%d", count, size)
	}
	if a.spy.count() != 0 {
		t.Fatalf("豁免写入不应累加拒绝计数，得 %d", a.spy.count())
	}
	logged := buf.String()
	if !strings.Contains(logged, "仓库存储配额豁免") {
		t.Fatalf("豁免必须记日志，得 %q", logged)
	}
	if strings.Contains(logged, string(filepath.Separator)) {
		t.Fatalf("豁免日志不得回显文件系统路径，得 %q", logged)
	}

	// 同一仓库上的正常协议发布仍必须被配额拦住（豁免不能变成全局旁路）。
	if _, err := a.assetSvc.Put("raw-migration", "normal.bin", bytes.NewReader(bytes.Repeat([]byte("n"), 50)), "application/octet-stream"); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("正常运行路径不得被豁免，得 %v", err)
	}
	if a.spy.count() != 1 {
		t.Fatalf("正常路径越限应累加拒绝计数，得 %d", a.spy.count())
	}
}

// TestRepoQuotaIgnoresUnlimitedAndMissingRepositories 缺省不限与不存在/非 hosted 仓库不判定。
func TestRepoQuotaIgnoresUnlimitedAndMissingRepositories(t *testing.T) {
	a := newQuotaAssembly(t)
	a.createHostedRepo(t, "raw-unlimited", "raw", domain.RepoQuotaLimits{})
	id, err := a.repos.Create("grp", "raw", "group", "public", `{"members":["raw-unlimited"]}`)
	if err != nil {
		t.Fatalf("创建 group 仓库：%v", err)
	}
	if _, err := a.repos.GetByID(id); err != nil {
		t.Fatalf("读取 group 仓库：%v", err)
	}

	for _, name := range []string{"raw-unlimited", "grp", "not-exists"} {
		limit, err := a.quota.Admission(name, "any.bin", 1<<40)
		if err != nil || limit != 0 {
			t.Fatalf("仓库 %s 不应被配额判定，得 limit=%d err=%v", name, limit, err)
		}
	}
	if a.spy.count() != 0 {
		t.Fatalf("未配置配额时不应累加拒绝计数，得 %d", a.spy.count())
	}
	// 未配置配额的仓库其提交点也不判定：超大写入照样通过提交点。
	if _, err := a.assetSvc.Put("raw-unlimited", "huge.bin", bytes.NewReader(bytes.Repeat([]byte("h"), 4096)), "application/octet-stream"); err != nil {
		t.Fatalf("未配置配额的仓库不应被拦，得 %v", err)
	}
}

// newQuotaRepositoryService 组装仓库服务（配额配置校验用）。
func newQuotaRepositoryService(t *testing.T) *domain.RepositoryService {
	t.Helper()
	db := newTestDB(t)
	return domain.NewRepositoryService(
		repository.NewRepoRepo(db),
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
}

// TestValidateConfigStorageQuotaRules 配额配置校验：缺省不限；负数非法；非 hosted（group/proxy）不允许非 0。
func TestValidateConfigStorageQuotaRules(t *testing.T) {
	svc := newQuotaRepositoryService(t)

	// 缺省（0）表示不限：三种类型都接受。
	if _, err := svc.Create("quota-default", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("缺省配额应被接受（0 = 不限）：%v", err)
	}
	if _, err := svc.Create("quota-proxy", "raw", "proxy", "private", "", repository.RepositoryConfig{RemoteURL: "https://example.com/repo"}); err != nil {
		t.Fatalf("proxy 缺省配额应被接受（0 = 不限）：%v", err)
	}
	// 显式正数配额：只有承载写入的 hosted 接受。
	if _, err := svc.Create("quota-ok", "raw", "hosted", "private", "", repository.RepositoryConfig{QuotaBytes: 1024, QuotaAssets: 8}); err != nil {
		t.Fatalf("hosted 正数配额应被接受：%v", err)
	}

	// 负数非法。
	for _, cfg := range []repository.RepositoryConfig{{QuotaBytes: -1}, {QuotaAssets: -1}} {
		if _, err := svc.Create("quota-negative", "raw", "hosted", "private", "", cfg); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("负数配额应返回 ErrValidation，得 %v（cfg=%+v）", err, cfg)
		}
	}

	// proxy 的缓存写入发生在回源路径上，配额强制尚未覆盖该路径：与 group 一样在配置层拒绝非 0 值
	// （避免"设了但永不生效"）。
	for _, cfg := range []repository.RepositoryConfig{
		{RemoteURL: "https://example.com/repo", QuotaBytes: 1},
		{RemoteURL: "https://example.com/repo", QuotaAssets: 1},
	} {
		if _, err := svc.Create("quota-proxy-q", "raw", "proxy", "private", "", cfg); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("proxy 非 0 配额应返回 ErrValidation，得 %v（cfg=%+v）", err, cfg)
		}
	}
	// 更新路径同样拒绝：proxy 仓库改成非 0 配额也必须报错。
	proxyQuota := repository.RepositoryConfig{RemoteURL: "https://example.com/repo", QuotaBytes: 1}
	if _, err := svc.Update("quota-proxy", "", nil, &proxyQuota, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("proxy 更新非 0 配额应返回 ErrValidation，得 %v", err)
	}

	// group 不承载写入：两个上限都必须为 0。
	if _, err := svc.Create("quota-grp", "raw", "group", "private", "", repository.RepositoryConfig{Members: []string{"quota-ok"}, QuotaBytes: 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("group 非 0 字节配额应返回 ErrValidation，得 %v", err)
	}
	if _, err := svc.Create("quota-grp", "raw", "group", "private", "", repository.RepositoryConfig{Members: []string{"quota-ok"}, QuotaAssets: 1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("group 非 0 件数配额应返回 ErrValidation，得 %v", err)
	}
	if _, err := svc.Create("quota-grp", "raw", "group", "private", "", repository.RepositoryConfig{Members: []string{"quota-ok"}}); err != nil {
		t.Fatalf("group 零值配额应被接受：%v", err)
	}

	// 更新路径同样受校验约束。
	negative := repository.RepositoryConfig{QuotaBytes: -5}
	if _, err := svc.Update("quota-ok", "", nil, &negative, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("更新负数配额应返回 ErrValidation，得 %v", err)
	}
	valid := repository.RepositoryConfig{QuotaBytes: 2048, QuotaAssets: 16}
	updated, err := svc.Update("quota-ok", "", nil, &valid, nil)
	if err != nil {
		t.Fatalf("更新合法配额：%v", err)
	}
	cfg, err := updated.DecodeConfig()
	if err != nil {
		t.Fatalf("解析更新后的配置：%v", err)
	}
	if cfg.QuotaBytes != 2048 || cfg.QuotaAssets != 16 {
		t.Fatalf("配额应持久化并回显，得 %+v", cfg)
	}
}
