package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestCargoMigrationCratePathAcceptsNexusDownloadRoute(t *testing.T) {
	for _, source := range []string{
		"api/v1/crates/fr31-cargo-source/0.1.0/download",
		"crates/fr31-cargo-source/0.1.0/download",
	} {
		name, version, ok := cargoMigrationCratePath(source)
		if !ok || name != "fr31-cargo-source" || version != "0.1.0" {
			t.Fatalf("Nexus Cargo 下载路径解析失败：source=%q name=%q version=%q ok=%t", source, name, version, ok)
		}
	}
}

func TestCargoMigrationIndexMergesSourceMetadata(t *testing.T) {
	service, assets, _, mutator, repoID := newLifecycleAssetService(t)
	db := mutator.mutations.DB()
	if _, err := db.Exec(`UPDATE repository SET format='cargo' WHERE id=?`, repoID); err != nil {
		t.Fatalf("设置 Cargo 仓库：%v", err)
	}
	repoSvc := NewRepositoryService(repository.NewRepoRepo(db), repository.NewAclRepo(db), assets, NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	cargo := NewCargoService(service, repoSvc)
	crate := []byte("crate-payload")
	if _, err := cargo.Publish(context.Background(), "raw", bytes.NewReader(cargoPublishFrame(t, "demo", "1.0.0", crate))); err != nil {
		t.Fatalf("发布 Cargo crate：%v", err)
	}
	before, err := assets.GetByPath(repoID, cargoHostedIndexAssetPath("demo"))
	if err != nil {
		t.Fatalf("读取生成索引：%v", err)
	}
	checksum := sha256.Sum256(crate)
	line := map[string]any{"name": "demo", "vers": "1.0.0", "deps": []any{}, "cksum": fmt.Sprintf("%x", checksum[:]), "features": map[string]any{}, "yanked": false, "authors": []string{"来源"}, "description": "来源索引字段", "v": 1}
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("编码来源索引：%v", err)
	}
	if err := cargo.ImportMigrationAsset(MigrationAsset{Repository: "raw", Format: "cargo", SourcePath: "de/mo/demo", Body: bytes.NewReader(append(data, '\n'))}); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("来源索引校验错误：%v", err)
		}
		t.Fatalf("导入来源索引：%v", err)
	}
	after, err := assets.GetByPath(repoID, cargoHostedIndexAssetPath("demo"))
	if err != nil {
		t.Fatalf("读取导入后索引：%v", err)
	}
	if before.BlobHash == after.BlobHash {
		t.Fatal("来源索引新增元数据后应更新索引")
	}
	_, rc, err := service.Get("raw", cargoHostedIndexAssetPath("demo"))
	if err != nil {
		t.Fatalf("读取合并索引：%v", err)
	}
	defer func() { _ = rc.Close() }()
	merged, err := io.ReadAll(rc)
	if err != nil || !bytes.Contains(merged, []byte(`"authors":["来源"]`)) {
		t.Fatalf("合并索引缺少来源元数据：%s err=%v", merged, err)
	}
}
