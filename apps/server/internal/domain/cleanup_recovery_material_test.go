package domain

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestCleanupPreservesUnconfirmedSnapshotThroughRecovery(t *testing.T) {
	db := mutationTestDB(t)
	root := t.TempDir()
	blobs := blobstore.NewStore(filepath.Join(root, "blobs"))
	repos, assets := repository.NewRepoRepo(db), repository.NewAssetRepo(db)
	svc := NewAssetService(repos, assets, blobs, nil)
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := svc.Put("raw", "a.txt", strings.NewReader("旧制品内容"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	newBlob, err := svc.StageBlob(strings.NewReader("新版制品内容"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	newBlob.RepositoryID, newBlob.Path = repoID, "a.txt"
	c := svc.MutationCoordinator()
	id := newOperationID()
	notCommitted := errors.New("测试完成事务回滚")
	complete := c.completeMutation
	c.completeMutation = func(op string, items []repository.AssetMutationItem, envelope *repository.OperationEnvelope, hooks ...repository.MutationCompletionHook) error {
		err := complete(op, items, envelope, append(hooks, func(*sqlx.Tx) error { return notCommitted })...)
		if !errors.Is(err, notCommitted) {
			t.Fatalf("临时库没有得到预期事务回滚：%v", err)
		}
		return errors.Join(repository.ErrAssetMutationCommitUnconfirmed, err)
	}
	c.readCompletionStatus = func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	audit := NewSecurityAuditService(db)
	item := repository.AssetMutationItem{RepositoryID: repoID, Path: "a.txt", Before: old, After: newBlob}
	returned, err := c.ApplyWithOperationIDAndHook(id, []repository.AssetMutationItem{item}, audit.CommitHook(context.Background(), id, 1, *newBlob))
	if returned != id || !errors.Is(err, errAssetCommitUnknown) {
		t.Fatalf("未保留原未决操作：%q / %v", returned, err)
	}
	q, err := c.mutations.ListQuarantine(id)
	if err != nil || len(q) != 1 || q[0].Status != "pending_gc" || blobs.Exists(old.BlobHash) {
		t.Fatalf("未进入快照保全的状态：%+v / %v", q, err)
	}
	snapshot := blobs.RollbackSnapshot(id)
	if _, err := os.Stat(snapshot.Path); err != nil {
		t.Fatal(err)
	}

	terminal := newOperationID()
	terminalFile := writeQuarantineTestFile(t, root, terminal)
	if err := c.mutations.Begin(terminal, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.mutations.MarkStaged(terminal, []repository.BlobQuarantine{{OperationID: terminal, BlobHash: "残余", QuarantinePath: terminalFile}}); err != nil {
		t.Fatal(err)
	}
	if err := c.mutations.MarkQuarantine(terminal, "残余", "pending_gc", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.mutations.Complete(terminal, nil); err != nil {
		t.Fatal(err)
	}
	cleanup := NewCleanupService(root, repository.NewCleanTaskRepo(db))
	attachRecoveryTestCoordinator(cleanup, c)
	preview, err := cleanup.Preview(CleanupKindQuarantine, 0)
	if err != nil || len(preview.Items) != 1 || filepath.Base(preview.Items[0].Path) != terminal || len(preview.Skipped) != 1 || filepath.Base(preview.Skipped[0]) != id {
		t.Fatalf("预览应保全原快照并允许终态项目：%+v / %v", preview, err)
	}
	if task, err := cleanup.Execute(ExecuteOptions{Kind: CleanupKindQuarantine}); err != nil || task == nil {
		t.Fatalf("终态项目清理失败：%+v / %v", task, err)
	}
	if _, err := os.Stat(terminalFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("已证终态项目仍在原位置：%v", err)
	}
	if _, err := os.Stat(snapshot.Path); err != nil {
		t.Fatalf("执行后的原快照应保留：%v", err)
	}
	if err := c.Recover(); err != nil {
		t.Fatal(err)
	}
	if receipt, err := c.mutations.Get(id); err != nil || receipt.Status != "rolled_back" {
		t.Fatalf("原操作未恢复到终态：%+v / %v", receipt, err)
	}
	stored, err := assets.GetByPath(repoID, "a.txt")
	if err != nil || stored.BlobHash != old.BlobHash {
		t.Fatalf("恢复后旧资产引用丢失：%+v / %v", stored, err)
	}
	f, _, err := blobs.Open(stored.BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil || string(body) != "旧制品内容" {
		t.Fatalf("真实恢复后旧内容不可读：%q / %v", body, err)
	}
	var successes int
	if err := db.Get(&successes, `SELECT count(*) FROM security_event WHERE operation_id=? AND event_type='asset.change_committed'`, id); err != nil || successes != 0 {
		t.Fatalf("恢复追加了提交成功事件：%d / %v", successes, err)
	}
}

func TestCleanupRechecksQuarantineUnderMutationGate(t *testing.T) {
	for _, dbUnavailable := range []bool{false, true} {
		name := "新建未完成意图"
		if dbUnavailable {
			name = "持久判定不可用"
		}
		t.Run(name, func(t *testing.T) {
			db := mutationTestDB(t)
			db.SetMaxOpenConns(1)
			root := t.TempDir()
			c := NewAssetMutationCoordinatorDeferred(db, blobstore.NewStore(filepath.Join(root, "blobs")))
			cleanup := NewCleanupService(root, repository.NewCleanTaskRepo(db))
			attachRecoveryTestCoordinator(cleanup, c)
			first, second := writeQuarantineTestFile(t, root, "a-first"), writeQuarantineTestFile(t, root, "b-second")
			var secondAttempted bool
			cleanup.SetRenameForTest(func(from, to string) error {
				switch from {
				case filepath.Dir(first):
					if c.gate.TryLock() {
						c.gate.Unlock()
						t.Error("实际移动未持有资产写门")
					}
					// 第一项已通过最终判定，在一次性库里改变第二项状态，模拟候选表变旧。
					if dbUnavailable {
						if _, err := db.Exec(`ALTER TABLE blob_quarantine RENAME TO test_quarantine_unavailable`); err != nil {
							t.Fatal(err)
						}
					} else if err := c.mutations.Begin("b-second", nil); err != nil {
						t.Fatal(err)
					}
				case filepath.Dir(second):
					secondAttempted = true
					return errors.New("测试禁止移动第二项目录")
				}
				return os.Rename(from, to)
			})
			if task, err := cleanup.Execute(ExecuteOptions{Kind: CleanupKindQuarantine}); err != nil || task == nil || task.Status != "failed" {
				t.Fatalf("最终判定应在第二项阻止移动并使用既有批次失败语义：%+v / %v", task, err)
			}
			if secondAttempted {
				t.Error("最终判定未阻止第二项的移动调用")
			}
			for _, path := range []string{first, second} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("保全或批次搬回失败：%v", err)
				}
			}
			if dbUnavailable {
				preview, err := cleanup.Preview(CleanupKindQuarantine, 0)
				if err != nil || len(preview.Items) != 0 || len(preview.Skipped) != 2 {
					t.Fatalf("DB 读错时预览也应保全：%+v / %v", preview, err)
				}
			}
		})
	}
}

func attachRecoveryTestCoordinator(cleanup *CleanupService, c *AssetMutationCoordinator) {
	cleanup.SetAssetMutationCoordinator(c)
}

func writeQuarantineTestFile(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, "blobs", "quarantine", id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-copy")
	if err := os.WriteFile(path, []byte("测试副本"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
