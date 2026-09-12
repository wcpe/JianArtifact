package domain_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestAssetServicePutUsesV2OperationOnPrimary 断言 Raw 发布写入**单个原子 v2 operation**，
// 且不再产生可拆分的 v1 asset change（复制退役后变更日志停止写入）。
func TestAssetServicePutUsesV2OperationOnPrimary(t *testing.T) {
	dir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	assets := repository.NewAssetRepo(db)
	repos := repository.NewRepoRepo(db)
	if _, err := repos.Create("raw", "raw", "hosted", "public", ""); err != nil {
		t.Fatalf("创建源仓库：%v", err)
	}
	service := domain.NewAssetService(repos, assets, blobstore.NewStore(filepath.Join(dir, "blobs")), nil)
	service.SetChangeRecorder(domain.NoopChangeRecorder{})
	service.SetNodeIdentity(domain.NewNodeIdentity(repository.NewSettingRepo(db)))
	if _, err := service.Put("raw", "releases/one.bin", bytes.NewReader([]byte("payload")), "application/octet-stream"); err != nil {
		t.Fatalf("发布资产：%v", err)
	}

	records, err := repository.NewReplicationOperationRepo(db).ListRecordsSince(0, 10)
	if err != nil {
		t.Fatalf("读取 v2 记录：%v", err)
	}
	if len(records) != 1 || records[0].Type != "operation" || records[0].Operation == nil {
		t.Fatalf("Raw 发布必须产生单个 v2 operation，实际：%+v", records)
	}
	items := records[0].Operation.Items
	if len(items) != 1 || items[0].Type != domain.EntityAsset || items[0].Key != domain.AssetKey("raw", "releases/one.bin") {
		t.Fatalf("Raw 发布 operation 清单不正确：%+v", items)
	}
	var changes int
	if err := db.Get(&changes, `SELECT COUNT(*) FROM repl_change WHERE entity_type=?`, domain.EntityAsset); err != nil {
		t.Fatalf("统计旧资产变更：%v", err)
	}
	if changes != 0 {
		t.Fatalf("退役后不得再写 v1 asset change，实际 %d 条", changes)
	}
}
