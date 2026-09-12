package repository_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func newOperationRepo(t *testing.T) *repository.ReplicationOperationRepo {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "operation.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	return repository.NewReplicationOperationRepo(db)
}

func testEnvelope() repository.OperationEnvelope {
	return repository.OperationEnvelope{
		OperationID: "op-test-001", SourceNode: "node-a",
		Version:   repository.ReplicationVersion{NodeID: "node-a", TS: "2026-08-21T00:00:00Z"},
		ItemCount: 1,
		Items: []repository.OperationItem{{
			Type: "asset", Key: "asset:raw/a.txt", Op: "delete",
			Version: repository.ReplicationVersion{NodeID: "node-a", TS: "2026-08-21T00:00:00Z"},
		}},
	}
}

func TestReplicationOperationOutboxUsesGlobalSeqAndAtomicRecord(t *testing.T) {
	outbox := newOperationRepo(t)
	seq, err := outbox.AppendStandalone(testEnvelope())
	if err != nil {
		t.Fatalf("追加 operation：%v", err)
	}
	if seq != 1 {
		t.Fatalf("operation seq=%d，期望首条全局 seq=1", seq)
	}
	records, err := outbox.ListRecordsSince(0, 1)
	if err != nil {
		t.Fatalf("读取 v2 第一页：%v", err)
	}
	if len(records) != 1 || records[0].Type != "change" || records[0].Seq != 1 {
		t.Fatalf("第一条应为普通 change：%+v", records)
	}
	records, err = outbox.ListRecordsSince(1, 1)
	if err != nil {
		t.Fatalf("读取 v2 operation 页：%v", err)
	}
	if len(records) != 1 || records[0].Type != "operation" || records[0].Operation == nil || len(records[0].Operation.Items) != 1 {
		t.Fatalf("operation 必须作为单条不可拆分 record：%+v", records)
	}
	if records[0].Operation.OperationID != "op-test-001" || records[0].Operation.Seq != 2 {
		t.Fatalf("operation envelope 字段不符：%+v", records[0].Operation)
	}
	has, err := outbox.HasAfter(1)
	if err != nil || !has {
		t.Fatalf("since=1 应检测到 operation，has=%v err=%v", has, err)
	}
}

func TestReplicationOperationOutboxRejectsInvalidManifestAndOversizedBatch(t *testing.T) {
	outbox := newOperationRepo(t)
	bad := testEnvelope()
	bad.ManifestSHA256 = "wrong"
	if _, err := outbox.AppendStandalone(bad); err == nil {
		t.Fatal("错误 manifest 应拒绝")
	}
	tooMany := testEnvelope()
	tooMany.OperationID = "op-too-many"
	tooMany.Items = make([]repository.OperationItem, repository.MaxOperationItems+1)
	for i := range tooMany.Items {
		tooMany.Items[i] = repository.OperationItem{Type: "asset", Key: "asset:raw/a", Op: "delete", Version: tooMany.Version}
	}
	if _, err := outbox.AppendStandalone(tooMany); err == nil {
		t.Fatal("超过传输上限的 operation 应拒绝")
	}
}

func TestValidateOperationEnvelopeRejectsInconsistentOrAmbiguousItems(t *testing.T) {
	missingCount := testEnvelope()
	missingCount.ItemCount = 0
	if err := repository.ValidateOperationEnvelope(missingCount); err == nil {
		t.Fatal("缺少 itemCount 应拒绝")
	}
	base := testEnvelope()
	base.ItemCount = 2
	if err := repository.ValidateOperationEnvelope(base); err == nil {
		t.Fatal("itemCount 与 items 数量不一致应拒绝")
	}
	duplicate := testEnvelope()
	duplicate.Items = append(duplicate.Items, duplicate.Items[0])
	duplicate.ItemCount = len(duplicate.Items)
	if err := repository.ValidateOperationEnvelope(duplicate); err == nil {
		t.Fatal("重复实体键应拒绝")
	}
	badDependency := testEnvelope()
	badDependency.Items[0].DependsOn = []string{""}
	if err := repository.ValidateOperationEnvelope(badDependency); err == nil {
		t.Fatal("空依赖键应拒绝")
	}
}

func TestValidateOperationEnvelopeRequiresAssetPutBlobDeclaration(t *testing.T) {
	validHash := strings.Repeat("a", 64)
	otherHash := strings.Repeat("b", 64)
	base := testEnvelope()
	base.Items[0] = repository.OperationItem{
		Type: "asset", Key: "asset:raw/a.txt", Op: "put",
		Data:    `{"path":"a.txt","blobHash":"","size":1}`,
		Version: base.Version,
	}
	if err := repository.ValidateOperationEnvelope(base); err == nil {
		t.Fatal("asset put 缺少 blobHash 必须拒绝")
	}

	mismatch := base
	mismatch.Items = append([]repository.OperationItem(nil), base.Items...)
	mismatch.Items[0].Data = `{"path":"a.txt","blobHash":"` + validHash + `","size":1}`
	mismatch.Items[0].BlobHashes = []string{otherHash}
	if err := repository.ValidateOperationEnvelope(mismatch); err == nil {
		t.Fatal("asset put blobHash 未声明或声明无关 blob 必须拒绝")
	}

	valid := mismatch
	valid.Items = append([]repository.OperationItem(nil), mismatch.Items...)
	valid.Items[0].BlobHashes = []string{validHash}
	if err := repository.ValidateOperationEnvelope(valid); err != nil {
		t.Fatalf("正确声明 asset put blob 应通过：%v", err)
	}
}
