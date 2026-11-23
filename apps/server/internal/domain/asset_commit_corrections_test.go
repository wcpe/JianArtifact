package domain

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestAssetCommitStateRejectsOrdinaryRollbackOfCompleted(t *testing.T) {
	mutations := repository.NewAssetMutationRepo(mutationTestDB(t))
	const id = "original-intent"
	if err := mutations.Begin(id, nil); err != nil {
		t.Fatal(err)
	}
	if err := mutations.MarkStaged(id, nil); err != nil {
		t.Fatal(err)
	}
	if err := mutations.Complete(id, nil); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		name string
		run  func() error
	}{
		{"staged", func() error { return mutations.MarkStaged(id, nil) }},
		{"rolling_back", func() error { return mutations.MarkRollingBack(id, "失败") }},
		{"rolled_back", func() error { return mutations.MarkRolledBack(id, "失败") }},
		{"duplicate_complete", func() error { return mutations.Complete(id, nil) }},
	} {
		if err := transition.run(); err == nil {
			t.Errorf("非法转换 %s 未报错", transition.name)
		}
	}
	if got, err := mutations.Get(id); err != nil || got.Status != "completed" {
		t.Fatalf("完成回执被覆盖：%+v / %v", got, err)
	}
}

func TestAssetCommitFailureDiscardsConnectionBeforeReceipt(t *testing.T) {
	db := mutationTestDB(t)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE test_commit_parent (id INT PRIMARY KEY);
		CREATE TABLE test_commit_child (id INT REFERENCES test_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatal(err)
	}
	mutations := repository.NewAssetMutationRepo(db)
	const id = "deferred-commit"
	if err := mutations.Begin(id, nil); err != nil {
		t.Fatal(err)
	}
	if err := mutations.MarkStaged(id, nil); err != nil {
		t.Fatal(err)
	}
	err := mutations.CompleteWithHook(id, nil, func(tx *sqlx.Tx) error {
		_, err := tx.Exec(`INSERT INTO test_commit_child VALUES (1)`)
		return err
	})
	var sqliteErr *sqlite.Error
	if !errors.Is(err, repository.ErrAssetMutationCommitUnconfirmed) || !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		t.Fatalf("未得到真实的提交期外键错误：%v", err)
	}
	if got := db.Stats().OpenConnections; got != 0 {
		t.Errorf("无法复用提交已报错的连接，连接池剩余 %d 条", got)
	}
	if status, err := mutations.ReadCompletionStatus(context.Background(), id); err != nil || status != "staged" {
		t.Fatalf("新连接未读到已证未提交的原回执：%q / %v", status, err)
	}
}

func TestSecurityAuditedPublishCommitReceiptDecision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed bool
		readErr   error
	}{
		{name: "已完成", committed: true},
		{name: "已证未提交"},
		{name: "已完成但回读超时", committed: true, readErr: context.DeadlineExceeded},
		{name: "未完成但回执暂不可见", readErr: repository.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := mutationTestDB(t)
			repos := repository.NewRepoRepo(db)
			assets := repository.NewAssetRepo(db)
			blobs := blobstore.NewStore(t.TempDir())
			svc := NewAssetService(repos, assets, blobs, nil)
			repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
			if err != nil {
				t.Fatal(err)
			}
			original, err := svc.Put("raw", "a.txt", strings.NewReader("原内容"), "text/plain")
			if err != nil {
				t.Fatal(err)
			}
			c := svc.MutationCoordinator()
			var id, newHash string
			var called, reads int
			commitErr := errors.New("受控注入：提交调用报告错误")
			complete := c.completeMutation
			c.completeMutation = func(op string, items []repository.AssetMutationItem, envelope *repository.OperationEnvelope, hooks ...repository.MutationCompletionHook) error {
				called++
				id, newHash = op, items[0].After.BlobHash
				if tc.committed {
					if err := complete(op, items, envelope, hooks...); err != nil {
						t.Fatal(err)
					}
				} else if err := complete(op, items, envelope, append(hooks, func(*sqlx.Tx) error { return commitErr })...); !errors.Is(err, commitErr) {
					t.Fatalf("SQLite 测试事务未如预期回滚：%v", err)
				}
				return errors.Join(repository.ErrAssetMutationCommitUnconfirmed, commitErr)
			}
			read := c.readCompletionStatus
			c.readCompletionStatus = func(ctx context.Context, op string) (string, error) {
				reads++
				if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil || op != id {
					t.Fatalf("回读必须查原操作并使用独立短预算：%q / %v", op, ctx.Err())
				}
				if tc.readErr != nil {
					return "", tc.readErr
				}
				return read(ctx, op)
			}
			asset, returned, err := svc.PutWithSecurityAudit(context.Background(), NewSecurityAuditService(db), 1, "raw", "a.txt", strings.NewReader("新内容"), "text/plain")
			if called != 1 || reads != 1 || id == "" {
				t.Fatalf("完成/回读次数或原 ID 异常：%d/%d/%q", called, reads, id)
			}
			snapshot := c.blobs.RollbackSnapshot(id)
			switch {
			case tc.readErr != nil:
				if asset != nil || returned != id || !errors.Is(err, errAssetCommitUnknown) || !errors.Is(err, ErrSecurityAuditUnavailable) || !errors.Is(err, tc.readErr) {
					t.Fatalf("未返回保留原 intent 的未决结果：%q / %v", returned, err)
				}
				if _, err := os.Stat(snapshot.Path); err != nil || blobs.Exists(original.BlobHash) || !blobs.Exists(newHash) {
					t.Fatalf("未决时不得还原旧 blob 或移除恢复材料：%v", err)
				}
				if release, ok := blobs.BeginUnreferencedRemoval(newHash); ok {
					release()
					t.Fatal("未决时新 blob 的暂存保护已被释放")
				}
			case tc.committed:
				if err != nil || returned != id || asset == nil || asset.BlobHash != newHash {
					t.Fatalf("原回执已经证明成功：%q / %v", returned, err)
				}
			case !errors.Is(err, commitErr) || errors.Is(err, ErrSecurityAuditUnavailable) || returned != "" || !blobs.Exists(original.BlobHash) || blobs.Exists(newHash):
				t.Fatalf("证实未提交后应补偿并返回原失败：%q / %v", returned, err)
			}
			wantStatus, wantHash, wantEvents := "rolled_back", original.BlobHash, 0
			if tc.committed {
				wantStatus, wantHash, wantEvents = "completed", newHash, 1
			}
			if receipt, err := c.mutations.Get(id); err != nil || (tc.readErr != nil && !tc.committed && receipt.Status != "staged") {
				t.Fatalf("没有保留原回执：%+v / %v", receipt, err)
			}
			for range 2 {
				if err := c.Recover(); err != nil {
					t.Fatal(err)
				}
				receipt, err := c.mutations.Get(id)
				if err != nil || receipt.Status != wantStatus || !blobs.Exists(wantHash) {
					t.Fatalf("重复恢复未收敛到正确回执/活动 blob：%+v / %v", receipt, err)
				}
				stored, err := assets.GetByPath(repoID, "a.txt")
				if err != nil || stored.BlobHash != wantHash || blobs.Exists(original.BlobHash) == tc.committed {
					t.Fatalf("重复恢复改变资产视图：%+v / %v", stored, err)
				}
				if _, err := os.Stat(snapshot.Path); tc.committed && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("确认提交后未清理冗余快照：%v", err)
				}
				var events, outbox int
				if err := db.Get(&events, `SELECT count(*) FROM security_event WHERE operation_id=? AND event_type='asset.change_committed'`, id); err != nil || events != wantEvents {
					t.Fatalf("操作产生的成功事件数错误：%d / %v", events, err)
				}
				if err := db.Get(&outbox, `SELECT count(*) FROM replication_operation_outbox WHERE operation_id=?`, id); err != nil || outbox != wantEvents {
					t.Fatalf("操作产生的 outbox 数错误：%d / %v", outbox, err)
				}
			}
		})
	}
}

func TestSecurityAuditedPublishMapsSQLiteBusyBeforeAuditHook(t *testing.T) {
	db := mutationTestDB(t)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	repos := repository.NewRepoRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := NewAssetService(repos, repository.NewAssetRepo(db), blobs, nil)
	if _, err := repos.Create("raw", "raw", "hosted", "private", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put("raw", "a.txt", strings.NewReader("原内容"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	c := svc.MutationCoordinator()
	var heldConn *sqlx.Conn
	var heldTx *sqlx.Tx
	t.Cleanup(func() {
		if heldTx != nil {
			_ = heldTx.Rollback()
		}
		if heldConn != nil {
			_ = heldConn.Close()
		}
	})
	c.finalize = func(entry blobstore.QuarantineEntry) error {
		if err := blobs.Finalize(entry); err != nil {
			return err
		}
		var err error
		heldConn, err = db.Connx(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		heldTx, err = heldConn.BeginTxx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := heldTx.Exec(`UPDATE repository SET name=name WHERE name='raw'`); err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`PRAGMA busy_timeout=0`)
		return err
	}
	var hookCalled bool
	c.SetStorageQuotaCheck(func(*sqlx.Tx, []repository.AssetMutationItem) error {
		hookCalled = true
		return nil
	})
	_, _, err := svc.PutWithSecurityAudit(context.Background(), NewSecurityAuditService(db), 1, "raw", "a.txt", strings.NewReader("新内容"), "text/plain")
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY || hookCalled || heldTx == nil {
		t.Fatalf("未在 Hook 前得到真实 SQLite BUSY：hook=%t / %v", hookCalled, err)
	}
	if !errors.Is(err, ErrSecurityAuditUnavailable) {
		t.Fatalf("Hook 前 BUSY 未稳定映射：%v", err)
	}
}
