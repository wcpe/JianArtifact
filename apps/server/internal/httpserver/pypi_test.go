package httpserver_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

func pypiSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func pypiUpload(t *testing.T, e *protocolEnv, repo, auth, project, version, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fields := map[string]string{"name": project, "version": version, "filename": filename, "requires_python": ">=3.10"}
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/pypi/"+repo+"/legacy/", &body)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

type pypiMultipartFile struct {
	filename string
	content  []byte
}

func pypiMultipartUpload(t *testing.T, e *protocolEnv, repo, auth string, fields map[string]string, files []pypiMultipartFile) *httptest.ResponseRecorder {
	t.Helper()
	req := pypiMultipartRequest(t, repo, auth, fields, files)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func pypiMultipartRequest(t *testing.T, repo, auth string, fields map[string]string, files []pypiMultipartFile) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		part, err := mw.CreateFormFile("content", file.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/pypi/"+repo+"/legacy/", &body)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func assertNoPyPIProjection(t *testing.T, e *protocolEnv, repo, project string) {
	t.Helper()
	r, err := e.repoRepo.GetByName(repo)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := e.assetRepo.CountByRepo(r.ID, ""); err != nil || count != 0 {
		t.Fatalf("失败上传不应保留资产 count=%d err=%v", count, err)
	}
	if files, err := e.formatMeta.List(r.ID, "pypi", project); err != nil || len(files) != 0 {
		t.Fatalf("失败上传不应保留 PyPI 元数据 files=%+v err=%v", files, err)
	}
}

func TestPypiLegacyUploadStreamsLargeDistribution(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-stream", "pypi", "hosted")
	payload := bytes.Repeat([]byte("x"), 24<<20)
	req := pypiMultipartRequest(t, "pypi-stream", "Bearer "+admin,
		map[string]string{"name": "stream-package", "version": "1.0.0", "filename": "stream_package-1.0.0.tar.gz"},
		[]pypiMultipartFile{{filename: "stream_package-1.0.0.tar.gz", content: payload}})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	runtime.ReadMemStats(&after)
	if rec.Code != http.StatusOK {
		t.Fatalf("大文件 PyPI 发布状态码=%d，响应=%s", rec.Code, rec.Body.String())
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("PyPI 上传额外分配=%d，超过流式阈值=%d", allocated, 8<<20)
	}
	r, err := e.repoRepo.GetByName("pypi-stream")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := e.assetRepo.ListByRepo(r.ID, "", 10, 0)
	if err != nil || len(assets) != 1 || assets[0].BlobHash != pypiSHA256(payload) {
		t.Fatalf("大文件 PyPI 资产不一致 assets=%+v err=%v", assets, err)
	}
}

func TestPypiLegacyUploadRejectsOversizedFieldWithoutProjection(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-field-limit", "pypi", "hosted")
	rec := pypiMultipartUpload(t, e, "pypi-field-limit", "Bearer "+admin,
		map[string]string{"name": strings.Repeat("x", (64<<10)+1), "version": "1.0.0", "filename": "field_limit-1.0.0.tar.gz"},
		[]pypiMultipartFile{{filename: "field_limit-1.0.0.tar.gz", content: []byte("payload")}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超大字段状态码=%d，期望 400：%s", rec.Code, rec.Body.String())
	}
	assertNoPyPIProjection(t, e, "pypi-field-limit", "")
}

func TestPypiLegacyUploadRejectsSecondFileWithoutProjection(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-single-file", "pypi", "hosted")
	rec := pypiMultipartUpload(t, e, "pypi-single-file", "Bearer "+admin,
		map[string]string{"name": "single-file", "version": "1.0.0", "filename": "single_file-1.0.0.tar.gz"},
		[]pypiMultipartFile{
			{filename: "single_file-1.0.0.tar.gz", content: []byte("first")},
			{filename: "single_file-1.0.0-py3-none-any.whl", content: []byte("second")},
		})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("第二文件状态码=%d，期望 400：%s", rec.Code, rec.Body.String())
	}
	assertNoPyPIProjection(t, e, "pypi-single-file", "single-file")
}

func TestPyPIPublishPolicyAuditRecordsSuccessAndRejection(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-policy-audit", "pypi", "hosted")
	const username = "pypi-policy-audit-user"
	const password = "pypi-policy-audit-password"
	var user api.User
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/users", admin, api.CreateUserRequest{Username: username, Password: password}, &user); code != http.StatusCreated {
		t.Fatalf("创建发布账号状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/repositories/pypi-policy-audit/acl", admin,
		api.PutAclRequest{Items: []api.AclEntry{{SubjectId: user.Id, Action: api.AclEntryActionWrite}}}, nil); code != http.StatusOK {
		t.Fatalf("授予 write ACL 状态码=%d", code)
	}
	if code := e.jsonReq(t, http.MethodPut, "/api/v1/users/"+strconv.FormatInt(user.Id, 10)+"/publish-policies/pypi-policy-audit", admin,
		map[string]any{"allowedPrefixes": []string{"pypi/packages/allowed"}}, nil); code != http.StatusOK {
		t.Fatalf("保存发布策略状态码=%d", code)
	}
	basic := basicUserPasswordHeader(username, password)
	if rec := pypiUpload(t, e, "pypi-policy-audit", basic, "blocked", "1.0.0", "blocked-1.0.0.tar.gz", []byte("blocked")); rec.Code != http.StatusForbidden {
		t.Fatalf("越前缀发布状态码=%d，期望 403", rec.Code)
	}
	if rec := pypiUpload(t, e, "pypi-policy-audit", basic, "allowed", "1.0.0", "allowed-1.0.0.tar.gz", []byte("allowed")); rec.Code != http.StatusOK {
		t.Fatalf("允许前缀发布状态码=%d，期望 200：%s", rec.Code, rec.Body.String())
	}
	assertProtocolAudit(t, e, username, user.Id, "pypi.publish", "rejected", "publish_path_denied")
	entries, err := e.auditLogs.List(repository.AuditFilter{Actor: username, Limit: 50})
	if err != nil {
		t.Fatalf("查询 PyPI 成功审计：%v", err)
	}
	for _, entry := range entries {
		if entry.Action == "pypi.publish" && entry.Result == "ok" && strings.Contains(entry.Detail, "operationId=") && strings.Contains(entry.Detail, "protocol=native") {
			return
		}
	}
	t.Fatalf("缺少带 operationId 的 PyPI 成功审计：%+v", entries)
}

func TestPypiHostedPEP503PEP691AndMultipart(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-hosted", "pypi", "hosted")
	payload := []byte("wheel-bytes")
	res := pypiUpload(t, e, "pypi-hosted", "Bearer "+admin, "Demo_Package", "1.2.3", "demo_package-1.2.3-py3-none-any.whl", payload)
	if res.Code != http.StatusOK {
		t.Fatalf("PyPI multipart 发布状态码=%d，响应=%s", res.Code, res.Body.String())
	}
	if duplicate := pypiUpload(t, e, "pypi-hosted", "Bearer "+admin, "demo-package", "1.2.3", "demo_package-1.2.3-py3-none-any.whl", payload); duplicate.Code != http.StatusConflict {
		t.Fatalf("重复 PyPI 文件状态码=%d，期望 409", duplicate.Code)
	}

	root := e.rawReq(http.MethodGet, "/pypi/pypi-hosted/simple/", "Bearer "+admin, "", nil)
	if root.Code != http.StatusOK || !bytes.Contains(root.Body.Bytes(), []byte("demo-package")) {
		t.Fatalf("PyPI simple 根索引无项目：状态码=%d，响应=%s", root.Code, root.Body.String())
	}
	page := e.rawReq(http.MethodGet, "/pypi/pypi-hosted/simple/demo-package/", "Bearer "+admin, "", nil)
	if page.Code != http.StatusOK || !bytes.Contains(page.Body.Bytes(), []byte("#sha256=")) || !bytes.Contains(page.Body.Bytes(), []byte("data-requires-python")) {
		t.Fatalf("PyPI PEP503 页面不完整：状态码=%d，响应=%s", page.Code, page.Body.String())
	}
	jsonReq := httptest.NewRequest(http.MethodGet, "/pypi/pypi-hosted/simple/demo-package/", nil)
	jsonReq.Host = "127.0.0.1"
	jsonReq.RemoteAddr = "127.0.0.1:1234"
	jsonReq.Header.Set("Authorization", "Bearer "+admin)
	jsonReq.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	jsonRec := httptest.NewRecorder()
	e.h.ServeHTTP(jsonRec, jsonReq)
	if jsonRec.Code != http.StatusOK || !strings.Contains(jsonRec.Header().Get("Content-Type"), "application/vnd.pypi.simple.v1+json") {
		t.Fatalf("PyPI PEP691 响应状态码=%d，Content-Type=%q", jsonRec.Code, jsonRec.Header().Get("Content-Type"))
	}
	var doc struct {
		Meta  map[string]any `json:"meta"`
		Files []struct {
			URL    string            `json:"url"`
			Hashes map[string]string `json:"hashes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(jsonRec.Body.Bytes(), &doc); err != nil || len(doc.Files) != 1 || doc.Files[0].Hashes["sha256"] == "" {
		t.Fatalf("PyPI PEP691 JSON 无哈希文件：err=%v body=%s", err, jsonRec.Body.String())
	}
	if strings.Contains(doc.Files[0].URL, "upstream") {
		t.Fatalf("PyPI 文件链接泄漏上游：%s", doc.Files[0].URL)
	}

	pkg := e.rawReq(http.MethodGet, "/pypi/pypi-hosted/packages/demo-package/demo_package-1.2.3-py3-none-any.whl", "Bearer "+admin, "", nil)
	if pkg.Code != http.StatusOK || !bytes.Equal(pkg.Body.Bytes(), payload) || pkg.Header().Get("ETag") == "" {
		t.Fatalf("PyPI 包下载不一致：状态码=%d 内容=%q ETag=%q", pkg.Code, pkg.Body.Bytes(), pkg.Header().Get("ETag"))
	}
	head := e.rawReq(http.MethodHead, "/pypi/pypi-hosted/packages/demo-package/demo_package-1.2.3-py3-none-any.whl", "Bearer "+admin, "", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("PyPI 包 HEAD 无效：状态码=%d body=%d length=%q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
}

func TestPypiGroupMergesMembersAndRejectsUpload(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-a", "pypi", "hosted")
	createFormatRepo(t, e, admin, "pypi-b", "pypi", "hosted")
	createFormatRepo(t, e, admin, "pypi-group", "pypi", "group", "pypi-a", "pypi-b")
	if res := pypiUpload(t, e, "pypi-a", "Bearer "+admin, "group-pkg", "1.0.0", "group_pkg-1.0.0.tar.gz", []byte("group")); res.Code != http.StatusOK {
		t.Fatalf("成员 PyPI 发布失败：%d %s", res.Code, res.Body.String())
	}
	page := e.rawReq(http.MethodGet, "/pypi/pypi-group/simple/group-pkg/", "Bearer "+admin, "", nil)
	if page.Code != http.StatusOK || !bytes.Contains(page.Body.Bytes(), []byte("group_pkg-1.0.0.tar.gz")) {
		t.Fatalf("PyPI group 未合并成员：状态码=%d，响应=%s", page.Code, page.Body.String())
	}
	if res := pypiUpload(t, e, "pypi-group", "Bearer "+admin, "group-pkg", "2.0.0", "group_pkg-2.0.0.tar.gz", []byte("no")); res.Code != http.StatusConflict {
		t.Fatalf("PyPI group 发布状态码=%d，期望 409", res.Code)
	}
}

func TestPypiInvalidMultipartIsRejected(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	createFormatRepo(t, e, admin, "pypi-invalid", "pypi", "hosted")
	res := e.rawReq(http.MethodPost, "/pypi/pypi-invalid/legacy/", "Bearer "+admin, "application/octet-stream", []byte("not multipart"))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("非法 PyPI multipart 状态码=%d，期望 400", res.Code)
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
}

func TestPypiProxyCachesSimpleIndexAndPackage(t *testing.T) {
	var indexHits, packageHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/proxy-pkg/":
			atomic.AddInt32(&indexHits, 1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<a href="/packages/proxy-pkg/proxy_pkg-1.0.0.tar.gz#sha256=` + pypiSHA256([]byte("proxy-pypi-package")) + `">proxy_pkg-1.0.0.tar.gz</a>`))
		case "/packages/proxy-pkg/proxy_pkg-1.0.0.tar.gz":
			atomic.AddInt32(&packageHits, 1)
			_, _ = w.Write([]byte("proxy-pypi-package"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL + "/simple"})
	if _, err := e.repoRepo.Create("pypi-proxy", "pypi", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, "/pypi/pypi-proxy/simple/proxy-pkg/", "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte("proxy_pkg-1.0.0.tar.gz")) || !bytes.Contains(res.Body.Bytes(), []byte("#sha256="+pypiSHA256([]byte("proxy-pypi-package")))) {
			t.Fatalf("PyPI proxy Simple 状态码=%d，响应=%s", res.Code, res.Body.String())
		}
	}
	for i := 0; i < 2; i++ {
		res := e.rawReq(http.MethodGet, "/pypi/pypi-proxy/packages/proxy-pkg/proxy_pkg-1.0.0.tar.gz", "Bearer "+admin, "", nil)
		if res.Code != http.StatusOK || res.Body.String() != "proxy-pypi-package" {
			t.Fatalf("PyPI proxy 下载状态码=%d，内容=%q", res.Code, res.Body.String())
		}
	}
	if atomic.LoadInt32(&indexHits) != 1 || atomic.LoadInt32(&packageHits) != 1 {
		t.Fatalf("PyPI proxy 未命中缓存：index=%d package=%d", indexHits, packageHits)
	}
}

func TestPypiProxyDefersPackageFetchAndCoalescesConcurrentRequests(t *testing.T) {
	payload := []byte("proxy-pypi-package")
	var indexHits, packageHits int32
	indexEntered := make(chan struct{}, 2)
	packageEntered := make(chan struct{}, 2)
	releaseIndex := make(chan struct{})
	releasePackage := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/proxy-pkg/":
			atomic.AddInt32(&indexHits, 1)
			indexEntered <- struct{}{}
			<-releaseIndex
			_, _ = w.Write([]byte(`<a href="/packages/proxy-pkg/proxy_pkg-1.0.0.tar.gz#sha256=` + pypiSHA256(payload) + `">proxy_pkg-1.0.0.tar.gz</a>`))
		case "/packages/proxy-pkg/proxy_pkg-1.0.0.tar.gz":
			atomic.AddInt32(&packageHits, 1)
			packageEntered <- struct{}{}
			<-releasePackage
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, _ := json.Marshal(map[string]any{"remoteUrl": upstream.URL + "/simple"})
	if _, err := e.repoRepo.Create("pypi-concurrent-proxy", "pypi", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}

	var simpleWG sync.WaitGroup
	for range 2 {
		simpleWG.Add(1)
		go func() {
			defer simpleWG.Done()
			res := e.rawReq(http.MethodGet, "/pypi/pypi-concurrent-proxy/simple/proxy-pkg/", "Bearer "+admin, "", nil)
			if res.Code != http.StatusOK {
				t.Errorf("并发 Simple 状态码=%d，响应=%s", res.Code, res.Body.String())
			}
		}()
	}
	<-indexEntered
	select {
	case <-indexEntered:
		close(releaseIndex)
		close(releasePackage)
		simpleWG.Wait()
		t.Fatalf("同项目 Simple 应单飞，实际回源 %d 次", atomic.LoadInt32(&indexHits))
	case <-time.After(100 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&packageHits); got != 0 {
		close(releaseIndex)
		simpleWG.Wait()
		t.Fatalf("Simple 不得预拉候选包，实际包回源 %d 次", got)
	}
	close(releaseIndex)
	simpleDone := make(chan struct{})
	go func() {
		simpleWG.Wait()
		close(simpleDone)
	}()
	select {
	case <-packageEntered:
		close(releasePackage)
		<-simpleDone
		t.Fatal("Simple 不得预拉候选包")
	case <-simpleDone:
	case <-time.After(time.Second):
		t.Fatal("Simple 请求未完成")
	}

	var packageWG sync.WaitGroup
	for range 2 {
		packageWG.Add(1)
		go func() {
			defer packageWG.Done()
			res := e.rawReq(http.MethodGet, "/pypi/pypi-concurrent-proxy/packages/proxy-pkg/proxy_pkg-1.0.0.tar.gz", "Bearer "+admin, "", nil)
			if res.Code != http.StatusOK || !bytes.Equal(res.Body.Bytes(), payload) {
				t.Errorf("并发包下载失败：状态码=%d，响应=%q", res.Code, res.Body.Bytes())
			}
		}()
	}
	select {
	case <-packageEntered:
	case <-time.After(time.Second):
		close(releasePackage)
		packageWG.Wait()
		t.Fatal("包下载未回源")
	}
	select {
	case <-packageEntered:
		close(releasePackage)
		packageWG.Wait()
		t.Fatalf("同项目同包应单飞，实际回源 %d 次", atomic.LoadInt32(&packageHits))
	case <-time.After(100 * time.Millisecond):
	}
	close(releasePackage)
	packageWG.Wait()
}

func TestPypiProxyProjectsVerifiedPEP691AndPEP503CandidatesWithoutSourceLeak(t *testing.T) {
	const credentialRef = "PYPI_PROXY_TEST"
	const credential = "upstream-user:upstream-password"
	t.Setenv("JIAN_UPSTREAM_CREDENTIAL_"+credentialRef, credential)

	relativePayload := []byte("relative-pypi-package")
	externalPayload := []byte("external-pypi-package")
	var originAuthorization, originAccept, externalAuthorization string
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalAuthorization = r.Header.Get("Authorization")
		if r.URL.Path != "/files/external_pkg-1.0.0.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(externalPayload)
	}))
	defer external.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuthorization = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/simple/proxy-pkg/":
			originAccept = r.Header.Get("Accept")
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			_, _ = w.Write([]byte(`{"meta":{"api-version":"1.0"},"files":[` +
				`{"filename":"relative_pkg-1.0.0.tar.gz","url":"../../packages/proxy-pkg/relative_pkg-1.0.0.tar.gz","hashes":{"sha256":"` + pypiSHA256(relativePayload) + `"}},` +
				`{"filename":"external_pkg-1.0.0.tar.gz","url":"` + external.URL + `/files/external_pkg-1.0.0.tar.gz","hashes":{"sha256":"` + pypiSHA256(externalPayload) + `"}}]}`))
		case "/packages/proxy-pkg/relative_pkg-1.0.0.tar.gz":
			_, _ = w.Write(relativePayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, err := json.Marshal(map[string]any{"remoteUrl": origin.URL + "/simple", "credentialRef": credentialRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.repoRepo.Create("pypi-secure-proxy", "pypi", "proxy", "public", string(config)); err != nil {
		t.Fatal(err)
	}

	page := e.rawReq(http.MethodGet, "/pypi/pypi-secure-proxy/simple/proxy-pkg/", "Bearer "+admin, "", nil)
	if page.Code != http.StatusOK || !bytes.Contains(page.Body.Bytes(), []byte("relative_pkg-1.0.0.tar.gz")) {
		t.Fatalf("PyPI proxy 索引状态码=%d，响应=%s", page.Code, page.Body.String())
	}
	if bytes.Contains(page.Body.Bytes(), []byte(origin.URL)) || bytes.Contains(page.Body.Bytes(), []byte(external.URL)) || bytes.Contains(page.Body.Bytes(), []byte(credential)) {
		t.Fatalf("对外 Simple 响应泄漏上游信息：%s", page.Body.String())
	}
	if originAuthorization == "" {
		t.Fatal("同源私有上游请求未携带凭据")
	}
	if !strings.Contains(originAccept, "application/vnd.pypi.simple.v1+json") {
		t.Fatalf("PyPI proxy 未优先请求 PEP 691：Accept=%q", originAccept)
	}
	if externalAuthorization != "" {
		t.Fatalf("外域上游请求不应携带凭据：%q", externalAuthorization)
	}
	for filename, want := range map[string][]byte{
		"relative_pkg-1.0.0.tar.gz": relativePayload,
		"external_pkg-1.0.0.tar.gz": externalPayload,
	} {
		pkg := e.rawReq(http.MethodGet, "/pypi/pypi-secure-proxy/packages/proxy-pkg/"+filename, "Bearer "+admin, "", nil)
		if pkg.Code != http.StatusOK || !bytes.Equal(pkg.Body.Bytes(), want) {
			repo, _ := e.repoRepo.GetByName("pypi-secure-proxy")
			assets, _ := e.assetRepo.ListByRepo(repo.ID, "", 10, 0)
			metadata, _ := e.formatMeta.List(repo.ID, "pypi", "proxy-pkg")
			t.Fatalf("PyPI proxy 安全投影下载失败 filename=%q status=%d body=%q assets=%+v metadata=%+v", filename, pkg.Code, pkg.Body.Bytes(), assets, metadata)
		}
	}
}

func TestPypiProxyRejectsHashMismatchWithoutPersistentProjection(t *testing.T) {
	payload := []byte("tampered-pypi-package")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/tampered-pkg/":
			_, _ = w.Write([]byte(`<a href="../../packages/tampered-pkg/tampered_pkg-1.0.0.tar.gz#sha256=` + pypiSHA256([]byte("expected")) + `">tampered_pkg-1.0.0.tar.gz</a>`))
		case "/packages/tampered-pkg/tampered_pkg-1.0.0.tar.gz":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	config, err := json.Marshal(map[string]any{"remoteUrl": upstream.URL + "/simple"})
	if err != nil {
		t.Fatal(err)
	}
	repoID, err := e.repoRepo.Create("pypi-hash-mismatch", "pypi", "proxy", "public", string(config))
	if err != nil {
		t.Fatal(err)
	}

	page := e.rawReq(http.MethodGet, "/pypi/pypi-hash-mismatch/simple/tampered-pkg/", "Bearer "+admin, "", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("哈希失配索引状态码=%d，期望 200：%s", page.Code, page.Body.String())
	}
	if bytes.Contains(page.Body.Bytes(), []byte(upstream.URL)) {
		t.Fatalf("哈希失配响应泄漏上游地址：%s", page.Body.String())
	}
	pkg := e.rawReq(http.MethodGet, "/pypi/pypi-hash-mismatch/packages/tampered-pkg/tampered_pkg-1.0.0.tar.gz", "Bearer "+admin, "", nil)
	if pkg.Code != http.StatusBadGateway {
		t.Fatalf("哈希失配包下载状态码=%d，期望 502：%s", pkg.Code, pkg.Body.String())
	}
	if count, err := e.assetRepo.CountByRepo(repoID, ""); err != nil || count != 0 {
		t.Fatalf("哈希失配不得保留资产 count=%d err=%v", count, err)
	}
	if files, err := e.formatMeta.List(repoID, "pypi", "tampered-pkg"); err != nil || len(files) != 1 || files[0].MetadataJSON == "{}" {
		t.Fatalf("哈希失配必须保留可重试的索引候选但不得标记缓存成功 files=%+v err=%v", files, err)
	}
}
