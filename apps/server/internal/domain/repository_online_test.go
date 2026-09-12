package domain_test

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRepositorySetOnlineNoReplChange SetOnline 是节点本地运维状态：
// 直接落库且不写复制变更日志（M-2 硬约束），对照业务写路径仍记录变更。
func TestRepositorySetOnlineNoReplChange(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	aclRepo := repository.NewAclRepo(db)
	userRepo := repository.NewUserRepo(db)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, assetRepo, domain.NewSettingService(repository.NewSettingRepo(db)), userRepo)
	rec := &recordingRecorder{}
	repoSvc.SetChangeRecorder(rec)

	if _, err := repoSvc.Create("raw-proxy", "raw", "proxy", "private", "", repository.RepositoryConfig{RemoteURL: "https://example.org/raw"}); err != nil {
		t.Fatalf("Create：%v", err)
	}
	// Create 已记录 1 条仓库变更。
	if len(rec.records) != 1 {
		t.Fatalf("Create 后应记录 1 条变更，得 %d", len(rec.records))
	}

	// SetOnline 不应新增任何复制变更日志。
	if err := repoSvc.SetOnline("raw-proxy", false); err != nil {
		t.Fatalf("SetOnline：%v", err)
	}
	if len(rec.records) != 1 {
		t.Fatalf("SetOnline 不应记录复制变更，得 %d 条：%+v", len(rec.records), rec.records)
	}

	// 持久化读回。
	repo, err := repoSvc.Get("raw-proxy")
	if err != nil {
		t.Fatalf("Get：%v", err)
	}
	if repo.Online {
		t.Errorf("SetOnline(false) 后应 online=false，得 %v", repo.Online)
	}
}

// TestRepoOnlineNotReplicated 守护测试（M-2 硬约束）：复制应用 RepoChangeData
// （不含 online 字段）不得覆盖本地 online 状态。
