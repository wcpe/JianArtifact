package domain_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestPublishPolicyStreamingReservationEnforcesPathAndQuota(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw-publish", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	policies := repository.NewPublishPolicyRepo(db)
	svc := domain.NewPublishPolicyService(policies, repos, repository.NewAssetRepo(db))
	if _, err := svc.Save(repository.PublishPolicy{
		UserID: uid, PathPrefixes: []string{"releases"}, MaxBytesDay: 10, MaxFileBytes: 4,
	}, "raw-publish"); err != nil {
		t.Fatalf("保存发布策略：%v", err)
	}
	if _, _, err := svc.BeginStreaming(uid, "raw-publish", "snapshots/a.jar"); !errors.Is(err, domain.ErrPublishPathDenied) {
		t.Fatalf("越界路径应拒绝，得 %v", err)
	}
	id, limit, err := svc.BeginStreaming(uid, "raw-publish", "releases/a.jar")
	if err != nil || id == 0 || limit != 4 {
		t.Fatalf("流式预留异常：id=%d limit=%d err=%v", id, limit, err)
	}
	if err := svc.SettleSize(id, true, 3); err != nil {
		t.Fatalf("流式预留结算失败：%v", err)
	}
	if _, limit, err := svc.BeginStreaming(uid, "raw-publish", "releases/b.jar"); err != nil || limit != 4 {
		t.Fatalf("结算后仍应按文件上限允许新上传：limit=%d err=%v", limit, err)
	}
}

func TestPublishPolicyUnresolvedStreamingReservesBeforePathParsing(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("unresolved-publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("format-publish", "pypi", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, repository.NewAssetRepo(db))
	if _, err := svc.Save(repository.PublishPolicy{UserID: uid, PathPrefixes: []string{"pypi/packages/release"}, MaxAssetsHour: 1, MaxFileBytes: 3}, "format-publish"); err != nil {
		t.Fatalf("保存发布策略：%v", err)
	}
	id, limit, err := svc.BeginUnresolvedStreaming(uid, "format-publish")
	if err != nil || id == 0 || limit != 3 {
		t.Fatalf("未解析路径的预留异常：id=%d limit=%d err=%v", id, limit, err)
	}
	if _, _, err := svc.BeginUnresolvedStreaming(uid, "format-publish"); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("并发上传必须在落盘前耗尽件数额度，实际：%v", err)
	}
	if err := svc.ValidateUnresolvedPublish(uid, "format-publish", "pypi/packages/blocked/a.whl"); !errors.Is(err, domain.ErrPublishPathDenied) {
		t.Fatalf("解析路径后必须执行前缀限制，实际：%v", err)
	}
	if err := svc.SettleSize(id, false, 0); err != nil {
		t.Fatalf("释放预留：%v", err)
	}
}

func TestPublishPolicyMultiplePrefixesAndNoPolicyAllowsAllPaths(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("prefix-publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw-prefixes", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, repository.NewAssetRepo(db))

	if _, err := svc.Begin(uid, "raw-prefixes", "unrestricted/a.jar", 1); err != nil {
		t.Fatalf("无策略账号应允许仓库内全部路径：%v", err)
	}
	if _, err := svc.Save(repository.PublishPolicy{UserID: uid, PathPrefixes: []string{"releases", "snapshots"}}, "raw-prefixes"); err != nil {
		t.Fatalf("保存多前缀策略：%v", err)
	}
	for _, path := range []string{"releases/a.jar", "snapshots/a.jar"} {
		if _, err := svc.Begin(uid, "raw-prefixes", path, 1); err != nil {
			t.Fatalf("允许前缀 %s 应可发布：%v", path, err)
		}
	}
	if _, err := svc.Begin(uid, "raw-prefixes", "other/a.jar", 1); !errors.Is(err, domain.ErrPublishPathDenied) {
		t.Fatalf("未配置的前缀应拒绝，得 %v", err)
	}
}

func TestPublishPolicyImmutableReleaseAlsoRejectsAdministratorOverwrite(t *testing.T) {
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	repoID, err := repos.Create("raw-immutable-admin", "raw", "hosted", "private", `{"immutableRelease":true}`)
	if err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	assets := repository.NewAssetRepo(db)
	if err := assets.Upsert(repoID, "release.jar", "existing", 1, "application/java-archive", "", ""); err != nil {
		t.Fatalf("写入既有 Release：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, assets)
	if _, err := svc.Begin(0, "raw-immutable-admin", "release.jar", 1); !errors.Is(err, domain.ErrImmutableRelease) {
		t.Fatalf("管理员覆盖不可变 Release 错误=%v，期望 ErrImmutableRelease", err)
	}
}

func TestPublishPolicyEnforcesAllQuotaLimits(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("quota-publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw-quotas", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, repository.NewAssetRepo(db))
	if _, err := svc.Save(repository.PublishPolicy{UserID: uid, MaxAssetsHour: 2, MaxBytesDay: 5, MaxFileBytes: 3}, "raw-quotas"); err != nil {
		t.Fatalf("保存额度策略：%v", err)
	}
	if _, err := svc.Begin(uid, "raw-quotas", "too-large.jar", 4); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("单文件上限应拒绝，得 %v", err)
	}
	first, err := svc.Begin(uid, "raw-quotas", "first.jar", 3)
	if err != nil || first == 0 {
		t.Fatalf("首次上传预留异常：id=%d err=%v", first, err)
	}
	if err := svc.Settle(first, true); err != nil {
		t.Fatalf("首次上传结算失败：%v", err)
	}
	second, err := svc.Begin(uid, "raw-quotas", "second.jar", 2)
	if err != nil || second == 0 {
		t.Fatalf("第二次上传预留异常：id=%d err=%v", second, err)
	}
	if err := svc.Settle(second, true); err != nil {
		t.Fatalf("第二次上传结算失败：%v", err)
	}
	if _, err := svc.Begin(uid, "raw-quotas", "third.jar", 1); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("件数和日字节额度耗尽后应拒绝，得 %v", err)
	}
}

func TestPublishPolicyConcurrentReservationsDoNotExceedQuota(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("concurrent-publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw-concurrent", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, repository.NewAssetRepo(db))
	if _, err := svc.Save(repository.PublishPolicy{UserID: uid, MaxAssetsHour: 1, MaxBytesDay: 1}, "raw-concurrent"); err != nil {
		t.Fatalf("保存额度策略：%v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes []int64
	var failures []error
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, err := svc.Begin(uid, "raw-concurrent", "artifact-"+string(rune('a'+i)), 1)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes = append(successes, id)
			} else {
				failures = append(failures, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(successes) != 1 {
		t.Fatalf("并发预留只能成功一次，成功=%v，失败=%v", successes, failures)
	}
	for _, err := range failures {
		if !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("并发超限应返回配额错误，得 %v", err)
		}
	}
}

func TestPublishPolicyFailedStreamingUploadReleasesReservation(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("failed-stream-publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw-failed-stream", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, repository.NewAssetRepo(db))
	if _, err := svc.Save(repository.PublishPolicy{UserID: uid, MaxAssetsHour: 1, MaxBytesDay: 2, MaxFileBytes: 2}, "raw-failed-stream"); err != nil {
		t.Fatalf("保存额度策略：%v", err)
	}
	id, _, err := svc.BeginStreaming(uid, "raw-failed-stream", "interrupted.jar")
	if err != nil || id == 0 {
		t.Fatalf("断连前预留异常：id=%d err=%v", id, err)
	}
	if err := svc.SettleSize(id, false, 0); err != nil {
		t.Fatalf("断连释放预留失败：%v", err)
	}
	if next, _, err := svc.BeginStreaming(uid, "raw-failed-stream", "retry.jar"); err != nil || next == 0 {
		t.Fatalf("失败后额度应立即可重用：id=%d err=%v", next, err)
	}
}

func TestPublishPolicyStreamingReservationCountsPendingBytes(t *testing.T) {
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	uid, err := users.Create("streaming-quota-publisher", "hash", "user")
	if err != nil {
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw-streaming-quota", "raw", "hosted", "private", "{}"); err != nil {
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	svc := domain.NewPublishPolicyService(repository.NewPublishPolicyRepo(db), repos, repository.NewAssetRepo(db))
	if _, err := svc.Save(repository.PublishPolicy{UserID: uid, MaxBytesDay: 4, MaxFileBytes: 4}, "raw-streaming-quota"); err != nil {
		t.Fatalf("保存额度策略：%v", err)
	}
	if id, limit, err := svc.BeginStreaming(uid, "raw-streaming-quota", "first.jar"); err != nil || id == 0 || limit != 4 {
		t.Fatalf("首个流式预留异常：id=%d limit=%d err=%v", id, limit, err)
	}
	if _, _, err := svc.BeginStreaming(uid, "raw-streaming-quota", "second.jar"); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("未结算流式预留必须计入日额度，得 %v", err)
	}
}

func TestPublishPolicyFailureAndRestartRecoveryReleaseReservationsImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "publish-policy-restart.db")
	db, err := persistence.Open(path)
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		t.Fatalf("迁移数据库：%v", err)
	}
	users := repository.NewUserRepo(db)
	uid, err := users.Create("restart-publisher", "hash", "user")
	if err != nil {
		_ = db.Close()
		t.Fatalf("创建发布用户：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	repoID, err := repos.Create("raw-restart", "raw", "hosted", "private", "{}")
	if err != nil {
		_ = db.Close()
		t.Fatalf("创建 hosted 仓库：%v", err)
	}
	policies := repository.NewPublishPolicyRepo(db)
	policy := repository.PublishPolicy{UserID: uid, RepositoryID: repoID, MaxAssetsHour: 1, MaxBytesDay: 2}
	if err := policies.Upsert(policy); err != nil {
		_ = db.Close()
		t.Fatalf("保存额度策略：%v", err)
	}
	reservationID, err := policies.Reserve(policy, 1, 2)
	if err != nil || reservationID == 0 {
		_ = db.Close()
		t.Fatalf("创建未结算预留异常：id=%d err=%v", reservationID, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭模拟崩溃前连接：%v", err)
	}

	reopened, err := persistence.Open(path)
	if err != nil {
		t.Fatalf("重启后打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(); err != nil {
		t.Fatalf("重启后迁移数据库：%v", err)
	}
	recovered := repository.NewPublishPolicyRepo(reopened)
	released, err := recovered.ReleaseExpired()
	if err != nil {
		t.Fatalf("启动恢复预留失败：%v", err)
	}
	if released != 1 {
		t.Fatalf("启动恢复必须立即释放未结算预留，释放数=%d", released)
	}
	if _, err := recovered.Reserve(policy, 1, 2); err != nil {
		t.Fatalf("恢复后额度应立即可用：%v", err)
	}
}
