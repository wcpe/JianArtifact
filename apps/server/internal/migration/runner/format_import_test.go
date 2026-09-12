package runner_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func TestRunnerImportsPyPIWithSimpleMetadata(t *testing.T) {
	mig, assets, repos, _, db := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	metadata := domain.NewFormatMetadataService(assets, repository.NewRepoRepo(db), repository.NewFormatMetadataRepo(db), nil)
	root := writeFormatBundle(t, "pypi-source", "pypi", "pypi/packages/demo/demo-1.0.0-py3-none-any.whl", []byte("wheel"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	files, err := metadata.PyPIFiles("pypi-source", "demo")
	if err != nil || len(files) != 1 || files[0].Filename != "demo-1.0.0-py3-none-any.whl" {
		t.Fatalf("迁移后 PyPI simple 元数据不可读：files=%+v err=%v", files, err)
	}
}

func TestRunnerImportsNuGetWithRegistrationMetadata(t *testing.T) {
	mig, assets, repos, _, db := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	metadata := domain.NewFormatMetadataService(assets, repository.NewRepoRepo(db), repository.NewFormatMetadataRepo(db), nil)
	root := writeFormatBundle(t, "nuget-source", "nuget", "packages/demo.1.0.0.nupkg", testNuGetPackage(t, "Demo", "1.0.0"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	packages, err := metadata.NuGetPackages("nuget-source", "demo")
	if err != nil || len(packages) != 1 || packages[0].Version != "1.0.0" {
		t.Fatalf("迁移后 NuGet registration 元数据不可读：packages=%+v err=%v", packages, err)
	}
}

func TestRunnerImportsCargoCrateAndSparseIndex(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	cargo := domain.NewCargoService(assets, repos)
	root := writeCargoBundle(t, "cargo-source", "demo", "1.0.0", []byte("cargo crate"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	index, err := cargo.Index(context.Background(), "cargo-source", "demo")
	if err != nil || !bytes.Contains(index, []byte(`"vers":"1.0.0"`)) || !bytes.Contains(index, []byte(`"yanked":true`)) {
		t.Fatalf("迁移后 Cargo sparse 索引不可读：index=%s err=%v", index, err)
	}
	_, rc, err := cargo.Download(context.Background(), "cargo-source", "demo", "1.0.0")
	if err != nil {
		t.Fatalf("迁移后 Cargo crate 不可下载：%v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil || string(body) != "cargo crate" {
		t.Fatalf("迁移后 Cargo crate 不正确：body=%q err=%v", body, err)
	}
}

func TestRunnerImportsOCIManifestBlobsAndTag(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	oci := domain.NewOCIService(assets, repos)
	root, manifest := writeOCIBundle(t, "oci-source", "demo", "latest")
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	_, rc, err := oci.ResolveManifest(context.Background(), "oci-source", "demo", "latest")
	if err != nil {
		t.Fatalf("迁移后 OCI tag 不可 pull：%v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, manifest) {
		t.Fatalf("迁移后 OCI manifest 不一致：%s %v", got, err)
	}
}

func TestRunnerImportsNexusOCIPaths(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	oci := domain.NewOCIService(assets, repos)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"repositories":[{"name":"oci-nexus","format":"docker","type":"hosted"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := []byte("layer")
	sum := sha256.Sum256(blob)
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "layers": []map[string]any{{"digest": "sha256:" + hex.EncodeToString(sum[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	writeBundleAsset(t, root, "oci-nexus", "v2/demo/blobs/sha256/"+hex.EncodeToString(sum[:]), blob)
	writeBundleAsset(t, root, "oci-nexus", "v2/demo/manifests/latest", manifest)
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusCompleted)
	_, rc, err := oci.ResolveManifest(context.Background(), "oci-nexus", "demo", "latest")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, manifest) {
		t.Fatalf("Nexus OCI manifest 不一致：%s", got)
	}
}

func TestRunnerRejectsInvalidOCIManifestWithoutVisibleBlob(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"repositories":[{"name":"oci-invalid","format":"docker","type":"hosted"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := []byte("blob")
	sum := sha256.Sum256(blob)
	writeBundleAsset(t, root, "oci-invalid", "oci/blobs/sha256/"+hex.EncodeToString(sum[:]), blob)
	writeBundleAsset(t, root, "oci-invalid", "oci/manifests/demo/"+strings.Repeat("0", 64), []byte(`{"schemaVersion":2,"config":{"digest":"sha256:`+strings.Repeat("1", 64)+`"}}`))
	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	if _, _, err := assets.Get("oci-invalid", "oci/blobs/sha256/"+hex.EncodeToString(sum[:])); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("非法 OCI manifest 不得公开 blob：%v", err)
	}
}

func TestRunnerRejectsInvalidPyPIObjectWithoutBareAsset(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := writeFormatBundle(t, "pypi-invalid", "pypi", "pypi/packages/demo/not-a-distribution.bin", []byte("invalid"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	if _, _, err := assets.Get("pypi-invalid", "pypi/packages/demo/not-a-distribution.bin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("失败的 PyPI 对象不得留下裸 asset，得 %v", err)
	}
}

func TestRunnerRejectsInvalidNuGetObjectWithoutBareAsset(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := writeFormatBundle(t, "nuget-invalid", "nuget", "packages/not-a-package.nupkg", []byte("not-a-zip"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	if _, _, err := assets.Get("nuget-invalid", "packages/not-a-package.nupkg"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("失败的 NuGet 对象不得留下裸 asset，得 %v", err)
	}
}

func TestRunnerRejectsUnsafeNuGetArchiveWithoutBareAsset(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := writeFormatBundle(t, "nuget-unsafe", "nuget", "packages/demo.1.0.0.nupkg", unsafeNuGetPackage(t))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	if _, _, err := assets.Get("nuget-unsafe", "packages/demo.1.0.0.nupkg"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("恶意 NuGet 对象不得留下裸 asset，得 %v", err)
	}
}

func TestRunnerRejectsInvalidCargoCrateWithoutBareAsset(t *testing.T) {
	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := writeFormatBundle(t, "cargo-invalid", "cargo", "cargo/crates/demo/1.0.0/not-demo.crate", []byte("crate"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	if _, _, err := assets.Get("cargo-invalid", "cargo/crates/demo/1.0.0/not-demo.crate"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("非法 Cargo crate 不得留下裸 asset，得 %v", err)
	}
}

func TestRunnerRollsBackCargoCrateWhenIndexWriteFails(t *testing.T) {
	mig, assets, repos, _, db := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	if _, err := db.Exec("CREATE TRIGGER fail_cargo_index BEFORE INSERT ON asset WHEN NEW.path LIKE 'cargo/index/%' BEGIN SELECT RAISE(ABORT, '模拟索引失败'); END"); err != nil {
		t.Fatal(err)
	}
	root := writeFormatBundle(t, "cargo-index-fail", "cargo", "cargo/crates/demo/1.0.0/demo-1.0.0.crate", []byte("crate"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	if _, _, err := assets.Get("cargo-index-fail", "cargo/crates/demo/1.0.0/demo-1.0.0.crate"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("索引写入失败不得留下 Cargo crate，得 %v", err)
	}
}

func TestRunnerRollsBackCreatedRepositoriesAfterLaterObjectFails(t *testing.T) {
	mig, _, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	root := t.TempDir()
	manifest := `{"repositories":[{"name":"a-created","format":"raw","type":"hosted"},{"name":"z-invalid","format":"pypi","type":"hosted"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBundleAsset(t, root, "a-created", "ok.bin", []byte("ok"))
	writeBundleAsset(t, root, "z-invalid", "pypi/packages/demo/not-a-distribution.bin", []byte("invalid"))

	result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mig.Start(result.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
	for _, name := range []string{"a-created", "z-invalid"} {
		if _, err := repos.Get(name); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("失败任务不得留下本轮新仓库 %s，得 %v", name, err)
		}
	}
}

func TestRunnerRollsBackFormatAssetWhenMetadataWriteFails(t *testing.T) {
	tests := []struct {
		name   string
		format string
		repo   string
		source string
		target string
		body   func(*testing.T) []byte
	}{
		{
			name:   "pypi",
			format: "pypi",
			repo:   "pypi-metadata-fail",
			source: "pypi/packages/demo/demo-1.0.0-py3-none-any.whl",
			target: "pypi/packages/demo/demo-1.0.0-py3-none-any.whl",
			body:   func(*testing.T) []byte { return []byte("wheel") },
		},
		{
			name:   "nuget",
			format: "nuget",
			repo:   "nuget-metadata-fail",
			source: "packages/demo.1.0.0.nupkg",
			target: "nuget/demo/1.0.0/demo.1.0.0.nupkg",
			body:   func(t *testing.T) []byte { return testNuGetPackage(t, "Demo", "1.0.0") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mig, assets, repos, _, db := setup(t)
			repos.SetEnabledFormats(formats.New(formats.Known...))
			trigger := "CREATE TRIGGER fail_format_metadata BEFORE INSERT ON format_metadata WHEN NEW.format = '" + tt.format + "' BEGIN SELECT RAISE(ABORT, '模拟元数据失败'); END"
			if _, err := db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			root := writeFormatBundle(t, tt.repo, tt.format, tt.source, tt.body(t))
			result, err := mig.Discover(context.Background(), domain.MigrationDiscoverInput{SourceType: repository.MigrationSourceOfflineBundle, SourceConfig: map[string]any{"path": root}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mig.Start(result.Task.ID, nil); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, mig, result.Task.ID, repository.MigrationStatusFailed)
			if _, _, err := assets.Get(tt.repo, tt.target); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("元数据失败不得留下裸 asset，得 %v", err)
			}
		})
	}
}

func writeFormatBundle(t *testing.T, repoName, format, path string, body []byte) string {
	t.Helper()
	root := t.TempDir()
	manifest := `{"repositories":[{"name":"` + repoName + `","format":"` + format + `","type":"hosted"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBundleAsset(t, root, repoName, path, body)
	return root
}

func writeBundleAsset(t *testing.T, root, repoName, path string, body []byte) {
	t.Helper()
	file := filepath.Join(root, "content", repoName, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCargoBundle(t *testing.T, repoName, name, version string, crate []byte) string {
	t.Helper()
	root := writeFormatBundle(t, repoName, "cargo", "cargo/crates/"+name+"/"+version+"/"+name+"-"+version+".crate", crate)
	sum := sha256.Sum256(crate)
	indexLine, err := json.Marshal(map[string]any{
		"name": name, "vers": version, "deps": []any{}, "cksum": hex.EncodeToString(sum[:]), "features": map[string]any{}, "yanked": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(root, "content", repoName, "de", "mo", name)
	if err := os.MkdirAll(filepath.Dir(index), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, append(indexLine, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeOCIBundle(t *testing.T, repo, image, tag string) (string, []byte) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"repositories":[{"name":"`+repo+`","format":"docker","type":"hosted"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	config, layer := []byte("config"), []byte("layer")
	configSum, layerSum := sha256.Sum256(config), sha256.Sum256(layer)
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "config": map[string]any{"digest": "sha256:" + hex.EncodeToString(configSum[:])}, "layers": []map[string]any{{"digest": "sha256:" + hex.EncodeToString(layerSum[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	manifestSum := sha256.Sum256(manifest)
	writeBundleAsset(t, root, repo, "oci/blobs/sha256/"+hex.EncodeToString(configSum[:]), config)
	writeBundleAsset(t, root, repo, "oci/blobs/sha256/"+hex.EncodeToString(layerSum[:]), layer)
	writeBundleAsset(t, root, repo, "oci/manifests/"+image+"/"+hex.EncodeToString(manifestSum[:]), manifest)
	writeBundleAsset(t, root, repo, "oci/tags/"+image+"/"+tag, []byte("sha256:"+hex.EncodeToString(manifestSum[:])))
	return root, manifest
}

func testNuGetPackage(t *testing.T, id, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("package.nuspec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, `<package><metadata><id>`+id+`</id><version>`+version+`</version></metadata></package>`); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unsafeNuGetPackage(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "invalid"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
