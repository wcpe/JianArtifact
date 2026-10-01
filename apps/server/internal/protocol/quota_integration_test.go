package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/formats"
	"github.com/wcpe/jianartifact/apps/server/internal/metrics"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// protocolQuotaHarness 是按真实装配构造的协议层发布环境（FR-41）：
// SQLite + blobstore + 域服务 + 真实路由 + 真实仓库存储配额服务（预检与提交点都已接线）。
type protocolQuotaHarness struct {
	router   *gin.Engine
	blobRoot string
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
	assetSvc *domain.AssetService
	quota    *domain.RepoQuotaService
	registry *metrics.Registry
}

func newProtocolQuotaHarness(t *testing.T) *protocolQuotaHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dataDir, "jianartifact.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移数据库：%v", err)
	}
	blobRoot := filepath.Join(dataDir, "blobs")
	if err := os.MkdirAll(blobRoot, 0o750); err != nil {
		t.Fatalf("创建 blob 根目录：%v", err)
	}
	blobs := blobstore.NewStore(blobRoot)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	users := repository.NewUserRepo(db)
	mutator, err := domain.NewAssetMutationCoordinator(db, blobs)
	if err != nil {
		t.Fatalf("创建资产协调器：%v", err)
	}
	assetSvc := domain.NewAssetService(repos, assets, blobs, nil)
	assetSvc.SetMutationCoordinator(mutator)
	repoSvc := domain.NewRepositoryService(repos, repository.NewAclRepo(db), assets, domain.NewSettingService(repository.NewSettingRepo(db)), users)

	quota := domain.NewRepoQuotaService(repos, assets)
	registry := metrics.New()
	quota.SetRejectionRecorder(registry)
	assetSvc.SetStorageQuotaCheck(quota.CheckCommit)

	raw := NewRawHandler(assetSvc, repoSvc)
	raw.SetQuotaGuard(quota)
	maven := NewMavenHandler(raw)
	npm := NewNpmHandler(raw, nil, nil, "")
	metadata := domain.NewFormatMetadataService(assetSvc, repos, repository.NewFormatMetadataRepo(db))
	pypi := NewPypiHandler(raw, metadata, "")
	nuget := NewNuGetHandler(raw, metadata, "")
	cargo := NewCargoHandler(raw, domain.NewCargoService(assetSvc, repoSvc), "")
	oci := NewOCIHandler(raw, domain.NewOCIService(assetSvc, repoSvc))
	dispatcher := NewDispatcher(repoSvc, raw, maven, formats.New("raw", "maven", "npm", "docker", "cargo", "pypi", "nuget"))

	router := gin.New()
	// 以管理员主体直投：authorize 对管理员直接放行，测试只关注配额闸门本身。
	router.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 1, Username: "quota-tester", Role: "admin"})
		c.Next()
	})
	RegisterRoutes(router, dispatcher)
	RegisterNpmRoutes(router, npm)
	RegisterOCIRoutes(router, oci)
	RegisterCargoRoutes(router, cargo)
	RegisterPypiRoutes(router, pypi)
	RegisterNuGetRoutes(router, nuget)

	return &protocolQuotaHarness{router: router, blobRoot: blobRoot, repos: repos, assets: assets, assetSvc: assetSvc, quota: quota, registry: registry}
}

// createRepo 建一个 hosted 仓库并写入配额上限（0 = 不限）。
func (h *protocolQuotaHarness) createRepo(t *testing.T, name, format string, limits domain.RepoQuotaLimits) {
	t.Helper()
	encoded, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{QuotaBytes: limits.Bytes, QuotaAssets: limits.Assets})
	if err != nil {
		t.Fatalf("编码仓库配置：%v", err)
	}
	if _, err := h.repos.Create(name, format, "hosted", "public", encoded); err != nil {
		t.Fatalf("创建仓库：%v", err)
	}
}

// usage 返回仓库当前的件数与逻辑字节占用。
func (h *protocolQuotaHarness) usage(t *testing.T, name string) (int64, int64) {
	t.Helper()
	repo, err := h.repos.GetByName(name)
	if err != nil {
		t.Fatalf("读取仓库：%v", err)
	}
	count, size, err := h.assets.CountAndSizeByRepo(repo.ID)
	if err != nil {
		t.Fatalf("读取占用：%v", err)
	}
	return count, size
}

// do 发一次请求并返回响应与响应体文本。
func (h *protocolQuotaHarness) do(t *testing.T, method, target, contentType string, body []byte) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

// rejectionCount 读取当前 reason=quota 的拒绝计数（经真实渲染路径解析）。
func (h *protocolQuotaHarness) rejectionCount(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	metrics.NewExposition(h.registry, nil).WritePrometheus(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, `jianartifact_publish_rejections_total{reason="quota"}`) {
			return strings.TrimSpace(strings.TrimPrefix(line, `jianartifact_publish_rejections_total{reason="quota"}`))
		}
	}
	return "0"
}

// TestProtocolPublishRejectsOverQuotaWith429 覆盖全部七个发布协议：越限必须被拒（429
// quota_exceeded / OCI 的 DENIED）且仓库占用不变——任何一条漏掉都是绕过通道。
func TestProtocolPublishRejectsOverQuotaWith429(t *testing.T) {
	h := newProtocolQuotaHarness(t)
	const quotaBytes = 100
	// 仓库名 → 仓库格式（OCI 的格式名是 docker）。
	for name, format := range map[string]string{
		"raw-quota": "raw", "maven-quota": "maven", "npm-quota": "npm",
		"oci-quota": "docker", "pypi-quota": "pypi", "nuget-quota": "nuget", "cargo-quota": "cargo",
	} {
		h.createRepo(t, name, format, domain.RepoQuotaLimits{Bytes: quotaBytes})
	}
	oversize := bytes.Repeat([]byte("x"), 400)

	pypiContentType, pypiBody := pypiUploadBody(t, oversize)
	npmBody, err := json.Marshal(map[string]any{
		"_attachments": map[string]any{
			"demo-1.0.0.tgz": map[string]any{"data": base64.StdEncoding.EncodeToString(oversize)},
		},
		"versions": map[string]any{"1.0.0": map[string]any{"dist": map[string]any{"tarball": "demo-1.0.0.tgz"}}},
	})
	if err != nil {
		t.Fatalf("构造 npm 发布体：%v", err)
	}

	cases := []struct {
		name        string
		protocol    string
		method      string
		target      string
		contentType string
		body        []byte
		wantCode    string
	}{
		{
			name: "raw 协议发布", protocol: "raw",
			method: http.MethodPut, target: "/repository/raw-quota/app.bin",
			contentType: "application/octet-stream", body: oversize, wantCode: "quota_exceeded",
		},
		{
			name: "maven 协议发布", protocol: "maven",
			method: http.MethodPut, target: "/repository/maven-quota/com/example/demo/1.0.0/demo-1.0.0.jar",
			contentType: "application/java-archive", body: oversize, wantCode: "quota_exceeded",
		},
		{
			name: "npm 发布", protocol: "npm",
			method: http.MethodPut, target: "/npm/npm-quota/demo",
			contentType: "application/json", body: npmBody, wantCode: "quota_exceeded",
		},
		{
			name: "oci blob 直传", protocol: "oci",
			method: http.MethodPut, target: "/v2/oci-quota/demo/blobs/uploads/?digest=sha256:" + strings.Repeat("a", 64),
			contentType: "application/octet-stream", body: oversize, wantCode: "DENIED",
		},
		{
			name: "pypi legacy 上传（长度未知，流式早拒）", protocol: "pypi",
			method: http.MethodPost, target: "/pypi/pypi-quota/legacy",
			contentType: pypiContentType, body: pypiBody, wantCode: "quota_exceeded",
		},
		{
			name: "nuget push（长度未知，流式早拒）", protocol: "nuget",
			method: http.MethodPut, target: "/nuget/nuget-quota/api/v2/package",
			contentType: "application/octet-stream", body: oversize, wantCode: "quota_exceeded",
		},
		{
			name: "cargo publish", protocol: "cargo",
			method: http.MethodPut, target: "/cargo/cargo-quota/api/v1/crates/new",
			contentType: "application/octet-stream", body: cargoPublishBody(t, "demo", "1.0.0", len(oversize)), wantCode: "quota_exceeded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.rejectionCount(t)
			rec, body := h.do(t, tc.method, tc.target, tc.contentType, tc.body)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("%s 越限必须以 429 拒绝，得 %d：%s", tc.protocol, rec.Code, body)
			}
			if !strings.Contains(body, `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("%s 拒绝响应应带错误码 %s，得 %s", tc.protocol, tc.wantCode, body)
			}
			count, size := h.usage(t, tc.protocol+"-quota")
			if count != 0 || size != 0 {
				t.Fatalf("%s 被拒后不得留下任何制品，得 count=%d size=%d", tc.protocol, count, size)
			}
			if got := h.rejectionCount(t); got == before {
				t.Fatalf("%s 拒绝必须累加 publish_rejections_total{reason=\"quota\"}", tc.protocol)
			}
		})
	}

	if got := h.rejectionCount(t); got != "7" {
		t.Fatalf("七个协议各应累加一次拒绝计数，得 %s", got)
	}
}

// TestProtocolPublishRejectionKeepsExistingContent 越限拒绝不得破坏既有内容：
// 第一次上传成功，第二次越限被拒，既有制品与仓库占用保持不变。
func TestProtocolPublishRejectionKeepsExistingContent(t *testing.T) {
	h := newProtocolQuotaHarness(t)
	h.createRepo(t, "raw-keep", "raw", domain.RepoQuotaLimits{Bytes: 100})

	rec, body := h.do(t, http.MethodPut, "/repository/raw-keep/app.bin", "application/octet-stream", bytes.Repeat([]byte("a"), 60))
	if rec.Code != http.StatusCreated {
		t.Fatalf("限内上传应成功，得 %d：%s", rec.Code, body)
	}
	rec, body = h.do(t, http.MethodPut, "/repository/raw-keep/second.bin", "application/octet-stream", bytes.Repeat([]byte("b"), 60))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("越限上传应以 429 拒绝，得 %d：%s", rec.Code, body)
	}

	rec, body = h.do(t, http.MethodGet, "/repository/raw-keep/app.bin", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("既有制品必须仍可读取，得 %d：%s", rec.Code, body)
	}
	if body != strings.Repeat("a", 60) {
		t.Fatalf("既有制品内容不得被破坏，得 %q", body)
	}
	count, size := h.usage(t, "raw-keep")
	if count != 1 || size != 60 {
		t.Fatalf("被拒后占用应保持 1 件 / 60 字节，得 %d / %d", count, size)
	}
}

// TestProtocolPublishRejectionMessageHasUsageAndNoFilesystemPath 拒绝响应必须给出可读的
// 「当前占用 / 上限」且不得回显文件系统路径。
func TestProtocolPublishRejectionMessageHasUsageAndNoFilesystemPath(t *testing.T) {
	h := newProtocolQuotaHarness(t)
	h.createRepo(t, "raw-message", "raw", domain.RepoQuotaLimits{Bytes: 10})
	rec, body := h.do(t, http.MethodPut, "/repository/raw-message/app.bin", "application/octet-stream", bytes.Repeat([]byte("a"), 20))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("越限应以 429 拒绝，得 %d：%s", rec.Code, body)
	}
	for _, want := range []string{"当前占用", "上限", "存储配额已超限", "quota_exceeded"} {
		if !strings.Contains(body, want) {
			t.Fatalf("拒绝响应应含 %q，得 %s", want, body)
		}
	}
	if strings.Contains(body, string(filepath.Separator)) || strings.Contains(body, "blobs") {
		t.Fatalf("拒绝响应不得回显文件系统路径，得 %s", body)
	}
}

// pypiUploadBody 构造 twine legacy 上传的 multipart 请求体，返回 Content-Type 与请求体。
func pypiUploadBody(t *testing.T, file []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for key, value := range map[string]string{"name": "demo", "version": "1.0.0", "filename": "demo-1.0.0.tar.gz"} {
		if err := w.WriteField(key, value); err != nil {
			t.Fatalf("写入表单字段：%v", err)
		}
	}
	part, err := w.CreateFormFile("content", "demo-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("创建文件字段：%v", err)
	}
	if _, err := part.Write(file); err != nil {
		t.Fatalf("写入文件内容：%v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart：%v", err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

// cargoPublishBody 构造 cargo publish 的帧：metadataLen(4) + metadata + crateLen(4) + crate。
// crate 字节长度诚实声明但内容可省略：配额闸门在读取 crate 之前判定。
func cargoPublishBody(t *testing.T, name, version string, crateLen int) []byte {
	t.Helper()
	metadata, err := json.Marshal(map[string]any{"name": name, "vers": version})
	if err != nil {
		t.Fatalf("构造 cargo 元数据：%v", err)
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(metadata))); err != nil {
		t.Fatalf("写入元数据长度：%v", err)
	}
	buf.Write(metadata)
	if err := binary.Write(&buf, binary.LittleEndian, uint32(crateLen)); err != nil {
		t.Fatalf("写入 crate 长度：%v", err)
	}
	buf.Write(bytes.Repeat([]byte("c"), crateLen))
	return buf.Bytes()
}

// TestRawStreamingUploadAbortsAtQuotaLimit 长度未知的流式上传：限额感知读取器在到达
// 允许上限处即中断，不允许"先写满再拒绝"，且磁盘上不留下超限内容。
func TestRawStreamingUploadAbortsAtQuotaLimit(t *testing.T) {
	h := newProtocolQuotaHarness(t)
	h.createRepo(t, "raw-stream", "raw", domain.RepoQuotaLimits{Bytes: 100})

	before := h.rejectionCount(t)
	req := httptest.NewRequest(http.MethodPut, "/repository/raw-stream/app.bin", bytes.NewReader(bytes.Repeat([]byte("x"), 400)))
	// 模拟 chunked 上传：ContentLength 未知，触发限额感知读取器路径。
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("流式越限必须以 429 中断，得 %d：%s", rec.Code, rec.Body.String())
	}
	if got := h.rejectionCount(t); got == before {
		t.Fatal("流式早拒必须累加 publish_rejections_total{reason=\"quota\"}")
	}
	count, size := h.usage(t, "raw-stream")
	if count != 0 || size != 0 {
		t.Fatalf("被中断的流式上传不得留下制品，得 count=%d size=%d", count, size)
	}
	if leftovers := blobFilesOfSize(t, h.blobRoot, 400); leftovers != 0 {
		t.Fatalf("不得落盘超限内容，得 %d 个 400 字节文件", leftovers)
	}
	if leftovers := blobFilesOfSize(t, h.blobRoot, 100); leftovers != 0 {
		t.Fatalf("被拒写入的暂存内容必须由失败路径回收，得 %d 个 100 字节文件", leftovers)
	}
}

// blobFilesOfSize 统计 blob 根目录下大小恰好为 size 的普通文件数量。
func blobFilesOfSize(t *testing.T, root string, size int64) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() == size {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 blob 根目录：%v", err)
	}
	return count
}
