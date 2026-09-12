package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func cargoPublishFrame(t *testing.T, name, version string, crate []byte) []byte {
	t.Helper()
	metadata, err := json.Marshal(map[string]any{"name": name, "vers": version, "deps": []any{}, "features": map[string]any{}})
	if err != nil {
		t.Fatalf("编码 Cargo 元数据：%v", err)
	}
	var frame bytes.Buffer
	if err := binary.Write(&frame, binary.LittleEndian, uint32(len(metadata))); err != nil {
		t.Fatalf("写入 Cargo 元数据长度：%v", err)
	}
	_, _ = frame.Write(metadata)
	if err := binary.Write(&frame, binary.LittleEndian, uint32(len(crate))); err != nil {
		t.Fatalf("写入 Cargo 制品长度：%v", err)
	}
	_, _ = frame.Write(crate)
	return frame.Bytes()
}

func TestCargoPublishIndexFailureDoesNotExposeCrate(t *testing.T) {
	service, assets, blobs, mutator, repoID := newLifecycleAssetService(t)
	if _, err := mutator.mutations.DB().Exec(`UPDATE repository SET format='cargo' WHERE id=?`, repoID); err != nil {
		t.Fatalf("设置 Cargo 仓库：%v", err)
	}
	repoSvc := NewRepositoryService(repository.NewRepoRepo(mutator.mutations.DB()), repository.NewAclRepo(mutator.mutations.DB()), assets, NewSettingService(repository.NewSettingRepo(mutator.mutations.DB())), repository.NewUserRepo(mutator.mutations.DB()))
	cargo := NewCargoService(service, repoSvc)
	if _, err := mutator.mutations.DB().Exec(`CREATE TRIGGER reject_cargo_index BEFORE INSERT ON asset WHEN NEW.path LIKE 'cargo/index/%' BEGIN SELECT RAISE(ABORT, '注入 Cargo 索引失败'); END`); err != nil {
		t.Fatalf("创建失败注入：%v", err)
	}
	mutator.finalize = func(blobstore.QuarantineEntry) error { return errors.New("注入物理回收失败") }
	crate := []byte("crate-payload")
	if _, err := cargo.Publish(context.Background(), "raw", bytes.NewReader(cargoPublishFrame(t, "demo", "1.0.0", crate))); err == nil {
		t.Fatal("Cargo 索引失败必须返回错误")
	}
	if _, err := assets.GetByPath(repoID, cargoAssetPath("demo", "1.0.0")); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Cargo 索引失败不得暴露 crate：%v", err)
	}
	if _, err := assets.GetByPath(repoID, cargoHostedIndexAssetPath("demo")); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Cargo 索引失败不得暴露 index：%v", err)
	}
	if err := blobs.WalkActive(func(string) error { return errors.New("Cargo 索引失败不得留下 blob") }); err != nil {
		t.Fatalf("枚举活动 blob：%v", err)
	}
}

func TestCargoPublishGuardRejectsBeforeCrateTempWrite(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)
	service, assets, _, mutator, repoID := newLifecycleAssetService(t)
	if _, err := mutator.mutations.DB().Exec(`UPDATE repository SET format='cargo' WHERE id=?`, repoID); err != nil {
		t.Fatalf("设置 Cargo 仓库：%v", err)
	}
	repoSvc := NewRepositoryService(repository.NewRepoRepo(mutator.mutations.DB()), repository.NewAclRepo(mutator.mutations.DB()), assets, NewSettingService(repository.NewSettingRepo(mutator.mutations.DB())), repository.NewUserRepo(mutator.mutations.DB()))
	cargo := NewCargoService(service, repoSvc)
	crate := []byte("crate-payload")
	guardErr := errors.New("额度不足")

	_, err := cargo.PublishWithGuard(context.Background(), "raw", bytes.NewReader(cargoPublishFrame(t, "demo", "1.0.0", crate)), func(path string, size int64) (func(bool), error) {
		if path != cargoAssetPath("demo", "1.0.0") || size != int64(len(crate)) {
			t.Fatalf("守卫参数错误：path=%q size=%d", path, size)
		}
		entries, readErr := os.ReadDir(tempDir)
		if readErr != nil {
			t.Fatalf("读取临时目录：%v", readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "jianartifact-cargo-") {
				t.Fatal("发布守卫拒绝前不得写入 Cargo 临时文件")
			}
		}
		return nil, guardErr
	})
	if !errors.Is(err, guardErr) {
		t.Fatalf("发布守卫拒绝必须原样返回，实际：%v", err)
	}
	if err := service.blobs.WalkActive(func(string) error { return errors.New("守卫拒绝不得留下 blob") }); err != nil {
		t.Fatal(err)
	}
}
