package domain_test

import (
	"errors"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newExternalAuthSvc 装配外部身份登录所需的 AuthService 与用户仓库（真实 SQLite + 迁移 0044）。
func newExternalAuthSvc(t *testing.T) (*domain.AuthService, *repository.UserRepo) {
	t.Helper()
	db := migratedTestDB(t, "auth-external.db")
	users := repository.NewUserRepo(db)
	jwtMgr := auth.NewJWTManager([]byte("external-identity-test-secret-32b"))
	return domain.NewAuthService(users, repository.NewRevokedRepo(db), jwtMgr), users
}

func oidcIdentity(username, subject string) domain.ExternalIdentity {
	return domain.ExternalIdentity{Source: "oidc", Subject: subject, Username: username, Email: username + "@example.com"}
}

// TestLoginExternalCreatesAccountOnFirstLogin 首次外部登录即建号：角色固定 user、不设本地口令。
func TestLoginExternalCreatesAccountOnFirstLogin(t *testing.T) {
	t.Parallel()
	svc, users := newExternalAuthSvc(t)

	token, user, err := svc.LoginExternal(oidcIdentity("alice", "sub-alice"))
	if err != nil {
		t.Fatalf("首次外部登录：%v", err)
	}
	if token == "" || user == nil {
		t.Fatal("首次外部登录应签发会话")
	}
	if user.Role != "user" || user.AuthSource != "oidc" || user.ExternalSubject != "sub-alice" {
		t.Fatalf("建号口径不符：role=%q source=%q subject=%q", user.Role, user.AuthSource, user.ExternalSubject)
	}
	if user.PasswordHash != "" {
		t.Fatal("外部来源账号不得设置本地口令")
	}
	stored, err := users.GetByUsername("alice")
	if err != nil || stored.ID != user.ID {
		t.Fatalf("建号未落库：%v", err)
	}
}

// TestLoginExternalBindsExistingLocalUser 已存在的本地普通账号按用户名绑定，保留原 ID。
func TestLoginExternalBindsExistingLocalUser(t *testing.T) {
	t.Parallel()
	svc, users := newExternalAuthSvc(t)
	id, err := users.Create("bob", "$argon2id$local-hash", "user")
	if err != nil {
		t.Fatalf("预建本地用户：%v", err)
	}

	_, user, err := svc.LoginExternal(oidcIdentity("bob", "sub-bob"))
	if err != nil {
		t.Fatalf("绑定登录：%v", err)
	}
	if user.ID != id {
		t.Fatalf("应绑定既有账号（id=%d），实际 %d", id, user.ID)
	}
	if user.AuthSource != "oidc" || user.ExternalSubject != "sub-bob" {
		t.Fatalf("绑定后应记录来源与标识：%+v", user)
	}
}

// TestLoginExternalNeverBindsAdmin 管理员账号绝不参与自动绑定，避免用户名撞名即接管管理员。
func TestLoginExternalNeverBindsAdmin(t *testing.T) {
	t.Parallel()
	svc, users := newExternalAuthSvc(t)
	id, err := users.Create("root", "$argon2id$admin-hash", "admin")
	if err != nil {
		t.Fatalf("预建管理员：%v", err)
	}

	_, _, err = svc.LoginExternal(oidcIdentity("root", "sub-root"))
	if !errors.Is(err, domain.ErrExternalIdentityInvalid) {
		t.Fatalf("管理员账号不应被自动绑定，实际 %v", err)
	}
	admin, err := users.GetByID(id)
	if err != nil {
		t.Fatalf("读取管理员：%v", err)
	}
	if admin.ExternalSubject != "" {
		t.Fatalf("管理员账号不得被写入外部标识：%q", admin.ExternalSubject)
	}
}

// TestLoginExternalRejectsBuiltinAndInvalidIdentities 内置主体、空字段与来源不符一律拒绝。
func TestLoginExternalRejectsBuiltinAndInvalidIdentities(t *testing.T) {
	t.Parallel()
	svc, users := newExternalAuthSvc(t)

	if _, _, err := svc.LoginExternal(oidcIdentity(domain.AnonymousUsername, "sub-anon")); !errors.Is(err, domain.ErrExternalIdentityInvalid) {
		t.Fatalf("内置 anonymous 主体不得登录，实际 %v", err)
	}
	if _, _, err := svc.LoginExternal(domain.ExternalIdentity{Source: "oidc", Subject: "", Username: "x"}); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("缺失外部标识应拒绝，实际 %v", err)
	}

	// 同一标识但来源不同：不得沿用既有绑定。
	bindID, err := users.Create("carol", "$argon2id$local", "user")
	if err != nil {
		t.Fatalf("预建用户：%v", err)
	}
	if err := users.BindExternalSubject(bindID, "oidc", "sub-carol"); err != nil {
		t.Fatalf("预绑定：%v", err)
	}
	mismatch := domain.ExternalIdentity{Source: "ldap", Subject: "sub-carol", Username: "carol"}
	if _, _, err := svc.LoginExternal(mismatch); !errors.Is(err, domain.ErrExternalIdentityInvalid) {
		t.Fatalf("来源不符应拒绝，实际 %v", err)
	}
}

// TestLoginExternalRejectsRebindingAndDisabled 已绑定其它标识的账号不被劫持；停用与禁网页登录一律拒绝。
func TestLoginExternalRejectsRebindingAndDisabled(t *testing.T) {
	t.Parallel()
	svc, users := newExternalAuthSvc(t)

	id, err := users.Create("dave", "$argon2id$local", "user")
	if err != nil {
		t.Fatalf("预建用户：%v", err)
	}
	if err := users.BindExternalSubject(id, "oidc", "sub-dave-1"); err != nil {
		t.Fatalf("预绑定：%v", err)
	}
	if _, _, err := svc.LoginExternal(oidcIdentity("dave", "sub-dave-2")); !errors.Is(err, domain.ErrExternalIdentityInvalid) {
		t.Fatalf("已绑定账号不应被另一标识劫持，实际 %v", err)
	}

	// 停用：即便标识匹配也必须拒绝。
	disabled, err := users.Create("erin", "$argon2id$local", "user")
	if err != nil {
		t.Fatalf("预建用户：%v", err)
	}
	if err := users.BindExternalSubject(disabled, "oidc", "sub-erin"); err != nil {
		t.Fatalf("预绑定：%v", err)
	}
	if err := users.Update(disabled, "", "disabled"); err != nil {
		t.Fatalf("停用用户：%v", err)
	}
	if _, _, err := svc.LoginExternal(oidcIdentity("erin", "sub-erin")); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("停用账号应拒绝，实际 %v", err)
	}

	// 禁止网页登录：同样拒绝。
	noWeb, err := users.Create("frank", "$argon2id$local", "user")
	if err != nil {
		t.Fatalf("预建用户：%v", err)
	}
	if err := users.BindExternalSubject(noWeb, "oidc", "sub-frank"); err != nil {
		t.Fatalf("预绑定：%v", err)
	}
	denyRandom := true
	if err := users.Update(noWeb, "", "", &denyRandom); err != nil {
		t.Fatalf("禁用网页登录：%v", err)
	}
	if _, _, err := svc.LoginExternal(oidcIdentity("frank", "sub-frank")); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("禁止网页登录的账号应拒绝，实际 %v", err)
	}
}
