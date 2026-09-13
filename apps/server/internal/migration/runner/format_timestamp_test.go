package runner_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestRunnerOnlineFormatImportersPreserveSourceTimestamps 是 pypi/nuget/cargo/docker
// 四种格式走 MigrationFormatImporter 路径的端到端时间戳回归：真实 sqlite + 假 Nexus 源，
// 断言各格式经 Publish*/putBlob/putManifest 写入的资产行 created_at/updated_at 等于
// 源端 lastModified 转 UTC 的 "YYYY-MM-DD HH:MM:SS"，而不是迁移执行时刻。
// maven/npm/raw 走 PutWithTimestamps 已有 maven_timestamp_test.go 覆盖，此处补齐四格式分支。
func TestRunnerOnlineFormatImportersPreserveSourceTimestamps(t *testing.T) {
	const sourceLastModified = "2021-10-19T02:14:21.187+00:00"
	const wantTimestamp = "2021-10-19 02:14:21"

	// OCI：先上传 blob，再由 manifest 引用发布，tag 指向 manifest digest。
	blobBody := []byte("oci-blob-bytes")
	blobSum := sha256.Sum256(blobBody)
	blobDigest := hex.EncodeToString(blobSum[:])
	manifestBody := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + blobDigest + `","size":` + strconv.Itoa(len(blobBody)) + `},"layers":[]}`)
	manifestSum := sha256.Sum256(manifestBody)
	manifestDigest := hex.EncodeToString(manifestSum[:])

	// NuGet：合法 nupkg（zip + nuspec），id/version 由导入器解析。
	var nupkg bytes.Buffer
	zw := zip.NewWriter(&nupkg)
	nuspec, err := zw.Create("Demo.Package.nuspec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nuspec.Write([]byte(`<package><metadata><id>Demo.Package</id><version>1.0.0</version></metadata></package>`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	bodies := map[string][]byte{
		"pypi.whl":                 []byte("pypi-wheel-bytes"),
		"demo-package.1.0.0.nupkg": nupkg.Bytes(),
		"demo-crate-1.0.0.crate":   []byte("crate-bytes"),
		"blob":                     blobBody,
		"manifest":                 manifestBody,
		"tag":                      []byte("sha256:" + manifestDigest),
	}
	// downloadUrl 必须是绝对地址（与真实 Nexus 一致），由 handler 按请求 host 拼接。
	assetPathsByRepo := map[string][]map[string]string{
		"pypi-releases": {
			{"path": "demo/1.0.0/demo-1.0.0-py3-none-any.whl", "dl": "pypi.whl", "contentType": "application/zip"},
		},
		"nuget-releases": {
			{"path": "demo-package/1.0.0/demo-package.1.0.0.nupkg", "dl": "demo-package.1.0.0.nupkg", "contentType": "application/octet-stream"},
		},
		"cargo-releases": {
			{"path": "cargo/crates/demo-crate/1.0.0/demo-crate-1.0.0.crate", "dl": "demo-crate-1.0.0.crate", "contentType": "application/octet-stream"},
		},
		"docker-releases": {
			{"path": "oci/blobs/sha256/" + blobDigest, "dl": "blob", "contentType": "application/octet-stream"},
			{"path": "oci/manifests/demo-image/" + manifestDigest, "dl": "manifest", "contentType": "application/vnd.oci.image.manifest.v1+json"},
			{"path": "oci/tags/demo-image/latest", "dl": "tag", "contentType": "text/plain"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/service/rest/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		host := "http://" + r.Host
		specs, ok := assetPathsByRepo[r.URL.Query().Get("repository")]
		if !ok {
			specs = []map[string]string{}
		}
		items := make([]map[string]string, 0, len(specs))
		for _, spec := range specs {
			items = append(items, map[string]string{
				"path":         spec["path"],
				"downloadUrl":  host + "/dl/" + spec["dl"],
				"contentType":  spec["contentType"],
				"lastModified": sourceLastModified,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/dl/"):]
		body, ok := bodies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)

	mig, assets, repos, _, _ := setup(t)
	repos.SetEnabledFormats(formats.New(formats.Known...))
	for _, spec := range []struct{ name, format string }{
		{"pypi-releases", "pypi"},
		{"nuget-releases", "nuget"},
		{"cargo-releases", "cargo"},
		{"docker-releases", "docker"},
	} {
		if _, err := repos.Create(spec.name, spec.format, "hosted", "private", "", repository.RepositoryConfig{}); err != nil {
			t.Fatalf("创建 %s：%v", spec.name, err)
		}
	}

	created, err := mig.Create(domain.MigrationCreateInput{
		SourceType:   repository.MigrationSourceOnlineREST,
		SourceConfig: map[string]any{"url": source.URL, "allowPrivateSource": true},
		PlanJSON:     `{"repositories":[{"name":"pypi-releases","format":"pypi"},{"name":"nuget-releases","format":"nuget"},{"name":"cargo-releases","format":"cargo"},{"name":"docker-releases","format":"docker"}]}`,
	})
	if err != nil {
		t.Fatalf("Create：%v", err)
	}
	if _, err := mig.Start(created.ID, nil); err != nil {
		t.Fatalf("Start：%v", err)
	}
	task := waitStatus(t, mig, created.ID, repository.MigrationStatusCompleted)

	report := struct {
		Copied  int `json:"copied"`
		Skipped int `json:"skipped"`
		Failed  int `json:"failed"`
	}{}
	if err := json.Unmarshal([]byte(task.ReportJSON), &report); err != nil {
		t.Fatalf("解析 report：%v", report.Failed)
	}
	if report.Copied != 6 || report.Failed != 0 {
		t.Fatalf("四格式资产应全部复制成功：report=%+v", report)
	}

	// 每个格式最终写库的资产行都必须携带源端时间。
	expectSourceTime := []struct{ repo, path string }{
		{"pypi-releases", "pypi/packages/demo/demo-1.0.0-py3-none-any.whl"},
		{"nuget-releases", "nuget/demo.package/1.0.0/demo-package.1.0.0.nupkg"},
		{"cargo-releases", "cargo/crates/demo-crate/1.0.0/demo-crate-1.0.0.crate"},
		{"docker-releases", "oci/blobs/sha256/" + blobDigest},
		{"docker-releases", "oci/manifests/demo-image/" + manifestDigest},
		{"docker-releases", "oci/tags/demo-image/latest"},
	}
	for _, item := range expectSourceTime {
		asset, rc, err := assets.Get(item.repo, item.path)
		if err != nil {
			t.Fatalf("读取 %s/%s：%v", item.repo, item.path, err)
		}
		_ = rc.Close()
		if asset.CreatedAt != wantTimestamp || asset.UpdatedAt != wantTimestamp {
			t.Errorf("%s/%s created_at=%q updated_at=%q，期望源端时间 %q", item.repo, item.path, asset.CreatedAt, asset.UpdatedAt, wantTimestamp)
		}
	}

	// cargo 索引是合并衍生行，保持本地时间语义：只断言存在且时间非空。
	index, rc, err := assets.Get("cargo-releases", "cargo/index/de/mo/demo-crate")
	if err != nil {
		t.Fatalf("读取 cargo 索引：%v", err)
	}
	_ = rc.Close()
	if index.CreatedAt == "" || index.UpdatedAt == "" {
		t.Fatalf("cargo 索引行时间不应为空：created_at=%q updated_at=%q", index.CreatedAt, index.UpdatedAt)
	}
}
