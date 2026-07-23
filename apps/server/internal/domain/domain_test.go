package domain_test

import (
	"errors"
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
	svc := domain.NewRepositoryService(repoRepo, aclRepo, repository.NewAssetRepo(db))

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
			if _, err := svc.Create(name, "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
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
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db), repository.NewAssetRepo(db))
	if _, err := svc.Create("public-repo", "raw", "hosted", "public", repository.RepositoryConfig{}); err != nil {
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
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db), repository.NewAssetRepo(db))
	if _, err := svc.CanAccess("ghost", 1, "read"); err != domain.ErrNotFound {
		t.Errorf("未知仓库应返回 ErrNotFound，得 %v", err)
	}
}

// TestCreateConfigValidation 校验 proxy/group 仓库配置的校验规则（FR-13）：
// proxy 必填合法 remoteUrl；group 必填成员且均存在、同 format、禁止自引用。
func TestCreateConfigValidation(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db), repository.NewAssetRepo(db))

	// 前置：一个 raw hosted 与一个 maven hosted，供 group 成员校验用。
	if _, err := svc.Create("raw-hosted", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建 raw-hosted：%v", err)
	}
	if _, err := svc.Create("maven-hosted", "maven", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建 maven-hosted：%v", err)
	}

	cases := []struct {
		name    string
		repo    string
		format  string
		typ     string
		cfg     repository.RepositoryConfig
		wantErr bool
	}{
		{"hosted 带 remoteUrl 应拒绝", "bad-hosted", "raw", "hosted", repository.RepositoryConfig{RemoteURL: "https://x"}, true},
		{"proxy 缺 remoteUrl 应拒绝", "bad-proxy", "raw", "proxy", repository.RepositoryConfig{}, true},
		{"proxy remoteUrl 非法应拒绝", "bad-proxy2", "raw", "proxy", repository.RepositoryConfig{RemoteURL: "not-a-url"}, true},
		{"proxy 合法 remoteUrl 应通过", "ok-proxy", "raw", "proxy", repository.RepositoryConfig{RemoteURL: "https://repo.example.com/raw"}, false},
		{"group 缺成员应拒绝", "bad-group", "raw", "group", repository.RepositoryConfig{}, true},
		{"group 成员不存在应拒绝", "bad-group2", "raw", "group", repository.RepositoryConfig{Members: []string{"ghost"}}, true},
		{"group 成员跨 format 应拒绝", "bad-group3", "raw", "group", repository.RepositoryConfig{Members: []string{"maven-hosted"}}, true},
		{"group 自引用应拒绝", "self-group", "raw", "group", repository.RepositoryConfig{Members: []string{"self-group"}}, true},
		{"group 合法成员应通过", "ok-group", "raw", "group", repository.RepositoryConfig{Members: []string{"raw-hosted"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(c.repo, c.format, c.typ, "private", c.cfg)
			if c.wantErr {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("期望 ErrValidation，得 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通过，得 %v", err)
			}
		})
	}
}

// TestListAssetsPaginationAndPrefix 校验制品浏览（FR-16）：前缀过滤、分页与总数。
func TestListAssetsPaginationAndPrefix(t *testing.T) {
	db := newTestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	assetRepo := repository.NewAssetRepo(db)
	svc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assetRepo)

	if _, err := svc.Create("raw-store", "raw", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	repo, err := repoRepo.GetByName("raw-store")
	if err != nil {
		t.Fatalf("取仓库：%v", err)
	}
	// 造数据：com/ 前缀 3 个，org/ 前缀 1 个。
	paths := []string{"com/a/1.txt", "com/a/2.txt", "com/b/3.txt", "org/c/4.txt"}
	for _, p := range paths {
		if err := assetRepo.Upsert(repo.ID, p, "hash-"+p, 10, "text/plain"); err != nil {
			t.Fatalf("写资产 %s：%v", p, err)
		}
	}

	// 无前缀：总数 4，首页 2 条按 path 升序。
	items, total, err := svc.ListAssets("raw-store", "", 2, 0)
	if err != nil {
		t.Fatalf("ListAssets：%v", err)
	}
	if total != 4 {
		t.Errorf("总数 = %d，期望 4", total)
	}
	if len(items) != 2 || items[0].Path != "com/a/1.txt" || items[1].Path != "com/a/2.txt" {
		t.Errorf("首页分页结果异常：%+v", items)
	}
	// 第二页 offset=2。
	page2, _, err := svc.ListAssets("raw-store", "", 2, 2)
	if err != nil {
		t.Fatalf("ListAssets 第二页：%v", err)
	}
	if len(page2) != 2 || page2[0].Path != "com/b/3.txt" || page2[1].Path != "org/c/4.txt" {
		t.Errorf("第二页分页结果异常：%+v", page2)
	}
	// 前缀过滤 com/：总数 3。
	comItems, comTotal, err := svc.ListAssets("raw-store", "com/", 10, 0)
	if err != nil {
		t.Fatalf("ListAssets 前缀：%v", err)
	}
	if comTotal != 3 || len(comItems) != 3 {
		t.Errorf("前缀 com/ 应 3 条，得 total=%d len=%d", comTotal, len(comItems))
	}
	// 仓库不存在返回 ErrNotFound。
	if _, _, err := svc.ListAssets("ghost", "", 10, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("未知仓库应返回 ErrNotFound，得 %v", err)
	}
}

// TestUsageByFormat 校验使用片段（FR-16）：按 format 返回正确接入片段，
// hosted 含发布 / 上传片段，proxy 仅含下载 / 解析片段。
func TestUsageByFormat(t *testing.T) {
	db := newTestDB(t)
	svc := domain.NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db), repository.NewAssetRepo(db))

	if _, err := svc.Create("mvn", "maven", "hosted", "private", repository.RepositoryConfig{}); err != nil {
		t.Fatalf("建 maven 仓库：%v", err)
	}
	if _, err := svc.Create("npm-proxy", "npm", "proxy", "private", repository.RepositoryConfig{RemoteURL: "https://registry.npmjs.org"}); err != nil {
		t.Fatalf("建 npm proxy 仓库：%v", err)
	}

	const base = "https://artifact.example.com"

	// maven hosted：含 settings.xml、pom、distributionManagement 三段。
	repo, mvnSnips, err := svc.Usage("mvn", base)
	if err != nil {
		t.Fatalf("Usage maven：%v", err)
	}
	if repo.Format != "maven" || len(mvnSnips) != 3 {
		t.Fatalf("maven hosted 应 3 段，得 format=%s len=%d", repo.Format, len(mvnSnips))
	}
	joined := ""
	for _, s := range mvnSnips {
		joined += s.Code
	}
	if !strings.Contains(joined, base+"/repository/mvn") {
		t.Errorf("maven 片段应含仓库地址，得 %q", joined)
	}
	if !strings.Contains(joined, "distributionManagement") {
		t.Error("maven hosted 应含发布片段")
	}

	// npm proxy：仅配置 + 安装两段（不可写，无 publish）。
	_, npmSnips, err := svc.Usage("npm-proxy", base)
	if err != nil {
		t.Fatalf("Usage npm：%v", err)
	}
	if len(npmSnips) != 2 {
		t.Fatalf("npm proxy 应 2 段（无 publish），得 %d", len(npmSnips))
	}
	for _, s := range npmSnips {
		if strings.Contains(s.Code, "npm publish") {
			t.Error("proxy 仓库不应含 publish 片段")
		}
		if !strings.Contains(s.Code, base+"/npm/npm-proxy/") {
			t.Errorf("npm 片段应含 registry 地址，得 %q", s.Code)
		}
	}

	// 仓库不存在返回 ErrNotFound。
	if _, _, err := svc.Usage("ghost", base); !errors.Is(err, domain.ErrNotFound) {
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
