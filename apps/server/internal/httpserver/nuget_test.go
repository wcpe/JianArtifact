package httpserver_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

var errNuGetReadBufferTooLarge = errors.New("NuGet 上传读取缓冲区过大")

// boundedNuGetReader 用于确认协议层不会将整个 nupkg 聚合到单个读取缓冲区。
type boundedNuGetReader struct {
	r       io.Reader
	maximum int
	seen    int
}

func (r *boundedNuGetReader) Read(p []byte) (int, error) {
	if len(p) > r.maximum {
		return 0, errNuGetReadBufferTooLarge
	}
	if len(p) > r.seen {
		r.seen = len(p)
	}
	return r.r.Read(p)
}

func nugetPackage(t *testing.T, id, version string, extra func(*zip.Writer) error) []byte {
	t.Helper()
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	w, err := zw.Create(id + ".nuspec")
	if err != nil {
		t.Fatal(err)
	}
	nuspec := `<package><metadata><id>` + id + `</id><version>` + version + `</version></metadata></package>`
	if _, err := w.Write([]byte(nuspec)); err != nil {
		t.Fatal(err)
	}
	if extra != nil {
		if err := extra(zw); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestNuGetPublishPolicyAuditRecordsSuccessAndRejection(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-policy-audit", "nuget", "hosted")
	const username = "nuget-policy-audit-user"
	const password = "nuget-policy-audit-password"
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", admin, api.CreateUserRequest{Username: username, Password: password}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/nuget-policy-audit/acl", admin,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予 write ACL 状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/users/"+strconv.FormatInt(user.Id, 10)+"/publish-policies/nuget-policy-audit", admin,
		map[string]any{"allowedPrefixes": []string{"nuget/allowed"}}, nil); code != http.StatusOK {
		t.Fatalf("保存发布策略状态码=%d", code)
	}
	basic := basicUserPasswordHeader(username, password)
	blocked := nugetPackage(t, "Blocked.Package", "1.0.0", nil)
	if rec := e.rawReq(http.MethodPut, "/nuget/nuget-policy-audit/api/v2/package", basic, "application/octet-stream", blocked); rec.Code != http.StatusForbidden {
		t.Fatalf("越前缀发布状态码=%d，期望 403", rec.Code)
	}
	allowed := nugetPackage(t, "Allowed", "1.0.0", nil)
	if rec := e.rawReq(http.MethodPut, "/nuget/nuget-policy-audit/api/v2/package", basic, "application/octet-stream", allowed); rec.Code != http.StatusCreated {
		t.Fatalf("允许前缀发布状态码=%d，期望 201：%s", rec.Code, rec.Body.String())
	}
	assertProtocolAudit(t, e, username, user.Id, "nuget.publish", "rejected", "publish_path_denied")
	assertProtocolAudit(t, e, username, user.Id, "nuget.publish", "ok", "size="+strconv.FormatInt(int64(len(allowed)), 10))
}

func TestNuGetHostedV3PushAndRestore(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-hosted", "nuget", "hosted")
	payload := nugetPackage(t, "Demo.Package", "1.2.3", nil)
	push := e.rawReq(http.MethodPut, "/nuget/nuget-hosted/api/v2/package", "Bearer "+admin, "application/octet-stream", payload)
	if push.Code != http.StatusCreated {
		t.Fatalf("NuGet push 状态码=%d，响应=%s", push.Code, push.Body.String())
	}
	if duplicate := e.rawReq(http.MethodPut, "/nuget/nuget-hosted/api/v2/package", "Bearer "+admin, "application/octet-stream", payload); duplicate.Code != http.StatusConflict {
		t.Fatalf("重复 NuGet 版本状态码=%d，期望 409", duplicate.Code)
	}
	apiKeyPayload := nugetPackage(t, "ApiKey.Package", "1.0.0", nil)
	apiKeyReq := httptest.NewRequest(http.MethodPut, "/nuget/nuget-hosted/api/v2/package", bytes.NewReader(apiKeyPayload))
	apiKeyReq.Host = "127.0.0.1"
	apiKeyReq.RemoteAddr = "127.0.0.1:1234"
	apiKeyReq.Header.Set("X-NuGet-ApiKey", admin)
	apiKeyReq.Header.Set("Content-Type", "application/octet-stream")
	apiKeyRec := httptest.NewRecorder()
	e.h.ServeHTTP(apiKeyRec, apiKeyReq)
	if apiKeyRec.Code != http.StatusCreated {
		t.Fatalf("NuGet X-NuGet-ApiKey 发布状态码=%d，响应=%s", apiKeyRec.Code, apiKeyRec.Body.String())
	}

	index := e.rawReq(http.MethodGet, "/nuget/nuget-hosted/v3/index.json", "Bearer "+admin, "", nil)
	if index.Code != http.StatusOK || !bytes.Contains(index.Body.Bytes(), []byte("PackageBaseAddress/3.0.0")) || !bytes.Contains(index.Body.Bytes(), []byte("PackagePublish/2.0.0")) {
		t.Fatalf("NuGet service index 不完整：状态码=%d，响应=%s", index.Code, index.Body.String())
	}
	var service struct {
		Resources []map[string]string `json:"resources"`
	}
	if err := json.Unmarshal(index.Body.Bytes(), &service); err != nil {
		t.Fatal(err)
	}
	for _, resource := range service.Resources {
		if bytes.Contains([]byte(resource["@id"]), []byte("upstream")) {
			t.Fatalf("NuGet service index 泄漏上游：%v", resource)
		}
	}

	versions := e.rawReq(http.MethodGet, "/nuget/nuget-hosted/v3-flatcontainer/demo.package/index.json", "Bearer "+admin, "", nil)
	if versions.Code != http.StatusOK || !bytes.Contains(versions.Body.Bytes(), []byte("1.2.3")) {
		t.Fatalf("NuGet flat index 不一致：状态码=%d，响应=%s", versions.Code, versions.Body.String())
	}
	path := "/nuget/nuget-hosted/v3-flatcontainer/demo.package/1.2.3/demo.package.1.2.3.nupkg"
	download := e.rawReq(http.MethodGet, path, "Bearer "+admin, "", nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), payload) {
		t.Fatalf("NuGet 包下载不一致：状态码=%d，大小=%d", download.Code, download.Body.Len())
	}
	registration := e.rawReq(http.MethodGet, "/nuget/nuget-hosted/v3-registration5-gz-semver2/demo.package/index.json", "Bearer "+admin, "", nil)
	if registration.Code != http.StatusOK || !bytes.Contains(registration.Body.Bytes(), []byte("1.2.3")) || !bytes.Contains(registration.Body.Bytes(), []byte("catalogEntry")) {
		t.Fatalf("NuGet registration 不一致：状态码=%d，响应=%s", registration.Code, registration.Body.String())
	}
	search := e.rawReq(http.MethodGet, "/nuget/nuget-hosted/query?q=Demo.Package", "Bearer "+admin, "", nil)
	if search.Code != http.StatusOK || !bytes.Contains(search.Body.Bytes(), []byte("Demo.Package")) {
		t.Fatalf("NuGet 精确搜索不一致：状态码=%d，响应=%s", search.Code, search.Body.String())
	}
}

func TestNuGetRegistrationProjectsNuSpecDependencies(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-registration", "nuget", "hosted")
	var payload bytes.Buffer
	writer := zip.NewWriter(&payload)
	entry, err := writer.Create("Demo.Package.nuspec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`<package><metadata><id>Demo.Package</id><version>1.2.3</version><dependencies><group targetFramework="net8.0"><dependency id="Child.Package" version="[2.0.0]" /></group></dependencies></metadata></package>`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if push := e.rawReq(http.MethodPut, "/nuget/nuget-registration/api/v2/package", "Bearer "+admin, "application/octet-stream", payload.Bytes()); push.Code != http.StatusCreated {
		t.Fatalf("NuGet push 状态码=%d，响应=%s", push.Code, push.Body.String())
	}

	registration := e.rawReq(http.MethodGet, "/nuget/nuget-registration/v3-registration5-gz-semver2/demo.package/index.json", "Bearer "+admin, "", nil)
	if registration.Code != http.StatusOK {
		t.Fatalf("NuGet registration 状态码=%d，响应=%s", registration.Code, registration.Body.String())
	}
	var document struct {
		Items []struct {
			CatalogEntry struct {
				DependencyGroups []struct {
					TargetFramework string `json:"targetFramework"`
					Dependencies    []struct {
						ID    string `json:"id"`
						Range string `json:"range"`
					} `json:"dependencies"`
				} `json:"dependencyGroups"`
			} `json:"catalogEntry"`
		} `json:"items"`
	}
	if err := json.Unmarshal(registration.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Items) != 1 || len(document.Items[0].CatalogEntry.DependencyGroups) != 1 || document.Items[0].CatalogEntry.DependencyGroups[0].TargetFramework != "net8.0" || len(document.Items[0].CatalogEntry.DependencyGroups[0].Dependencies) != 1 || document.Items[0].CatalogEntry.DependencyGroups[0].Dependencies[0].ID != "Child.Package" || document.Items[0].CatalogEntry.DependencyGroups[0].Dependencies[0].Range != "[2.0.0]" {
		t.Fatalf("registration 未投影 nuspec 依赖：%s", registration.Body.String())
	}
}

func TestNuGetSearchReturnsEmptyResultForUnknownPackage(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-empty-search", "nuget", "hosted")

	res := e.rawReq(http.MethodGet, "/nuget/nuget-empty-search/query?q=missing.package", "Bearer "+admin, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("不存在包的 NuGet 搜索状态码=%d，响应=%s", res.Code, res.Body.String())
	}
	if !bytes.Contains(res.Body.Bytes(), []byte(`"totalHits":0`)) {
		t.Fatalf("不存在包的 NuGet 搜索应返回空结果，响应=%s", res.Body.String())
	}
}

func TestNuGetMultipartPush(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-multipart", "nuget", "hosted")
	pkg := nugetPackage(t, "Multipart.Package", "1.0.0", nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("package", "Multipart.Package.1.0.0.nupkg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(pkg); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/nuget/nuget-multipart/api/v2/package", &body)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("NuGet multipart push 状态码=%d，响应=%s", rec.Code, rec.Body.String())
	}
}

func TestNuGetPushStreamsLargePackageWithoutWholeBodyRead(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-stream", "nuget", "hosted")
	pkg := nugetPackage(t, "Stream.Package", "1.0.0", func(zw *zip.Writer) error {
		for _, name := range []string{"lib/payload-a.bin", "lib/payload-b.bin", "lib/payload-c.bin"} {
			w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := w.Write(bytes.Repeat([]byte("x"), 768<<10)); err != nil {
				return err
			}
		}
		return nil
	})
	reader := &boundedNuGetReader{r: bytes.NewReader(pkg), maximum: 64 << 10}
	req := httptest.NewRequest(http.MethodPut, "/nuget/nuget-stream/api/v2/package", reader)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	req.ContentLength = int64(len(pkg))
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("NuGet 流式发布状态码=%d，响应=%s", rec.Code, rec.Body.String())
	}
	if reader.seen > reader.maximum {
		t.Fatalf("NuGet 单次读取缓冲区=%d，超过上限=%d", reader.seen, reader.maximum)
	}
}

func TestNuGetRejectsMalformedAndMaliciousPackages(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-invalid", "nuget", "hosted")
	for name, body := range map[string][]byte{
		"损坏 ZIP": []byte("not a zip"),
		"恶意路径": nugetPackage(t, "Bad.Package", "1.0.0", func(zw *zip.Writer) error {
			_, err := zw.CreateHeader(&zip.FileHeader{Name: "../escape.txt", Method: zip.Store})
			return err
		}),
	} {
		res := e.rawReq(http.MethodPut, "/nuget/nuget-invalid/api/v2/package", "Bearer "+admin, "application/octet-stream", body)
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s 状态码=%d，期望 400", name, res.Code)
		}
	}
	repo, err := e.repoRepo.GetByName("nuget-invalid")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := e.assetRepo.ListByRepo(repo.ID, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("无效 nupkg 不得写入任何制品，得 %d 个", len(assets))
	}
}

func TestNuGetGroupMergesMembersAndRejectsPush(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "nuget-a", "nuget", "hosted")
	createFormatRepo(t, e, admin, "nuget-b", "nuget", "hosted")
	createFormatRepo(t, e, admin, "nuget-group", "nuget", "group", "nuget-a", "nuget-b")
	packageBody := nugetPackage(t, "Group.Package", "1.0.0", nil)
	if res := e.rawReq(http.MethodPut, "/nuget/nuget-a/api/v2/package", "Bearer "+admin, "application/octet-stream", packageBody); res.Code != http.StatusCreated {
		t.Fatalf("成员 NuGet 发布失败：%d %s", res.Code, res.Body.String())
	}
	versions := e.rawReq(http.MethodGet, "/nuget/nuget-group/v3-flatcontainer/group.package/index.json", "Bearer "+admin, "", nil)
	if versions.Code != http.StatusOK || !bytes.Contains(versions.Body.Bytes(), []byte("1.0.0")) {
		t.Fatalf("NuGet group 未合并：状态码=%d，响应=%s", versions.Code, versions.Body.String())
	}
	if res := e.rawReq(http.MethodPut, "/nuget/nuget-group/api/v2/package", "Bearer "+admin, "application/octet-stream", packageBody); res.Code != http.StatusConflict {
		t.Fatalf("NuGet group push 状态码=%d，期望 409", res.Code)
	}
}

func TestNuGetProxyCachesFlatIndexAndPackage(t *testing.T) {
	var indexHits, packageHits int32
	packageBody := []byte("proxy-nuget-package")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3-flatcontainer/proxy.package/index.json":
			atomic.AddInt32(&indexHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"versions":["1.0.0"]}`))
		case "/v3-flatcontainer/proxy.package/1.0.0/proxy.package.1.0.0.nupkg":
			atomic.AddInt32(&packageHits, 1)
			_, _ = w.Write(packageBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL})
	if _, err := e.repoRepo.Create("nuget-proxy", "nuget", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, "/nuget/nuget-proxy/v3-flatcontainer/proxy.package/index.json", "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte(`"1.0.0"`)) {
			t.Fatalf("NuGet proxy flat index 状态码=%d，响应=%s", res.Code, res.Body.String())
		}
	}
	serviceIndex := e.rawReq(http.MethodGet, "/nuget/nuget-proxy/v3/index.json", "Bearer "+admin, "", nil)
	if serviceIndex.Code != http.StatusOK || bytes.Contains(serviceIndex.Body.Bytes(), []byte(upstream.URL)) {
		t.Fatalf("NuGet proxy service index 必须重写为本地资源：状态码=%d，响应=%s", serviceIndex.Code, serviceIndex.Body.String())
	}
	path := "/nuget/nuget-proxy/v3-flatcontainer/proxy.package/1.0.0/proxy.package.1.0.0.nupkg"
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, path, "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), packageBody) {
			t.Fatalf("NuGet proxy 下载状态码=%d，内容=%q", res.Code, res.Body.Bytes())
		}
	}
	if atomic.LoadInt32(&indexHits) != 1 || atomic.LoadInt32(&packageHits) != 1 {
		t.Fatalf("NuGet proxy 未命中缓存：index=%d package=%d", indexHits, packageHits)
	}
}
