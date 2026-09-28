package repository_test

// 发布策略的路径前缀在「无前缀」时必须落库为 [] 且读回为非 nil 空切片：
// 契约把 allowedPrefixes 定为 required 数组，nil 切片会被 json.Marshal 写成 null，
// 管理端 value.allowedPrefixes.join(...) 随即抛
// TypeError: Cannot read properties of null (reading 'join')。

import (
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 写入时 PathPrefixes 为 nil：落库必须是 []（不是 null），读回也必须非 nil。
func TestPublishPolicyPrefixesNeverNil(t *testing.T) {
	policies, db, userID, repoID := newPublishPolicyEnv(t)

	if err := policies.Upsert(repository.PublishPolicy{UserID: userID, RepositoryID: repoID}); err != nil {
		t.Fatalf("保存策略：%v", err)
	}

	var stored string
	if err := db.Get(&stored, `SELECT path_prefixes_json FROM publish_policy WHERE user_id = ? AND repository_id = ?`, userID, repoID); err != nil {
		t.Fatalf("读原始列：%v", err)
	}
	if stored != "[]" {
		t.Fatalf("无前缀应落库为 []，实际 %q", stored)
	}

	got, err := policies.Get(userID, repoID)
	if err != nil {
		t.Fatalf("读策略：%v", err)
	}
	if got.PathPrefixes == nil {
		t.Fatal("读回的 PathPrefixes 为 nil，会被序列化成 null 并让前端 .join 崩溃")
	}
	if len(got.PathPrefixes) != 0 {
		t.Fatalf("应为空切片，实际 %v", got.PathPrefixes)
	}
}

// 历史行可能存着 'null'（早期 json.Marshal(nil 切片) 的产物）：读回同样要归一为空切片。
func TestPublishPolicyLegacyNullRowReadsAsEmpty(t *testing.T) {
	policies, db, userID, repoID := newPublishPolicyEnv(t)

	if _, err := db.Exec(`INSERT INTO publish_policy (user_id, repository_id, path_prefixes_json) VALUES (?, ?, 'null')`, userID, repoID); err != nil {
		t.Fatalf("插入历史 null 策略行：%v", err)
	}

	got, err := policies.Get(userID, repoID)
	if err != nil {
		t.Fatalf("读策略：%v", err)
	}
	if got.PathPrefixes == nil {
		t.Fatal("历史 'null' 行读回为 nil，会被序列化成 null 并让前端 .join 崩溃")
	}
	if len(got.PathPrefixes) != 0 {
		t.Fatalf("应为空切片，实际 %v", got.PathPrefixes)
	}
}

// 有前缀时往返无损（确认归一化没有吃掉真实数据）。
func TestPublishPolicyPrefixesRoundtrip(t *testing.T) {
	policies, _, userID, repoID := newPublishPolicyEnv(t)

	want := []string{"releases", "snapshots"}
	if err := policies.Upsert(repository.PublishPolicy{UserID: userID, RepositoryID: repoID, PathPrefixes: want}); err != nil {
		t.Fatalf("保存策略：%v", err)
	}
	got, err := policies.Get(userID, repoID)
	if err != nil {
		t.Fatalf("读策略：%v", err)
	}
	if len(got.PathPrefixes) != len(want) {
		t.Fatalf("前缀数量不符：得 %v，期望 %v", got.PathPrefixes, want)
	}
	for i := range want {
		if got.PathPrefixes[i] != want[i] {
			t.Fatalf("第 %d 项不符：得 %q，期望 %q", i, got.PathPrefixes[i], want[i])
		}
	}
}

// newPublishPolicyEnv 建库并预置外键依赖（一个用户与一个 hosted 仓库）。
// publish_policy 对 user/repository 都有 FK，缺依赖无法写入。
func newPublishPolicyEnv(t *testing.T) (*repository.PublishPolicyRepo, *persistence.DB, int64, int64) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	userID, err := repository.NewUserRepo(db).Create("policy-user", "hash", "user")
	if err != nil {
		t.Fatalf("建用户：%v", err)
	}
	repoID, err := repository.NewRepoRepo(db).Create("raw-a", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	return repository.NewPublishPolicyRepo(db), db, userID, repoID
}
