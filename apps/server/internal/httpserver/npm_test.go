package httpserver_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
)

// createNpmRepo 以管理员身份创建指定 type 的 npm 仓库；remoteURL/members 按需传入。
func (e *protocolEnv) createNpmRepo(t *testing.T, adminToken, name, typ, remoteURL string, members []string) {
	t.Helper()
	vis := api.CreateRepositoryRequestVisibility("public")
	req := api.CreateRepositoryRequest{
		Name:       name,
		Format:     api.CreateRepositoryRequestFormat("npm"),
		Type:       api.CreateRepositoryRequestType(typ),
		Visibility: &vis,
	}
	if remoteURL != "" {
		req.RemoteUrl = &remoteURL
	}
	if len(members) > 0 {
		req.Members = &members
	}
	if code := e.jsonReq(t, http.MethodPost, "/api/v1/repositories", adminToken, req, nil); code != http.StatusCreated {
		t.Fatalf("建 npm %s 仓库 %s 状态码 = %d，期望 201", typ, name, code)
	}
}

// npmPublishBody 构造 npm publish PUT 体：单版本 + 内联 base64 tarball（_attachments）。
func npmPublishBody(t *testing.T, pkg, version, tarballName string, tarball []byte) []byte {
	t.Helper()
	doc := map[string]any{
		"_id":       pkg,
		"name":      pkg,
		"dist-tags": map[string]any{"latest": version},
		"versions": map[string]any{
			version: map[string]any{
				"name":    pkg,
				"version": version,
				"dist":    map[string]any{"tarball": "http://upstream.invalid/" + pkg + "/-/" + tarballName},
			},
		},
		"_attachments": map[string]any{
			tarballName: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("构造发布体：%v", err)
	}
	return raw
}

// npmTarballURL 从 packument 响应体取指定版本的 dist.tarball。
func npmTarballURL(t *testing.T, body []byte, version string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("解析 packument：%v（体：%s）", err, body)
	}
	versions, _ := doc["versions"].(map[string]any)
	v, _ := versions[version].(map[string]any)
	dist, _ := v["dist"].(map[string]any)
	url, _ := dist["tarball"].(string)
	return url
}

func TestNpmPublishAndInstall(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createNpmRepo(t, adminToken, "npm-hosted", "hosted", "", nil)

	tarball := []byte("fake tgz bytes for lodash")
	body := npmPublishBody(t, "lodash", "1.0.0", "lodash-1.0.0.tgz", tarball)
	if rec := e.rawReq(http.MethodPut, "/npm/npm-hosted/lodash", "Bearer "+adminToken, "application/json", body); rec.Code != http.StatusCreated {
		t.Fatalf("publish 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}

	// packument：含版本、dist-tags.latest，tarball 已重写为本仓地址。
	rec := e.rawReq(http.MethodGet, "/npm/npm-hosted/lodash", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("packument 状态码 = %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析 packument：%v", err)
	}
	if dt, _ := doc["dist-tags"].(map[string]any); dt["latest"] != "1.0.0" {
		t.Errorf("dist-tags.latest = %v，期望 1.0.0", dt["latest"])
	}
	url := npmTarballURL(t, rec.Body.Bytes(), "1.0.0")
	if !strings.HasSuffix(url, "/npm/npm-hosted/lodash/-/lodash-1.0.0.tgz") || !strings.HasPrefix(url, "http") {
		t.Fatalf("tarball 未重写为本仓绝对地址：%q", url)
	}

	// tarball：字节一致。
	tarPath := url[strings.Index(url, "/npm/"):]
	rec = e.rawReq(http.MethodGet, tarPath, "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK || !strings.EqualFold(rec.Body.String(), string(tarball)) {
		t.Fatalf("tarball 状态码 = %d，内容一致 = %v", rec.Code, rec.Body.String() == string(tarball))
	}

	// 缺失包 → 404。
	if rec := e.rawReq(http.MethodGet, "/npm/npm-hosted/missing-pkg", "Bearer "+adminToken, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("缺失 packument 状态码 = %d，期望 404", rec.Code)
	}
}

func TestNpmScopedPackage(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createNpmRepo(t, adminToken, "npm-scoped", "hosted", "", nil)

	tarball := []byte("scoped util tgz")
	body := npmPublishBody(t, "@myscope/util", "2.1.0", "util-2.1.0.tgz", tarball)
	if rec := e.rawReq(http.MethodPut, "/npm/npm-scoped/@myscope/util", "Bearer "+adminToken, "application/json", body); rec.Code != http.StatusCreated {
		t.Fatalf("scoped publish 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}

	rec := e.rawReq(http.MethodGet, "/npm/npm-scoped/@myscope/util", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped packument 状态码 = %d", rec.Code)
	}
	url := npmTarballURL(t, rec.Body.Bytes(), "2.1.0")
	if !strings.HasSuffix(url, "/npm/npm-scoped/@myscope/util/-/util-2.1.0.tgz") {
		t.Fatalf("scoped tarball 重写异常：%q", url)
	}
	tarPath := url[strings.Index(url, "/npm/"):]
	if rec := e.rawReq(http.MethodGet, tarPath, "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.String() != string(tarball) {
		t.Fatalf("scoped tarball 状态码 = %d，内容一致 = %v", rec.Code, rec.Body.String() == string(tarball))
	}
}

func TestNpmProxyRewritesTarball(t *testing.T) {
	tarball := []byte("upstream registry tgz")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lodash":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"name":"lodash","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":"lodash","version":"1.0.0","dist":{"tarball":"%s/lodash/-/lodash-1.0.0.tgz"}}}}`, srv.URL)
		case "/lodash/-/lodash-1.0.0.tgz":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarball)
		default:
			http.Error(w, "nf", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createNpmRepo(t, adminToken, "npm-proxy", "proxy", srv.URL, nil)

	rec := e.rawReq(http.MethodGet, "/npm/npm-proxy/lodash", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy packument 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	url := npmTarballURL(t, rec.Body.Bytes(), "1.0.0")
	if !strings.HasSuffix(url, "/npm/npm-proxy/lodash/-/lodash-1.0.0.tgz") || strings.Contains(url, srv.URL) {
		t.Fatalf("proxy tarball 未重写为本仓地址：%q", url)
	}

	// 经本仓回源拉取 tarball。
	tarPath := url[strings.Index(url, "/npm/"):]
	if rec := e.rawReq(http.MethodGet, tarPath, "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.String() != string(tarball) {
		t.Fatalf("proxy tarball 状态码 = %d，内容一致 = %v", rec.Code, rec.Body.String() == string(tarball))
	}
}

func TestNpmGroupMergesPackument(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createNpmRepo(t, adminToken, "npm-a", "hosted", "", nil)
	e.createNpmRepo(t, adminToken, "npm-b", "hosted", "", nil)
	e.createNpmRepo(t, adminToken, "npm-grp", "group", "", []string{"npm-a", "npm-b"})

	if rec := e.rawReq(http.MethodPut, "/npm/npm-a/lodash", "Bearer "+adminToken, "application/json",
		npmPublishBody(t, "lodash", "1.0.0", "lodash-1.0.0.tgz", []byte("a-tgz"))); rec.Code != http.StatusCreated {
		t.Fatalf("npm-a publish 状态码 = %d", rec.Code)
	}
	if rec := e.rawReq(http.MethodPut, "/npm/npm-b/lodash", "Bearer "+adminToken, "application/json",
		npmPublishBody(t, "lodash", "2.0.0", "lodash-2.0.0.tgz", []byte("b-tgz"))); rec.Code != http.StatusCreated {
		t.Fatalf("npm-b publish 状态码 = %d", rec.Code)
	}

	rec := e.rawReq(http.MethodGet, "/npm/npm-grp/lodash", "Bearer "+adminToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group packument 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析 group packument：%v", err)
	}
	versions, _ := doc["versions"].(map[string]any)
	if _, ok := versions["1.0.0"]; !ok {
		t.Errorf("group 合并缺 1.0.0：%s", rec.Body.String())
	}
	if _, ok := versions["2.0.0"]; !ok {
		t.Errorf("group 合并缺 2.0.0：%s", rec.Body.String())
	}
	// dist-tags 首成员优先：npm-a 的 latest=1.0.0 胜出。
	if dt, _ := doc["dist-tags"].(map[string]any); dt["latest"] != "1.0.0" {
		t.Errorf("group dist-tags.latest = %v，期望首成员 1.0.0", dt["latest"])
	}

	// group tarball 有序命中成员。
	if rec := e.rawReq(http.MethodGet, "/npm/npm-grp/lodash/-/lodash-2.0.0.tgz", "Bearer "+adminToken, "", nil); rec.Code != http.StatusOK || rec.Body.String() != "b-tgz" {
		t.Errorf("group tarball 状态码 = %d，内容 = %q", rec.Code, rec.Body.String())
	}
}
