package domain_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newAliasService 组装一个仅含仓库服务所需依赖的实例（别名相关用例共用）。
func newAliasService(t *testing.T) *domain.RepositoryService {
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

// TestRepositoryCreateWithAliases 创建时携带别名：按别名可解析到主名仓库，且别名随列表带出。
func TestRepositoryCreateWithAliases(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	created, err := svc.Create("maven-main", "maven", "hosted", "private", "", repository.RepositoryConfig{}, "maven-legacy", "maven-old")
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if !slices.Equal(created.Aliases, []string{"maven-legacy", "maven-old"}) {
		t.Fatalf("创建响应应带出别名，得 %v", created.Aliases)
	}

	byAlias, err := svc.Get("maven-legacy")
	if err != nil {
		t.Fatalf("按别名取仓库：%v", err)
	}
	if byAlias.Name != "maven-main" {
		t.Fatalf("别名应解析到主名仓库，得 %q", byAlias.Name)
	}
	if !slices.Equal(byAlias.Aliases, []string{"maven-legacy", "maven-old"}) {
		t.Errorf("按别名取仓库也应带出完整别名列表，得 %v", byAlias.Aliases)
	}

	items, _, _, err := svc.ListWithStats(10, 0)
	if err != nil {
		t.Fatalf("ListWithStats：%v", err)
	}
	if len(items) != 1 || !slices.Equal(items[0].Aliases, []string{"maven-legacy", "maven-old"}) {
		t.Fatalf("列表项应带出别名，得 %+v", items)
	}
}

// TestRepositoryCreateAliasValidation 创建时别名校验：空、等于自身主名、撞主名/别名。
func TestRepositoryCreateAliasValidation(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	if _, err := svc.Create("a", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库 a：%v", err)
	}
	if _, err := svc.Create("b", "raw", "hosted", "private", "", repository.RepositoryConfig{}, "taken-alias"); err != nil {
		t.Fatalf("建仓库 b：%v", err)
	}

	cases := []struct {
		name    string
		newName string
		aliases []string
		want    error
	}{
		{"空别名", "empty-alias", []string{"   "}, domain.ErrValidation},
		{"等于自身主名", "self-name", []string{"self-name"}, domain.ErrValidation},
		{"撞既有主名", "hit-main", []string{"a"}, domain.ErrConflict},
		{"撞既有别名", "hit-alias", []string{"taken-alias"}, domain.ErrConflict},
	}
	for _, tc := range cases {
		_, err := svc.Create(tc.newName, "raw", "hosted", "private", "", repository.RepositoryConfig{}, tc.aliases...)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s：期望 %v，得 %v", tc.name, tc.want, err)
		}
	}
}

// TestRepositoryCreateAliasDedup 重复别名去重后仅保留一份。
func TestRepositoryCreateAliasDedup(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	created, err := svc.Create("dedup", "raw", "hosted", "private", "", repository.RepositoryConfig{}, "x", "x", " y ")
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if !slices.Equal(created.Aliases, []string{"x", "y"}) {
		t.Fatalf("别名应去重并去空白，得 %v", created.Aliases)
	}
}

// TestRepositoryCanAccessResolvedMatchesCanAccess 按对象鉴权与按名鉴权判定一致：
// 列表可见性过滤逐行调用，必须用按对象版本（省掉逐行按名查库），两者判定不能分叉。
func TestRepositoryCanAccessResolvedMatchesCanAccess(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	for _, spec := range []struct{ name, visibility string }{
		{"pub", "public"},
		{"priv", "private"},
	} {
		if _, err := svc.Create(spec.name, "raw", "hosted", spec.visibility, "", repository.RepositoryConfig{}); err != nil {
			t.Fatalf("建仓库 %s：%v", spec.name, err)
		}
	}
	pub, err := svc.Get("pub")
	if err != nil {
		t.Fatalf("取 pub：%v", err)
	}
	priv, err := svc.Get("priv")
	if err != nil {
		t.Fatalf("取 priv：%v", err)
	}
	for _, tc := range []struct {
		label      string
		repo       *repository.Repository
		subject    repository.Subject
		wantAccess bool
	}{
		{"public 仓库对匿名可读", pub, repository.AnonymousSubject(), true},
		{"private 仓库对匿名不可读", priv, repository.AnonymousSubject(), false},
		{"private 仓库对无授权用户不可读", priv, repository.UserSubject(999), false},
	} {
		byName, errName := svc.CanAccess(tc.repo.Name, tc.subject, repository.ActionRead)
		byObj, errObj := svc.CanAccessResolved(tc.repo, tc.subject, repository.ActionRead)
		if (errName == nil) != (errObj == nil) || byName != byObj {
			t.Errorf("%s：按名(%v, %v) 与按对象(%v, %v) 判定分叉", tc.label, byName, errName, byObj, errObj)
		}
		if byObj != tc.wantAccess {
			t.Errorf("%s：期望 %v，得 %v", tc.label, tc.wantAccess, byObj)
		}
	}
}

// TestRepositoryCreateAliasRaceLeavesNoOrphan 并发创建共用同一别名的仓库：
// 撞名失败的一方不得留下「调用方以为没创建、实际已存在」的仓库行（别名与主名跨表唯一性
// 无 DB 兜底，只有服务层先查后写，并发窗口内会真的插进重复别名）。
func TestRepositoryCreateAliasRaceLeavesNoOrphan(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	const rounds = 6
	const workers = 24
	for r := 0; r < rounds; r++ {
		prefix := fmt.Sprintf("race-%d", r)
		alias := fmt.Sprintf("shared-%d", r)
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = svc.Create(fmt.Sprintf("%s-%d", prefix, i), "raw", "hosted", "private", "", repository.RepositoryConfig{}, alias)
			}(i)
		}
		wg.Wait()

		wins := 0
		for i, err := range errs {
			if err == nil {
				wins++
				continue
			}
			if !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("第 %d 个并发请求应 ErrConflict，得 %v", i, err)
			}
			if _, gerr := svc.Get(fmt.Sprintf("%s-%d", prefix, i)); gerr == nil {
				t.Fatalf("并发撞名的失败请求留下了仓库行 %s-%d", prefix, i)
			}
		}
		if wins != 1 {
			t.Fatalf("每轮应恰好 1 个成功，得 %d", wins)
		}
	}
}

// TestRepositoryUpdateAliases Update 覆盖式更新别名：保留自身既有别名、可清空、拒绝冲突。
func TestRepositoryUpdateAliases(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	if _, err := svc.Create("main", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库 main：%v", err)
	}
	if _, err := svc.Create("other", "raw", "hosted", "private", "", repository.RepositoryConfig{}, "other-alias"); err != nil {
		t.Fatalf("建仓库 other：%v", err)
	}

	// 首次设置别名。
	updated, err := svc.Update("main", "", nil, nil, &[]string{"keep", "drop"})
	if err != nil {
		t.Fatalf("Update 设置别名：%v", err)
	}
	if !slices.Equal(updated.Aliases, []string{"drop", "keep"}) {
		t.Fatalf("Update 未写入别名，得 %v", updated.Aliases)
	}

	// 覆盖时保留自身既有别名「keep」，移除「drop」。
	updated, err = svc.Update("main", "", nil, nil, &[]string{"keep"})
	if err != nil {
		t.Fatalf("Update 覆盖别名：%v", err)
	}
	if !slices.Equal(updated.Aliases, []string{"keep"}) {
		t.Fatalf("覆盖后别名应为 [keep]，得 %v", updated.Aliases)
	}

	// 引用其它仓库的别名 → 冲突。
	if _, err := svc.Update("main", "", nil, nil, &[]string{"other-alias"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("撞它仓别名应 ErrConflict，得 %v", err)
	}
	// 等于自身主名 → 校验失败。
	if _, err := svc.Update("main", "", nil, nil, &[]string{"main"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("别名等于主名应 ErrValidation，得 %v", err)
	}

	// 传空数组表示清空。
	updated, err = svc.Update("main", "", nil, nil, &[]string{})
	if err != nil {
		t.Fatalf("Update 清空别名：%v", err)
	}
	if len(updated.Aliases) != 0 {
		t.Fatalf("空数组应清空别名，得 %v", updated.Aliases)
	}
	if _, err := svc.Get("keep"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("清空后旧别名应不可解析，得 %v", err)
	}
}

// TestRepositoryRename 重命名成功：新名为主名、旧名转别名仍可解析。
func TestRepositoryRename(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	if _, err := svc.Create("raw-old", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	renamed, err := svc.Rename("raw-old", "raw-new")
	if err != nil {
		t.Fatalf("Rename：%v", err)
	}
	if renamed.Name != "raw-new" {
		t.Fatalf("重命名后主名应为 raw-new，得 %q", renamed.Name)
	}
	if !slices.Contains(renamed.Aliases, "raw-old") {
		t.Fatalf("旧名应自动成为别名，得 %v", renamed.Aliases)
	}
	byOld, err := svc.Get("raw-old")
	if err != nil {
		t.Fatalf("按旧名取仓库：%v", err)
	}
	if byOld.Name != "raw-new" {
		t.Fatalf("旧名应解析到 raw-new，得 %q", byOld.Name)
	}

	// 亦可经旧名（现为别名）再次改名。
	again, err := svc.Rename("raw-old", "raw-third")
	if err != nil {
		t.Fatalf("经别名再次重命名：%v", err)
	}
	if again.Name != "raw-third" {
		t.Fatalf("应重命名为 raw-third，得 %q", again.Name)
	}
	if !slices.Contains(again.Aliases, "raw-new") || !slices.Contains(again.Aliases, "raw-old") {
		t.Fatalf("两次改名的旧名都应保留为别名，得 %v", again.Aliases)
	}
}

// TestRepositoryRenameValidation 重命名非法输入：空、同名、撞名、不存在。
func TestRepositoryRenameValidation(t *testing.T) {
	t.Parallel()
	svc := newAliasService(t)
	if _, err := svc.Create("a", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库 a：%v", err)
	}
	if _, err := svc.Create("b", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库 b：%v", err)
	}

	if _, err := svc.Rename("a", "   "); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("空新名应 ErrValidation，得 %v", err)
	}
	if _, err := svc.Rename("a", "a"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("同名应 ErrValidation，得 %v", err)
	}
	if _, err := svc.Rename("a", "b"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("撞既有主名应 ErrConflict，得 %v", err)
	}
	if _, err := svc.Rename("ghost", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("不存在仓库应 ErrNotFound，得 %v", err)
	}
}
