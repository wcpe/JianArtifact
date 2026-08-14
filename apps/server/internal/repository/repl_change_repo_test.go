package repository_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// newTestRepl 打开临时 SQLite、迁移并返回 ReplChangeRepo。
func newTestRepl(t *testing.T) *repository.ReplChangeRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "repl.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewReplChangeRepo(db)
}

// TestReplChangeAppendAndLatestSeq 追加变更日志：seq 递增、LatestSeq 正确、空表返回 0。
func TestReplChangeAppendAndLatestSeq(t *testing.T) {
	repl := newTestRepl(t)

	seq, err := repl.LatestSeq()
	if err != nil {
		t.Fatalf("空表 LatestSeq：%v", err)
	}
	if seq != 0 {
		t.Errorf("空表 LatestSeq = %d，期望 0", seq)
	}

	s1, err := repl.Append("node-a", "put", "asset", "asset:r1/a.txt", `{"path":"a.txt"}`, "2026-08-13T00:00:00.000000001Z")
	if err != nil {
		t.Fatalf("Append 1：%v", err)
	}
	s2, err := repl.Append("node-a", "put", "repository", "repo:r1", `{"name":"r1"}`, "2026-08-13T00:00:00.000000002Z")
	if err != nil {
		t.Fatalf("Append 2：%v", err)
	}
	if s1 != 1 || s2 != 2 {
		t.Errorf("seq = %d,%d，期望 1,2", s1, s2)
	}
	latest, err := repl.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq：%v", err)
	}
	if latest != 2 {
		t.Errorf("LatestSeq = %d，期望 2", latest)
	}
}

// TestReplChangeLastChangeOf 按实体取最近变更：ts 更大者胜出；无记录返回 ErrNotFound。
func TestReplChangeLastChangeOf(t *testing.T) {
	repl := newTestRepl(t)

	if _, err := repl.LastChangeOf("asset", "asset:r1/a.txt"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("无记录 LastChangeOf 应返回 ErrNotFound，得 %v", err)
	}

	_, _ = repl.Append("node-a", "put", "asset", "asset:r1/a.txt", `{"v":1}`, "2026-08-13T00:00:01Z")
	_, _ = repl.Append("node-b", "put", "asset", "asset:r1/a.txt", `{"v":2}`, "2026-08-13T00:00:02Z")

	last, err := repl.LastChangeOf("asset", "asset:r1/a.txt")
	if err != nil {
		t.Fatalf("LastChangeOf：%v", err)
	}
	if last.NodeID != "node-b" || last.TS != "2026-08-13T00:00:02Z" {
		t.Errorf("最近变更应为 node-b/ts2，得 node=%s ts=%s", last.NodeID, last.TS)
	}
}

// TestReplChangeListSince 按 seq 断点续传：返回 seq 之后的有序变更；limit 生效。
func TestReplChangeListSince(t *testing.T) {
	repl := newTestRepl(t)
	for i := 1; i <= 5; i++ {
		if _, err := repl.Append("node-a", "put", "setting", "setting:k", `{"v":1}`, "ts"); err != nil {
			t.Fatalf("Append %d：%v", i, err)
		}
	}

	all, err := repl.ListSince(0, 0)
	if err != nil {
		t.Fatalf("ListSince(0)：%v", err)
	}
	if len(all) != 5 {
		t.Fatalf("ListSince(0) = %d 条，期望 5", len(all))
	}
	for i, c := range all {
		if c.Seq != int64(i+1) {
			t.Errorf("第 %d 条 seq = %d，期望 %d", i, c.Seq, i+1)
		}
	}

	// 从 seq=3 续传：应剩 seq 4,5。
	rest, err := repl.ListSince(3, 0)
	if err != nil {
		t.Fatalf("ListSince(3)：%v", err)
	}
	if len(rest) != 2 || rest[0].Seq != 4 || rest[1].Seq != 5 {
		t.Errorf("ListSince(3) = %+v，期望 seq 4,5", rest)
	}

	// limit=1 只取 1 条。
	one, err := repl.ListSince(0, 1)
	if err != nil {
		t.Fatalf("ListSince(0,1)：%v", err)
	}
	if len(one) != 1 || one[0].Seq != 1 {
		t.Errorf("ListSince(0,1) = %+v，期望 seq 1", one)
	}
}
