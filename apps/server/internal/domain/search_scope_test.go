package domain

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newScopeTestRepoService 构造一个真实库上的 RepositoryService（仅用于可搜范围的判定）。
func newScopeTestRepoService(t *testing.T) *RepositoryService {
	t.Helper()
	db := migratedTestDB(t, "search-scope.db")
	return NewRepositoryService(
		repository.NewRepoRepo(db),
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
}

// TestReadableRepoIDsPropagatesEnumerateFailure 钉住「仓库全量列举失败必须上抛」。
//
// 旧实现的条件是 `aclSubject > 0 && allErr == nil`：列举失败时既不报错也不记日志，直接
// 跳过私有仓库补充、返回「仅公开仓库」。对已授权私有仓库的用户，表现就是搜索结果数量莫名
// 下降，而链路上没有任何信号（日志、状态、HTTP 状态码都不变）——静默降级比失败更难排查。
//
// 这里直接用一个必然失败的 allErr 驱动判定：allRepos 无法使用就不该假装「可搜范围算完了」。
func TestReadableRepoIDsPropagatesEnumerateFailure(t *testing.T) {
	t.Parallel()
	svc := newScopeTestRepoService(t)
	enumerateErr := errors.New("模拟仓库全量列举失败")

	// subjectID 非 0：跳过匿名分支，直接进入「主体被授 read 的私有仓库」判定。
	_, _, err := svc.readableRepoIDs(1, false, nil, enumerateErr)
	if !errors.Is(err, enumerateErr) {
		t.Fatalf("仓库全量列举失败应上抛（而非退回仅公开仓库），得 %v", err)
	}
}

// TestReadableRepoIDsWithoutPrivateRepos 是上一条的对照：列举成功时判定必须正常走完，
// 不让新加的上抛分支把所有请求都打成错误。
func TestReadableRepoIDsWithoutPrivateRepos(t *testing.T) {
	t.Parallel()
	svc := newScopeTestRepoService(t)

	// 空库：没有公开仓库、也没有私有仓库 → 可搜集合为空，readable=false 而不是报错。
	repoIDs, readable, err := svc.readableRepoIDs(1, false, nil, nil)
	if err != nil {
		t.Fatalf("列举成功时不应报错：%v", err)
	}
	if readable || repoIDs != nil {
		t.Fatalf("没有任何可读仓库时 readable 应为 false，得 readable=%v repoIDs=%v", readable, repoIDs)
	}

	// 管理员不受可读集合限制：nil 表示不限。
	if repoIDs, readable, err = svc.readableRepoIDs(1, true, nil, nil); err != nil || !readable || repoIDs != nil {
		t.Fatalf("管理员应为不限（nil/false），得 repoIDs=%v readable=%v err=%v", repoIDs, readable, err)
	}
}
