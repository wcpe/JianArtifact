package domain_test

import (
	"encoding/json"
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
func TestRepoOnlineNotReplicated(t *testing.T) {
	replSvc, _, _, repoRepo, _, _, _ := newTestReplSvc(t)

	// 本地建仓库并置 offline。
	if _, err := repoRepo.Create("raw-proxy", "raw", "proxy", "private", "{}"); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if err := repoRepo.SetOnline("raw-proxy", false); err != nil {
		t.Fatalf("SetOnline(false)：%v", err)
	}
	before, err := repoRepo.GetByName("raw-proxy")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	if before.Online {
		t.Fatalf("前置：应 offline，得 %v", before.Online)
	}

	// 对端应用一条仓库 put 变更（RepoChangeData 无 online 字段）。
	raw, err := json.Marshal(domain.RepoChangeData{
		Name: "raw-proxy", Format: "raw", Type: "proxy",
		Visibility: "private", Description: "来自对端", Config: "{}",
	})
	if err != nil {
		t.Fatalf("编码变更数据：%v", err)
	}
	change := repository.Change{
		NodeID: "peer", EntityType: domain.EntityRepository,
		EntityKey: domain.RepoKey("raw-proxy"), Op: domain.OpPut, Data: string(raw),
	}
	if err := replSvc.Apply(change); err != nil {
		t.Fatalf("Apply：%v", err)
	}

	// 本地 online 保持不变（不被对端覆盖）。
	after, err := repoRepo.GetByName("raw-proxy")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	if after.Online {
		t.Errorf("复制应用后本地 online 应保持 offline=false，得 %v", after.Online)
	}
	// 描述应已应用（证明复制确实生效，排除测试误判）。
	if after.Description != "来自对端" {
		t.Errorf("复制应用后 description 应生效，得 %q", after.Description)
	}
}
