package domain

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestPrivateCandidateBoundary(t *testing.T) {
	ctx := t.Context()
	f := newBackupFixture(t)
	assets := NewAssetService(repository.NewRepoRepo(f.db), repository.NewAssetRepo(f.db), f.store, nil)
	const old, private, added = "已公开内容", "仅私有候选", "备份差集对照"
	oldHash := f.seedAssets(t, old)[0]
	orphan, _, _, _, err := f.store.Put(strings.NewReader("活动孤儿对照"))
	if err != nil {
		t.Fatal(err)
	}
	readCounts := func() [3]int {
		t.Helper()
		var counts [3]int
		err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM asset), (SELECT COUNT(*) FROM format_metadata), (SELECT COUNT(*) FROM asset_path_fts)`).Scan(&counts[0], &counts[1], &counts[2])
		if err != nil {
			t.Fatal(err)
		}
		return counts
	}
	before := readCounts()
	for _, body := range []string{private, old} {
		candidate, err := f.store.StagePrivateCandidate(ctx, strings.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		if (candidate.SHA256() == oldHash) != (body == old) {
			t.Fatal("摘要对照不匹配")
		}
		t.Cleanup(func() {
			if err := candidate.Discard(); err != nil {
				t.Error(err)
			}
		})
	}
	if actual := readCounts(); before != actual {
		t.Fatalf("候选改变了 SQLite 公开投影：%v → %v", before, actual)
	}
	_, rc, err := assets.Get("raw-repo-1", "new.bin")
	if rc != nil {
		_ = rc.Close()
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("新资产路径应保持不可见：%v", err)
	}
	checkExisting := func() {
		t.Helper()
		asset, rc, err := assets.Get("raw-repo-1", "a/0.bin")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(rc)
		if err := errors.Join(readErr, rc.Close()); err != nil || string(body) != old || asset.BlobHash != oldHash {
			t.Fatalf("合法旧引用未保持可读：%v", err)
		}
	}
	checkExisting()

	temp, err := f.store.CreateOCIUploadTemp()
	if err != nil {
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(temp.Name(), aged, aged); err != nil {
		t.Fatal(err)
	}
	candidateDir := filepath.Join(f.dataDir, "blobs", "tmp", "admission-candidates")
	privateFiles := func() map[string]string {
		t.Helper()
		entries, err := os.ReadDir(candidateDir)
		if err != nil {
			t.Fatal(err)
		}
		entriesByBody := make(map[string]string, len(entries))
		for _, entry := range entries {
			path := filepath.Join(candidateDir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			entriesByBody[string(data)] = entry.Name()
		}
		if len(entries) != 2 || entriesByBody[private] == "" || entriesByBody[old] == "" {
			t.Fatal("必须存在两个独立的私有字节对象")
		}
		return entriesByBody
	}
	existing := privateFiles()
	for _, file := range existing {
		if err := os.Chtimes(filepath.Join(candidateDir, file), aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := NewStorageCleanupService(nil, assets, f.store, StorageCleanupOptions{TempMaxAge: 24 * time.Hour})
	if result, err := cleanup.Run(); err != nil || result.TempFiles != 1 {
		t.Fatalf("真实维护应只清理 OCI 对照文件：%+v，%v", result, err)
	}
	if _, err := os.Stat(temp.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("过期 OCI 对照仍在：%v", err)
	}
	if count, err := assets.CleanupUnreferencedBlobs(); err != nil || count != 1 || f.store.Exists(orphan) {
		t.Fatalf("GC 应只清理活动孤儿：%d，%v", count, err)
	}
	checkExisting()

	assertBundle := func(base string, body, sidecar []archive.BlobRef) string {
		t.Helper()
		rec, err := f.svc.Generate(ctx, CreateBackupOptions{Mode: archive.ModeHot, BasePackageID: base})
		if err != nil {
			t.Fatal(err)
		}
		reader, manifest, err := archive.Open(f.svc.PackagePath(rec.PackageID))
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Verify(true); err != nil {
			t.Fatal(err)
		}
		idx, err := reader.BlobIndex()
		if err != nil || !reflect.DeepEqual(idx, archive.NewBlobIndex(body)) {
			t.Fatalf("备份实物包体不等于已提交引用子集：%v，%v", idx, err)
		}
		idx, err = f.svc.readSidecar(rec.PackageID)
		if err != nil || !reflect.DeepEqual(idx, archive.NewBlobIndex(sidecar)) {
			t.Fatalf("侧车不是已提交引用全集：%v，%v", idx, err)
		}
		if base != "" && (manifest.Expected == nil || *manifest.Expected != idx.Summary()) {
			t.Fatal("差包的目标摘要不是已提交引用集合摘要")
		}
		return rec.PackageID
	}
	oldRef := archive.BlobRef{Hash: oldHash, Size: int64(len(old))}
	base := assertBundle("", []archive.BlobRef{oldRef}, []archive.BlobRef{oldRef})
	addedRef := archive.BlobRef{Hash: f.seedAssets(t, added)[0], Size: int64(len(added))}
	assertBundle(base, []archive.BlobRef{addedRef}, []archive.BlobRef{oldRef, addedRef})
	if !reflect.DeepEqual(existing, privateFiles()) {
		t.Fatal("维护、GC 或备份改动了私有对象")
	}
}
