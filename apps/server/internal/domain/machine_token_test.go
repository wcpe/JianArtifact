package domain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func newMachineTokenFixture(t *testing.T) (*persistence.DB, *domain.TokenService, *repository.UserRepo, *repository.TokenRepo, *domain.RepositoryService, *repository.AclRepo) {
	t.Helper()
	db := newTestDB(t)
	users := repository.NewUserRepo(db)
	tokens := repository.NewTokenRepo(db)
	aclRepo := repository.NewAclRepo(db)
	repoRepo := repository.NewRepoRepo(db)
	repoSvc := domain.NewRepositoryService(repoRepo, aclRepo, repository.NewAssetRepo(db), domain.NewSettingService(repository.NewSettingRepo(db)), users)
	svc := domain.NewTokenService(tokens, users)
	svc.SetRepositoryService(repoSvc)
	return db, svc, users, tokens, repoSvc, aclRepo
}

func adminPrincipal(t *testing.T, users *repository.UserRepo) *auth.Principal {
	t.Helper()
	uid, err := users.Create("admin", "h", "admin")
	if err != nil {
		t.Fatalf("建管理员：%v", err)
	}
	return &auth.Principal{UserID: uid, Role: "admin", AccountEpoch: 1}
}

func TestMachineTokenScopedIssuance(t *testing.T) {
	_, svc, users, _, repoSvc, aclRepo := newMachineTokenFixture(t)
	ctx := context.Background()
	admin := adminPrincipal(t, users)
	if _, err := repoSvc.Create("hosted-raw", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	plain, tok, err := svc.CreateScopedForPrincipal(ctx, admin, domain.CreateTokenInput{
		Name: "ci-publisher", Purpose: auth.TokenPurposeArtifactPublish, ExpiresDays: 30,
		Scopes: []domain.MachineTokenScopeInput{{Repository: "hosted-raw", PathPrefix: "com/acme", Actions: []string{"read", "publish"}}},
	})
	if err != nil {
		t.Fatalf("受限签发失败：%v", err)
	}
	if plain == "" || tok.Purpose != auth.TokenPurposeArtifactPublish || tok.ScopesJSON == "" || tok.ExpiresAt == nil {
		t.Fatalf("签发返回不完整：%+v", tok)
	}
	// 鉴权加载：摘要裁决后用途与作用域随主体返回。
	list, err := svc.List(admin.UserID)
	if err != nil || len(list) != 1 || list[0].Purpose != auth.TokenPurposeArtifactPublish || list[0].ExpiresAt == nil {
		t.Fatalf("列表应含用途与期限：%+v，%v", list, err)
	}
	scopes, err := auth.ParseMachineScopes(list[0].ScopesJSON)
	if err != nil || len(scopes) != 1 || scopes[0].PathPrefix != "com/acme" {
		t.Fatalf("作用域未持久化：%s，%v", list[0].ScopesJSON, err)
	}
	// 仓库按名称解析为稳定 ID。
	repo, err := repoSvc.Get("hosted-raw")
	if err != nil || scopes[0].RepositoryID != repo.ID {
		t.Fatalf("仓库应解析为稳定 ID：%+v", scopes[0])
	}
	// 普通用户未被授予该动作时不能签发。
	uid, err := users.Create("plain", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	user := &auth.Principal{UserID: uid, Role: "user", AccountEpoch: 1}
	if _, _, err = svc.CreateScopedForPrincipal(ctx, user, domain.CreateTokenInput{
		Name: "bad", Purpose: auth.TokenPurposeArtifactPublish,
		Scopes: []domain.MachineTokenScopeInput{{Repository: "hosted-raw", Actions: []string{"publish"}}},
	}); !errors.Is(err, domain.ErrCredentialScopeDenied) {
		t.Fatalf("无权签发应拒绝：%v", err)
	}
	if err := aclRepo.Replace(repo.ID, []repository.Acl{{SubjectID: uid, Action: repository.ActionPublish}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = svc.CreateScopedForPrincipal(ctx, user, domain.CreateTokenInput{
		Name: "ok", Purpose: auth.TokenPurposeArtifactPublish,
		Scopes: []domain.MachineTokenScopeInput{{Repository: "hosted-raw", Actions: []string{"publish"}}},
	}); err != nil {
		t.Fatalf("持权签发应成功：%v", err)
	}
}

func TestMachineTokenIssuanceRejectsBadInput(t *testing.T) {
	_, svc, users, _, repoSvc, _ := newMachineTokenFixture(t)
	ctx := context.Background()
	admin := adminPrincipal(t, users)
	if _, err := repoSvc.Create("r1", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	scoped := func(in domain.CreateTokenInput) error {
		_, _, err := svc.CreateScopedForPrincipal(ctx, admin, in)
		return err
	}
	deny := func(name string, in domain.CreateTokenInput) {
		t.Helper()
		if err := scoped(in); !errors.Is(err, domain.ErrCredentialScopeDenied) {
			t.Errorf("%s 应拒绝，得 %v", name, err)
		}
	}
	deny("未知用途", domain.CreateTokenInput{Name: "x", Purpose: "admin", Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", Actions: []string{"read"}}}})
	deny("旧凭据带作用域", domain.CreateTokenInput{Name: "x", Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", Actions: []string{"read"}}}})
	deny("空作用域", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead})
	deny("read 携带 publish", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", Actions: []string{"publish"}}}})
	deny("未知动作", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactPublish, Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", Actions: []string{"write"}}}})
	deny("期限超 90 天", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, ExpiresDays: 91, Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", Actions: []string{"read"}}}})
	deny("仓库不存在", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, Scopes: []domain.MachineTokenScopeInput{{Repository: "ghost", Actions: []string{"read"}}}})
	deny("遍历前缀", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", PathPrefix: "../etc", Actions: []string{"read"}}}})
	deny("编码前缀", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", PathPrefix: "a%2fb", Actions: []string{"read"}}}})
	deny("空动作集", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, Scopes: []domain.MachineTokenScopeInput{{Repository: "r1"}}})
	many := make([]domain.MachineTokenScopeInput, domain.MachineTokenScopeLimit+1)
	for i := range many {
		many[i] = domain.MachineTokenScopeInput{Repository: "r1", PathPrefix: "p" + string(rune('a'+i)), Actions: []string{"read"}}
	}
	deny("作用域超限", domain.CreateTokenInput{Name: "x", Purpose: auth.TokenPurposeArtifactRead, Scopes: many})
	// 受限机器 Token 不能再签发任何凭据；旧式 Token 也不能签发受限凭据。
	scopedPrincipal := &auth.Principal{UserID: admin.UserID, Kind: auth.KindToken, TokenID: 1, AccountEpoch: 1, TokenPurpose: auth.TokenPurposeArtifactRead}
	if _, _, err := svc.CreateScopedForPrincipal(ctx, scopedPrincipal, domain.CreateTokenInput{Name: "nested"}); !errors.Is(err, domain.ErrCredentialScopeDenied) {
		t.Fatalf("受限凭据发证应拒绝：%v", err)
	}
	legacyPrincipal := &auth.Principal{UserID: admin.UserID, Kind: auth.KindToken, TokenID: 1, AccountEpoch: 1}
	if _, _, err := svc.CreateScopedForPrincipal(ctx, legacyPrincipal, domain.CreateTokenInput{
		Name: "nested", Purpose: auth.TokenPurposeArtifactRead,
		Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", Actions: []string{"read"}}},
	}); !errors.Is(err, domain.ErrCredentialScopeDenied) {
		t.Fatalf("Token 签发受限凭据应拒绝：%v", err)
	}
}

func TestMachineTokenPrincipalCarriesScope(t *testing.T) {
	db, svc, users, tokens, repoSvc, _ := newMachineTokenFixture(t)
	admin := adminPrincipal(t, users)
	repoSvcCreate := repoSvc.Create
	if _, err := repoSvcCreate("r1", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatal(err)
	}
	plain, tok, err := svc.CreateScopedForPrincipal(context.Background(), admin, domain.CreateTokenInput{
		Name: "reader", Purpose: auth.TokenPurposeArtifactRead,
		Scopes: []domain.MachineTokenScopeInput{{Repository: "r1", PathPrefix: "a/b", Actions: []string{"read"}}},
	})
	if err != nil {
		t.Fatalf("签发：%v", err)
	}
	// 用摘要路径模拟协议鉴权：同一库上的 authStore。
	store := domain.NewAuthStore(users, tokens, repository.NewRevokedRepo(db))
	p, err := store.PrincipalByTokenDigest(auth.DigestToken(plain))
	if err != nil {
		t.Fatalf("按摘要取主体：%v", err)
	}
	if p.TokenPurpose != auth.TokenPurposeArtifactRead || p.TokenID != tok.ID {
		t.Fatalf("主体未携带用途：%+v", p)
	}
	repo, err := repoSvc.Get("r1")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.MachineScopeAllowed(p.TokenScopes, repo.ID, "a/b/c.jar", "read") {
		t.Fatalf("scope 未随主体生效：%+v", p.TokenScopes)
	}
	if auth.MachineScopeAllowed(p.TokenScopes, repo.ID, "a/b/c.jar", "publish") {
		t.Fatal("read 作用域不能发布")
	}
}
