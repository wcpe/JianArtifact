package domain_test

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newAnonTestSvc 装配带匿名判定依赖的 RepositoryService 及配套服务。
func newAnonTestSvc(t *testing.T) (*domain.RepositoryService, *domain.SettingService, *repository.UserRepo, *repository.AclRepo) {
	t.Helper()
	db := newTestDB(t)
	userRepo := repository.NewUserRepo(db)
	aclRepo := repository.NewAclRepo(db)
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), aclRepo, repository.NewAssetRepo(db), settingSvc, userRepo)
	return svc, settingSvc, userRepo, aclRepo
}

// TestAnonymousAclRead anonymous 主体经 ACL 授 read 后，匿名（subjectID=0）可读 private 仓库；
// 未授权的 private 仓库仍拒绝；public 仓库始终可读（FR-66）。
func TestAnonymousAclRead(t *testing.T) {
	svc, _, userRepo, aclRepo := newAnonTestSvc(t)

	granted, err := svc.Create("anon-granted", "raw", "hosted", "private", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Create("anon-denied", "raw", "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	if _, err := svc.Create("anon-public", "raw", "hosted", "public", "", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}

	anon, err := userRepo.GetByUsername(domain.AnonymousUsername)
	if err != nil {
		t.Fatalf("迁移应内置 anonymous 用户：%v", err)
	}
	if err := aclRepo.Replace(granted.ID, []repository.Acl{{SubjectID: anon.ID, Action: "read"}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}

	cases := []struct {
		repo   string
		action string
		want   bool
	}{
		{"anon-granted", "read", true},
		{"anon-granted", "write", false},
		{"anon-denied", "read", false},
		{"anon-public", "read", true},
	}
	for _, c := range cases {
		got, err := svc.CanAccess(c.repo, 0, c.action)
		if err != nil {
			t.Fatalf("CanAccess(%s, 0, %s)：%v", c.repo, c.action, err)
		}
		if got != c.want {
			t.Errorf("CanAccess(%s, 0, %s) = %v，期望 %v", c.repo, c.action, got, c.want)
		}
	}
}

// TestAnonymousGlobalSwitch 全局开关关闭后一切匿名访问拒绝（public 也不例外）、
// 已认证主体不受影响；重新开启后恢复（FR-66）。
func TestAnonymousGlobalSwitch(t *testing.T) {
	svc, settings, userRepo, aclRepo := newAnonTestSvc(t)

	repo, err := svc.Create("switch-pub", "raw", "hosted", "public", "", repository.RepositoryConfig{})
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	// 开关默认开启：public 匿名可读。
	if ok, _ := svc.CanAccess("switch-pub", 0, "read"); !ok {
		t.Fatal("默认开关开启时 public 应匿名可读")
	}

	if err := settings.SetAnonymousAccessEnabled(false); err != nil {
		t.Fatalf("关闭开关：%v", err)
	}
	if enabled, _ := settings.AnonymousAccessEnabled(); enabled {
		t.Fatal("开关应已关闭")
	}
	if ok, _ := svc.CanAccess("switch-pub", 0, "read"); ok {
		t.Error("开关关闭后 public 匿名读应拒绝")
	}
	// 已认证主体不受开关影响：ACL 授权用户仍可读。
	uid, err := userRepo.Create("carol", "hash-placeholder", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	if err := aclRepo.Replace(repo.ID, []repository.Acl{{SubjectID: uid, Action: "read"}}); err != nil {
		t.Fatalf("写 ACL：%v", err)
	}
	if ok, _ := svc.CanAccess("switch-pub", uid, "read"); !ok {
		t.Error("开关关闭不应影响已认证主体")
	}
	// 匿名搜索兜底：开关关闭返回空集。
	if out, err := svc.SearchAssets("any", "", 0, false, "", "asc", 10, 0); err != nil || out.Total != 0 || len(out.Items) != 0 {
		t.Errorf("开关关闭时匿名搜索应空集，得 out=%+v err=%v", out, err)
	}

	if err := settings.SetAnonymousAccessEnabled(true); err != nil {
		t.Fatalf("重新开启开关：%v", err)
	}
	if ok, _ := svc.CanAccess("switch-pub", 0, "read"); !ok {
		t.Error("开关重新开启后 public 应恢复匿名可读")
	}
}

// TestAnonymousUserProtection 内置 anonymous 用户禁登录、禁删除 / 改密 / 改角色（FR-66），
// 且不影响自举判断（Bootstrap 不把 anonymous 计为已初始化）。
func TestAnonymousUserProtection(t *testing.T) {
	db := newTestDB(t)
	userRepo := repository.NewUserRepo(db)
	userSvc := domain.NewUserService(userRepo)

	anon, err := userRepo.GetByUsername(domain.AnonymousUsername)
	if err != nil {
		t.Fatalf("迁移应内置 anonymous 用户：%v", err)
	}
	if _, err := userSvc.Update(anon.ID, "admin", ""); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("改 anonymous 角色应 ErrValidation，得 %v", err)
	}
	if _, err := userSvc.Update(anon.ID, "", "disabled"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("停用 anonymous 应 ErrValidation，得 %v", err)
	}
	if err := userSvc.ChangePassword(anon.ID, "new-secret-123"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("改 anonymous 口令应 ErrValidation，得 %v", err)
	}
	if err := userSvc.Delete(anon.ID); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("删除 anonymous 应 ErrValidation，得 %v", err)
	}

	// 自举：仅有内置 anonymous 时仍视为未初始化。
	if n, err := userSvc.Count(); err != nil || n != 0 {
		t.Errorf("排除 anonymous 后用户数应 0，得 %d err=%v", n, err)
	}
}
