package httpserver_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

func createGoProxyRepo(t *testing.T, e *protocolEnv, token, name, remoteURL string) {
	createGoProxyRepoWithVisibility(t, e, token, name, remoteURL, "public")
}

func createGoProxyRepoWithVisibility(t *testing.T, e *protocolEnv, token, name, remoteURL, visibilityValue string) {
	t.Helper()
	visibility := api.CreateRepositoryRequestVisibility(visibilityValue)
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", token, api.CreateRepositoryRequest{
		Name: name, Format: api.CreateRepositoryRequestFormat("gomod"), Type: api.CreateRepositoryRequestType("proxy"),
		Visibility: &visibility, RemoteUrl: &remoteURL,
	}, nil); code != http.StatusCreated {
		t.Fatalf("创建 Go proxy 仓库状态码 = %d", code)
	}
}

func TestGoProxyFetchesAllEndpointsCachesAndMaps404(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		contents := map[string][]byte{
			"/example.com/demo/@v/list":        []byte("v1.0.0\nv1.1.0\n"),
			"/example.com/demo/@v/latest":      []byte("{\"Version\":\"v1.1.0\"}"),
			"/example.com/demo/@v/v1.1.0.info": []byte("{\"Version\":\"v1.1.0\"}"),
			"/example.com/demo/@v/v1.1.0.mod":  []byte("module example.com/demo\n\ngo 1.22\n"),
			"/example.com/demo/@v/v1.1.0.zip":  []byte("zip-bytes"),
		}
		body, ok := contents[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createGoProxyRepo(t, e, adminToken, "go-proxy", srv.URL)
	paths := map[string]string{
		"list":   "list",
		"latest": "latest",
		"info":   "v1.1.0.info",
		"mod":    "v1.1.0.mod",
		"zip":    "v1.1.0.zip",
	}
	for name, endpoint := range paths {
		path := "/go/go-proxy/example.com/demo/@v/" + endpoint
		rec := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("Go proxy %s 状态码 = %d，响应 = %s", name, rec.Code, rec.Body.String())
		}
		second := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil)
		if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), rec.Body.Bytes()) {
			t.Fatalf("Go proxy %s 缓存响应不一致：%d/%d", name, rec.Code, second.Code)
		}
	}
	if rec := e.rawReq(http.MethodGet, "/go/go-proxy/example.com/demo/@v/v9.9.9.mod", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("Go proxy 上游 404 状态码 = %d，期望 404", rec.Code)
	}
	if got := atomic.LoadInt32(&hits); got != 6 {
		t.Fatalf("Go proxy 应每个未缓存 endpoint 回源一次，实际回源 %d 次", got)
	}
}

func TestGoProxyRealGoModuleDownloadAndBuild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("本机未安装 go，跳过 Go 原生客户端验收")
	}
	moduleZip := goProxyModuleZip(t, "example.com/native@v1.0.0/go.mod", []byte("module example.com/native\n\ngo 1.22\n"), "example.com/native@v1.0.0/native.go", []byte("package native\n\nfunc Value() string { return \"go-native-ok\" }\n"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contents := map[string][]byte{
			"/example.com/native/@v/list":        []byte("v1.0.0\n"),
			"/example.com/native/@v/latest":      []byte(`{"Version":"v1.0.0"}`),
			"/example.com/native/@v/v1.0.0.info": []byte(`{"Version":"v1.0.0","Time":"2026-09-06T00:00:00Z"}`),
			"/example.com/native/@v/v1.0.0.mod":  []byte("module example.com/native\n\ngo 1.22\n"),
			"/example.com/native/@v/v1.0.0.zip":  moduleZip,
		}
		body, ok := contents[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createGoProxyRepo(t, e, adminToken, "go-native", upstream.URL)
	artifact := httptest.NewServer(e.h)
	defer artifact.Close()

	consumer := t.TempDir()
	if err := os.WriteFile(filepath.Join(consumer, "go.mod"), []byte("module example.com/consumer\n\ngo 1.22\n\nrequire example.com/native v1.0.0\n"), 0o600); err != nil {
		t.Fatalf("写入 Go consumer go.mod：%v", err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "main.go"), []byte("package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/native\"\n)\n\nfunc main() { fmt.Print(native.Value()) }\n"), 0o600); err != nil {
		t.Fatalf("写入 Go consumer main.go：%v", err)
	}
	env := append(os.Environ(),
		"GOPROXY="+artifact.URL+"/go/go-native",
		"GOSUMDB=off",
		"GONOSUMDB=*",
		"GOTOOLCHAIN=local",
	)
	for _, command := range [][]string{{"go", "mod", "download", "all"}, {"go", "build", "-o", filepath.Join(consumer, "consumer.exe"), "."}} {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = consumer
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("运行 %s：%v\n%s", strings.Join(command, " "), err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(consumer, "consumer.exe")); err != nil {
		t.Fatalf("Go 原生 consumer 未生成：%v", err)
	}
}

func TestGoProxyRealGoClientFallsBackAfter410(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("本机未安装 go，跳过 Go 原生客户端验收")
	}
	moduleZip := goProxyModuleZip(t, "example.com/native@v1.0.0/go.mod", []byte("module example.com/native\n\ngo 1.22\n"), "example.com/native@v1.0.0/native.go", []byte("package native\n\nfunc Value() string { return \"go-fallback-ok\" }\n"))
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer gone.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contents := map[string][]byte{
			"/example.com/native/@v/v1.0.0.info": []byte(`{"Version":"v1.0.0","Time":"2026-09-06T00:00:00Z"}`),
			"/example.com/native/@v/v1.0.0.mod":  []byte("module example.com/native\n\ngo 1.22\n"),
			"/example.com/native/@v/v1.0.0.zip":  moduleZip,
		}
		body, ok := contents[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer fallback.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createGoProxyRepo(t, e, adminToken, "go-gone-fallback", gone.URL)
	var artifactRequests []string
	artifact := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		e.h.ServeHTTP(recorder, r)
		artifactRequests = append(artifactRequests, fmt.Sprintf("%s %s host=%s -> %d body=%s", r.Method, r.URL.Path, r.Host, recorder.Code, strings.TrimSpace(recorder.Body.String())))
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer artifact.Close()
	probe := e.rawReq(http.MethodGet, "/go/go-gone-fallback/example.com/native/@v/v1.0.0.info", "", "", nil)
	if probe.Code != http.StatusGone {
		t.Fatalf("匿名 Go proxy 410 探针状态码 = %d，响应 = %s", probe.Code, probe.Body.String())
	}

	consumer := t.TempDir()
	if err := os.WriteFile(filepath.Join(consumer, "go.mod"), []byte("module example.com/consumer\n\ngo 1.22\n\nrequire example.com/native v1.0.0\n"), 0o600); err != nil {
		t.Fatalf("写入 Go fallback consumer go.mod：%v", err)
	}
	if err := os.WriteFile(filepath.Join(consumer, "main.go"), []byte("package main\n\nimport \"example.com/native\"\n\nfunc main() { _ = native.Value() }\n"), 0o600); err != nil {
		t.Fatalf("写入 Go fallback consumer main.go：%v", err)
	}
	env := append(os.Environ(),
		"GOPROXY="+artifact.URL+"/go/go-gone-fallback,"+fallback.URL,
		"GOSUMDB=off",
		"GONOSUMDB=*",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOMODCACHE="+t.TempDir(),
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	for _, command := range [][]string{{"go", "mod", "download", "all"}, {"go", "build", "-o", filepath.Join(consumer, "consumer.exe"), "."}} {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = consumer
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("410 回退运行 %s：%v\n%s\nartifact requests=%v", strings.Join(command, " "), err, output, artifactRequests)
		}
	}
	if _, err := os.Stat(filepath.Join(consumer, "consumer.exe")); err != nil {
		t.Fatalf("410 回退 Go consumer 未生成：%v", err)
	}
}

func goProxyModuleZip(t *testing.T, entries ...any) []byte {
	t.Helper()
	var body bytes.Buffer
	archive := zip.NewWriter(&body)
	for i := 0; i < len(entries); i += 2 {
		name, ok := entries[i].(string)
		if !ok || i+1 >= len(entries) {
			t.Fatalf("Go module zip fixture entry 非法：%v", entries)
		}
		content, ok := entries[i+1].([]byte)
		if !ok {
			t.Fatalf("Go module zip fixture content 非法：%T", entries[i+1])
		}
		file, err := archive.Create(name)
		if err != nil {
			t.Fatalf("创建 Go module zip entry：%v", err)
		}
		if _, err := file.Write(content); err != nil {
			t.Fatalf("写入 Go module zip entry：%v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("关闭 Go module zip：%v", err)
	}
	return body.Bytes()
}

func TestGoProxyPreservesUpstreamGone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "模块已下架", http.StatusGone)
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createGoProxyRepo(t, e, adminToken, "go-gone", upstream.URL)

	rec := e.rawReq(http.MethodGet, "/go/go-gone/example.com/demo/@v/v1.0.0.mod", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("Go proxy 上游 410 状态码 = %d，期望 410（体：%s）", rec.Code, rec.Body.String())
	}
}

func TestGoProxyConcurrentMissSingleFlightAndAuth(t *testing.T) {
	var hits int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		startedOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write([]byte("module example.com/concurrent"))
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createGoProxyRepo(t, e, adminToken, "go-concurrent", srv.URL)
	path := "/go/go-concurrent/example.com/concurrent/@v/v1.0.0.mod"
	const clients = 8
	results := make(chan int, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e.rawReq(http.MethodGet, path, "Bearer "+adminToken, "", nil)
			results <- rec.Code
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for code := range results {
		if code != http.StatusOK {
			t.Fatalf("Go proxy 并发请求状态码 = %d，期望 200", code)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("Go proxy 并发 miss 应 single-flight，实际回源 %d 次", got)
	}

	createGoProxyRepoWithVisibility(t, e, adminToken, "go-private", srv.URL, "private")
	if rec := e.rawReq(http.MethodGet, "/go/go-private/example.com/concurrent/@v/v1.0.0.mod", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("Go proxy 私有仓匿名请求状态码 = %d，期望 401", rec.Code)
	}
	if got := e.rawReq(http.MethodGet, "/go/go-private/example.com/concurrent/@v/v1.0.0.mod", "Bearer "+adminToken, "", nil).Code; got != http.StatusOK {
		t.Fatalf("Go proxy 私有仓管理员请求状态码 = %d，期望 200", got)
	}
}

func TestGoProxyRejectsNonGoRepositoryAndInvalidPath(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createRawRepo(t, adminToken, "not-go", "public")
	createGoProxyRepo(t, e, adminToken, "go-valid-path", "http://127.0.0.1:1")
	if rec := e.rawReq(http.MethodGet, "/go/not-go/example.com/demo/@v/v1.0.0.mod", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("非 gomod/proxy 请求状态码 = %d，期望 404", rec.Code)
	}
	if rec := e.rawReq(http.MethodGet, "/go/go-valid-path/example.com/DEMO/@v/v1.0.0.mod", "Bearer "+adminToken, "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 Go module 路径状态码 = %d，期望 400", rec.Code)
	}
}

func TestGoProxyRejectsDoubleSlashBeforeNormalization(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		_, _ = w.Write([]byte("must not fetch"))
	}))
	defer upstream.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	createGoProxyRepo(t, e, adminToken, "go-double-slash", upstream.URL)

	rec := e.rawReq(http.MethodGet, "/go/go-double-slash/example.com/demo//@v/v1.0.0.mod", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重复斜杠状态码 = %d，期望 400：%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType == "" || bytes.Contains(rec.Body.Bytes(), []byte("<html")) {
		t.Fatalf("重复斜杠不得返回 HTML：Content-Type=%q body=%q", contentType, rec.Body.String())
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
		t.Fatalf("重复斜杠必须在回源前拒绝，实际回源 %d 次", got)
	}
	repo, err := e.repoRepo.GetByName("go-double-slash")
	if err != nil {
		t.Fatalf("读取 Go proxy 仓库：%v", err)
	}
	if count, err := e.assetRepo.CountByRepo(repo.ID, ""); err != nil || count != 0 {
		t.Fatalf("重复斜杠拒绝后不得写缓存：count=%d err=%v", count, err)
	}
}
