// BUG 复现：npm 仓库经 `/repository/:repo/*artifactPath` 通用制品路径的发布/拉取。
// 真实 npm 客户端按用户配置的 registry URL（`…/repository/<repo>/`）发 publish，
// 此前 Dispatcher 将 npm 仓库分派到 RawHandler——packument 被当普通文件覆盖写入、
// `_attachments` 中的 tarball 静默丢失，全新 `npm install` 404 tarball。
// 修复后两条前缀必须具备完整 npm 语义（版本合并、tarball 落库、dist.tarball 重写）。
package httpserver_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// npmViaRepositoryPrefix 用例矩阵：未 scope / scoped（URL 编码 %2f）。
func TestNpmPublishViaRepositoryPrefix(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	e.createNpmRepo(t, admin, "npm-hosted", "hosted", "", nil)

	cases := []struct {
		name    string
		pkg     string // 包名（URL 路径段需编码）
		pathPkg string // URL 中的编码形式
		version string
		tarball string
	}{
		{name: "unscoped", pkg: "lodash", pathPkg: "lodash", version: "1.0.0", tarball: "fake tgz lodash"},
		{name: "scoped", pkg: "@acc/probe", pathPkg: "%40acc%2fprobe", version: "1.0.0", tarball: "fake tgz scoped"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := npmPublishBody(t, tc.pkg, tc.version, fmt.Sprintf("%s-%s.tgz", strings.TrimPrefix(tc.pkg, "@acc/"), tc.version), []byte(tc.tarball))
			publishPath := "/repository/npm-hosted/" + tc.pathPkg
			if rec := e.rawReq(http.MethodPut, publishPath, "Bearer "+admin, "application/json", body); rec.Code != http.StatusCreated {
				t.Fatalf("publish(/repository/) 状态码=%d（体：%s）", rec.Code, rec.Body.String())
			}

			// packument：版本在、dist.tarball 已重写为本仓请求基址。
			rec := e.rawReq(http.MethodGet, publishPath, "Bearer "+admin, "", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("packument(/repository/) 状态码=%d", rec.Code)
			}
			var doc map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("解析 packument：%v（体：%s）", err, rec.Body.String())
			}
			versions, _ := doc["versions"].(map[string]any)
			if _, ok := versions[tc.version]; !ok {
				t.Fatalf("packument 缺版本 %s：%s", tc.version, rec.Body.String())
			}
			tarURL := npmTarballURL(t, rec.Body.Bytes(), tc.version)
			// 重写语义：不再指向上游原文地址，且指向本仓规范 /npm/ 路径（两前缀同一资产）。
			if strings.Contains(tarURL, "upstream.invalid") || !strings.Contains(tarURL, "/npm/npm-hosted/") {
				t.Fatalf("dist.tarball 未重写为本仓地址：%q", tarURL)
			}

			// tarball：字节一致（/npm/ 与 /repository/ 两条前缀都要能取到同一资产）。
			tarPath := tarURL[strings.Index(tarURL, "/npm/"):]
			repoPath := strings.Replace(tarPath, "/npm/", "/repository/", 1)
			for _, fetchPath := range []string{tarPath, repoPath} {
				rec := e.rawReq(http.MethodGet, fetchPath, "Bearer "+admin, "", nil)
				if rec.Code != http.StatusOK || rec.Body.String() != tc.tarball {
					t.Fatalf("tarball GET %s 状态码=%d（内容一致=%v）", fetchPath, rec.Code, rec.Body.String() == tc.tarball)
				}
			}
		})
	}
}

// TestNpmPublishViaRepositoryPrefixMergesVersions 确保经 /repository/ 前缀连续发布
// 多版本时 packument 合并既有版本（不丢历史）。
func TestNpmPublishViaRepositoryPrefixMergesVersions(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	e.createNpmRepo(t, admin, "npm-hosted", "hosted", "", nil)

	for _, v := range []string{"1.0.0", "2.0.0"} {
		body := npmPublishBody(t, "multi", v, "multi-"+v+".tgz", []byte("tgz "+v))
		if rec := e.rawReq(http.MethodPut, "/repository/npm-hosted/multi", "Bearer "+admin, "application/json", body); rec.Code != http.StatusCreated {
			t.Fatalf("publish %s 状态码=%d", v, rec.Code)
		}
	}
	rec := e.rawReq(http.MethodGet, "/repository/npm-hosted/multi", "Bearer "+admin, "", nil)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析 packument：%v", err)
	}
	versions, _ := doc["versions"].(map[string]any)
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if _, ok := versions[v]; !ok {
			t.Fatalf("连续发布丢版本 %s：%s", v, rec.Body.String())
		}
	}
}

// TestNpmScopedPublishAlignsTarballNameWithPackument 复现 npm 11 在子路径
// registry 下发布 scoped 包的形状：`_attachments` 键带 scope 前缀
// （@acc/probe-1.0.0.tgz）而 dist.tarball 的 basename 是裸名（probe-1.0.0.tgz）。
// 存储名必须与 packument 声明对齐，否则重写后的 dist.tarball 404。
func TestNpmScopedPublishAlignsTarballNameWithPackument(t *testing.T) {
	e := newProtocolEnv(t)
	admin := e.bootstrapAdmin(t)
	e.createNpmRepo(t, admin, "npm-hosted", "hosted", "", nil)

	const pkg = "@acc/probe"
	tarball := []byte("scoped aligned tgz")
	body := map[string]any{
		"_id": pkg, "name": pkg,
		"dist-tags": map[string]any{"latest": "1.0.0"},
		"versions": map[string]any{"1.0.0": map[string]any{
			"name": pkg, "version": "1.0.0",
			// npm 11 子路径 registry 形态：basename 为裸名。
			"dist": map[string]any{"tarball": "http://127.0.0.1:1/repository/npm-hosted/@acc/probe/-/probe-1.0.0.tgz"},
		}},
		"_attachments": map[string]any{
			// 附件键带 scope 前缀（与 dist.tarball basename 不一致）。
			"@acc/probe-1.0.0.tgz": map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.rawReq(http.MethodPut, "/repository/npm-hosted/%40acc%2fprobe", "Bearer "+admin, "application/json", raw); rec.Code != http.StatusCreated {
		t.Fatalf("publish 状态码=%d（体：%s）", rec.Code, rec.Body.String())
	}

	// packument 重写后的 dist.tarball 必须可直取。
	rec := e.rawReq(http.MethodGet, "/repository/npm-hosted/%40acc%2fprobe", "Bearer "+admin, "", nil)
	tarURL := npmTarballURL(t, rec.Body.Bytes(), "1.0.0")
	if strings.Contains(tarURL, "placeholder") || !strings.Contains(tarURL, "/npm/npm-hosted/") {
		t.Fatalf("dist.tarball 未重写：%q", tarURL)
	}
	tarPath := tarURL[strings.Index(tarURL, "/npm/"):]
	rec = e.rawReq(http.MethodGet, tarPath, "Bearer "+admin, "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != string(tarball) {
		t.Fatalf("tarball GET %s 状态码=%d（内容一致=%v）", tarPath, rec.Code, rec.Body.String() == string(tarball))
	}
}

// guard：httptest 引用保持（与既有测试风格一致，避免 unused import）。
var _ = httptest.NewServer
