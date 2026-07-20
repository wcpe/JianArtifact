package domain_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTestDB 打开临时 SQLite 并执行迁移，返回连接与各 Repo。
func newTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "domain.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return db
}

// TestCanAccessImplicationMatrix 校验 ACL 蕴含矩阵：
// admin 蕴含 read/write；write 蕴含 read；read 仅 read。
func TestCanAccessImplicationMatrix(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	aclRepo := repository.NewAclRepo(db)
	userRepo := repository.NewUserRepo(db)
	svc := domain.NewRepositoryService(repoRepo, aclRepo)

	// 建一个用户作为 ACL 主体。
	uid, err := userRepo.Create("bob", "hash-placeholder", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}

	cases := []struct {
		granted string
		read    bool
		write   bool
		admin   bool
	}{
		{"read", true, false, false},
		{"write", true, true, false},
		{"admin", true, true, true},
	}
	for _, c := range cases {
		t.Run("granted="+c.granted, func(t *testing.T) {
			name := "private-" + c.granted
			if _, err := svc.Create(name, "raw", "hosted", "private"); err != nil {
				t.Fatalf("建仓库：%v", err)
			}
			if _, err := svc.SetAcl(name, []repository.Acl{{SubjectID: uid, Action: c.granted}}); err != nil {
				t.Fatalf("写 ACL：%v", err)
			}
			for action, want := range map[string]bool{"read": c.read, "write": c.write, "admin": c.admin} {
				got, err := svc.CanAccess(name, uid, action)
				if err != nil {
					t.Fatalf("CanAccess(%s)：%v", action, err)
				}
				if got != want {
					t.Errorf("granted=%s CanAccess(%s) = %t，期望 %t", c.granted, action, got, want)
				}
			}
		})
	}
}

// TestCanAccessPublicRead public 仓库对无 ACL 主体的 read 放行，write/admin 仍拒绝。
func TestCanAccessPublicRead(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db))
	if _, err := svc.Create("public-repo", "raw", "hosted", "public"); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	const strangerID = int64(999)
	if ok, err := svc.CanAccess("public-repo", strangerID, "read"); err != nil || !ok {
		t.Errorf("public 仓库 read 应放行，得 ok=%t err=%v", ok, err)
	}
	if ok, _ := svc.CanAccess("public-repo", strangerID, "write"); ok {
		t.Error("public 仓库 write 不应无 ACL 放行")
	}
}

// TestCanAccessNotFound 未知仓库返回 ErrNotFound。
func TestCanAccessNotFound(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db))
	if _, err := svc.CanAccess("ghost", 1, "read"); err != domain.ErrNotFound {
		t.Errorf("未知仓库应返回 ErrNotFound，得 %v", err)
	}
}

// TestPasswordNotStoredInPlaintext 建用户后 DB 中的口令哈希不得为明文（NFR-06）。
func TestPasswordNotStoredInPlaintext(t *testing.T) {
	db := newTestDB(t)
	userRepo := repository.NewUserRepo(db)
	svc := domain.NewUserService(userRepo)

	const plain = "s3cr3t-p@ssw0rd"
	u, err := svc.Create("carol", plain, "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	stored, err := userRepo.GetByUsername("carol")
	if err != nil {
		t.Fatalf("取用户：%v", err)
	}
	if stored.PasswordHash == plain {
		t.Fatal("口令以明文落库")
	}
	if !strings.HasPrefix(stored.PasswordHash, "$argon2") {
		t.Errorf("口令哈希应为 argon2 编码，得 %q", stored.PasswordHash)
	}
	_ = u
}

// TestTokenDigestNotPlaintext 签发 API Token 后 DB 中不得出现明文（AC-05）。
func TestTokenDigestNotPlaintext(t *testing.T) {
	db := newTestDB(t)
	userRepo := repository.NewUserRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	uid, err := userRepo.Create("dave", "hash-placeholder", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	svc := domain.NewTokenService(tokenRepo)
	plain, tok, err := svc.Create(uid, "ci")
	if err != nil {
		t.Fatalf("签发 Token：%v", err)
	}
	if plain == "" || tok == nil {
		t.Fatal("签发未返回明文或记录")
	}
	// 明文不得能直接用作摘要命中（摘要是明文的 sha256）。
	if _, err := tokenRepo.UserIDByDigest(plain); err == nil {
		t.Fatal("明文竟直接命中摘要列，Token 未做摘要存储")
	}
	// 扫描整表：任一列不得包含明文。
	var rows []struct {
		Name   string `db:"name"`
		Digest string `db:"token_digest"`
	}
	if err := db.Select(&rows, `SELECT name, token_digest FROM api_token`); err != nil {
		t.Fatalf("查 api_token：%v", err)
	}
	for _, r := range rows {
		if strings.Contains(r.Digest, plain) || r.Digest == plain {
			t.Errorf("api_token 摘要列疑似含明文：%q", r.Digest)
		}
	}
}
