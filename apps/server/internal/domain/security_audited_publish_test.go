package domain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestSecurityAuditedPublishKeepsCommitOnCleanupFailure(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	svc := NewAssetService(repos, assets, blobs, nil)
	repoID, err := repos.Create("raw", "raw", "hosted", "private", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put("raw", "a.txt", strings.NewReader("原内容"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	cleanupCalled := false
	svc.MutationCoordinator().finalizeSnapshot = func(blobstore.RollbackSnapshot) error {
		cleanupCalled = true
		return errors.New("注入提交后清理错误")
	}
	audit := NewSecurityAuditService(db)
	asset, op, err := svc.PutWithSecurityAudit(context.Background(), audit, 1, "raw", "a.txt", strings.NewReader("新内容"), "text/plain")
	if err != nil || !cleanupCalled || op == "" {
		t.Fatalf("清理失败改变提交结果：%q / %t / %v", op, cleanupCalled, err)
	}
	stored, err := assets.GetByPath(repoID, "a.txt")
	if err != nil || stored.BlobHash != asset.BlobHash {
		t.Fatalf("提交的新资产未保留：%v", err)
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM security_event e JOIN security_event_object o ON o.event_sequence=e.sequence JOIN replication_operation_outbox b ON b.operation_id=e.operation_id WHERE e.operation_id=? AND e.committed='committed' AND o.digest=? AND e.http_status IS NULL`, op, "sha256:"+asset.BlobHash); err != nil || count != 1 {
		t.Fatalf("已提交证据丢失：%d / %v", count, err)
	}
}

func TestSecurityAuditedPublishPreservesExistingCommitHook(t *testing.T) {
	db := mutationTestDB(t)
	repos := repository.NewRepoRepo(db)
	svc := NewAssetService(repos, repository.NewAssetRepo(db), blobstore.NewStore(t.TempDir()), nil)
	if _, err := repos.Create("raw", "raw", "hosted", "private", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE test_existing_hook (marker TEXT)`); err != nil {
		t.Fatal(err)
	}
	asset, err := svc.StageBlob(strings.NewReader("测试"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	asset.Path = "a.txt"
	audit := &securityAuditedPublish{ctx: context.Background(), audit: NewSecurityAuditService(db), actor: 1}
	op, err := svc.publishAssets("raw", []*repository.Asset{asset}, func(tx *sqlx.Tx) error {
		_, err := tx.Exec(`INSERT INTO test_existing_hook VALUES ('kept')`)
		return err
	}, audit)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM test_existing_hook JOIN security_event ON marker='kept' WHERE operation_id=?`, op); err != nil || count != 1 {
		t.Fatalf("已有钩子未随安全事件提交：%d / %v", count, err)
	}
}
