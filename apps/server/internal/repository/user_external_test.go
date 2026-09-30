package repository

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
)

// TestExternalIdentityBindingAndCreation 覆盖迁移 0044 的数据口径与绑定/供给路径：
// 既有行缺省 local 且未绑定；空标识不命中；绑定后可命中且记录来源；
// 外部用户不设本地口令；外部标识全局唯一；多个「未绑定」用户必须能并存。
func TestExternalIdentityBindingAndCreation(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "user-external.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	repo := NewUserRepo(db)

	// 既有本地用户：来源缺省 local、外部标识为空串（迁移不做历史回填）。
	localID, err := repo.Create("alice", "$argon2id$local", "user")
	if err != nil {
		t.Fatalf("创建本地用户：%v", err)
	}
	alice, err := repo.GetByUsername("alice")
	if err != nil {
		t.Fatalf("读取本地用户：%v", err)
	}
	if alice.AuthSource != "local" || alice.ExternalSubject != "" {
		t.Fatalf("既有用户应为本地且未绑定：auth_source=%q subject=%q", alice.AuthSource, alice.ExternalSubject)
	}

	// 空标识表示未绑定，不得命中任何用户。
	if _, err := repo.GetByExternalSubject(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空标识应返回 ErrNotFound，实际 %v", err)
	}

	// 绑定后按标识命中，并记录来源。
	if err := repo.BindExternalSubject(localID, "oidc", "sub-1"); err != nil {
		t.Fatalf("绑定外部身份：%v", err)
	}
	got, err := repo.GetByExternalSubject("sub-1")
	if err != nil {
		t.Fatalf("按外部标识读取：%v", err)
	}
	if got.ID != localID || got.AuthSource != "oidc" {
		t.Fatalf("绑定结果不符：id=%d auth_source=%q", got.ID, got.AuthSource)
	}

	// 新建外部来源用户：不设本地口令，登录只走身份源。
	bobID, err := repo.CreateExternal("bob", "ldap", "cn=bob,ou=people,dc=example,dc=com", "bob@example.com", "user")
	if err != nil {
		t.Fatalf("创建外部用户：%v", err)
	}
	bob, err := repo.GetByUsername("bob")
	if err != nil {
		t.Fatalf("读取外部用户：%v", err)
	}
	if bob.PasswordHash != "" || bob.AuthSource != "ldap" || bob.ExternalSubject != "cn=bob,ou=people,dc=example,dc=com" {
		t.Fatalf("外部用户口径不符：hash=%q source=%q subject=%q", bob.PasswordHash, bob.AuthSource, bob.ExternalSubject)
	}

	// 同一外部标识不得被第二个用户绑定（唯一索引兜底）。
	if err := repo.BindExternalSubject(bobID, "ldap", "sub-1"); err == nil {
		t.Fatal("同一外部标识不应允许绑定到第二个用户")
	}

	// 多个「未绑定」用户必须并存：唯一索引若漏写 WHERE 就会在这里失败。
	if _, err := repo.CreateExternal("carol", "oidc", "", "", "user"); err != nil {
		t.Fatalf("未绑定的外部用户应可创建：%v", err)
	}
	if _, err := repo.CreateExternal("dave", "oidc", "", "", "user"); err != nil {
		t.Fatalf("第二个未绑定用户应可创建（唯一索引须只约束非空标识）：%v", err)
	}

	// 列表查询同样带回新列（显式列名改漏会在断言处暴露）。
	list, err := repo.List(10, 0)
	if err != nil {
		t.Fatalf("列出用户：%v", err)
	}
	for _, u := range list {
		if u.Username == "bob" && u.AuthSource != "ldap" {
			t.Fatalf("列表未带回来源列：%+v", u)
		}
	}
}
