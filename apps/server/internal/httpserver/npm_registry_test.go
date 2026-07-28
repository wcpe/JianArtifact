package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// putJSON 便捷封装：构造 JSON 体发起协议层请求。
func (e *protocolEnv) npmJSON(t *testing.T, method, path, authHeader string, body any) ([]byte, int) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体：%v", err)
		}
	}
	rec := e.rawReq(method, path, authHeader, "application/json", raw)
	return rec.Body.Bytes(), rec.Code
}

func TestNpmRegistryPingWhoamiLogin(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createNpmRepo(t, adminToken, "npm-reg", "hosted", "", nil)

	// ping：无鉴权可达（npm ping 在 login 前调用）。
	if rec := e.rawReq(http.MethodGet, "/npm/npm-reg/-/ping", "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("ping 状态码 = %d，期望 200", rec.Code)
	}

	// whoami：无凭据 401；带会话凭据返回用户名。
	if rec := e.rawReq(http.MethodGet, "/npm/npm-reg/-/whoami", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 whoami 状态码 = %d，期望 401", rec.Code)
	}
	body, code := e.npmJSON(t, http.MethodGet, "/npm/npm-reg/-/whoami", "Bearer "+adminToken, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"username":"admin"`) {
		t.Fatalf("whoami = %d %s，期望 200 且含 username=admin", code, body)
	}

	// login：口令错误 401。
	_, code = e.npmJSON(t, http.MethodPut, "/npm/npm-reg/-/user/org.couchdb.user:admin", "",
		map[string]any{"name": "admin", "password": "wrong-pass"})
	if code != http.StatusUnauthorized {
		t.Fatalf("错误口令 login 状态码 = %d，期望 401", code)
	}

	// login：口令正确签发 jat_ API Token，且该 token 可用于后续请求。
	body, code = e.npmJSON(t, http.MethodPut, "/npm/npm-reg/-/user/org.couchdb.user:admin", "",
		map[string]any{"name": "admin", "password": "admin-pass-123"})
	if code != http.StatusCreated {
		t.Fatalf("login 状态码 = %d（体：%s），期望 201", code, body)
	}
	var loginResp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &loginResp); err != nil || !loginResp.OK || !strings.HasPrefix(loginResp.Token, "jat_") {
		t.Fatalf("login 响应异常：%s", body)
	}
	body, code = e.npmJSON(t, http.MethodGet, "/npm/npm-reg/-/whoami", "Bearer "+loginResp.Token, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"username":"admin"`) {
		t.Fatalf("签发 token whoami = %d %s，期望 200 admin", code, body)
	}
}

func TestNpmDistTags(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	auth := "Bearer " + adminToken
	e.createNpmRepo(t, adminToken, "npm-dt", "hosted", "", nil)
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if rec := e.rawReq(http.MethodPut, "/npm/npm-dt/lodash", auth, "application/json",
			npmPublishBody(t, "lodash", v, "lodash-"+v+".tgz", []byte("tgz-"+v))); rec.Code != http.StatusCreated {
			t.Fatalf("publish %s 状态码 = %d", v, rec.Code)
		}
	}

	// ls：latest 为最后一次 publish 的版本。
	body, code := e.npmJSON(t, http.MethodGet, "/npm/npm-dt/-/package/lodash/dist-tags", auth, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"latest":"2.0.0"`) {
		t.Fatalf("dist-tag ls = %d %s，期望 latest=2.0.0", code, body)
	}

	// add：版本必须已存在。
	if _, code := e.npmJSON(t, http.MethodPut, "/npm/npm-dt/-/package/lodash/dist-tags/beta", auth, "9.9.9"); code != http.StatusBadRequest {
		t.Fatalf("add 不存在版本状态码 = %d，期望 400", code)
	}
	if _, code := e.npmJSON(t, http.MethodPut, "/npm/npm-dt/-/package/lodash/dist-tags/beta", auth, "1.0.0"); code != http.StatusCreated {
		t.Fatalf("add beta 状态码 = %d，期望 201", code)
	}
	body, _ = e.npmJSON(t, http.MethodGet, "/npm/npm-dt/-/package/lodash/dist-tags", auth, nil)
	if !strings.Contains(string(body), `"beta":"1.0.0"`) {
		t.Fatalf("add 后 ls 缺 beta：%s", body)
	}

	// rm：latest 拒删、不存在 404、正常删除生效。
	if _, code := e.npmJSON(t, http.MethodDelete, "/npm/npm-dt/-/package/lodash/dist-tags/latest", auth, nil); code != http.StatusBadRequest {
		t.Fatalf("rm latest 状态码 = %d，期望 400", code)
	}
	if _, code := e.npmJSON(t, http.MethodDelete, "/npm/npm-dt/-/package/lodash/dist-tags/nope", auth, nil); code != http.StatusNotFound {
		t.Fatalf("rm 不存在标签状态码 = %d，期望 404", code)
	}
	if _, code := e.npmJSON(t, http.MethodDelete, "/npm/npm-dt/-/package/lodash/dist-tags/beta", auth, nil); code != http.StatusOK {
		t.Fatalf("rm beta 状态码 = %d，期望 200", code)
	}
	body, _ = e.npmJSON(t, http.MethodGet, "/npm/npm-dt/-/package/lodash/dist-tags", auth, nil)
	if strings.Contains(string(body), "beta") {
		t.Fatalf("rm 后 beta 仍在：%s", body)
	}

	// scoped 包路径解析（pkg 含 /）。
	if rec := e.rawReq(http.MethodPut, "/npm/npm-dt/@myscope/util", auth, "application/json",
		npmPublishBody(t, "@myscope/util", "1.0.0", "util-1.0.0.tgz", []byte("s"))); rec.Code != http.StatusCreated {
		t.Fatalf("scoped publish 状态码 = %d", rec.Code)
	}
	if _, code := e.npmJSON(t, http.MethodPut, "/npm/npm-dt/-/package/@myscope/util/dist-tags/next", auth, "1.0.0"); code != http.StatusCreated {
		t.Fatalf("scoped dist-tag add 状态码 = %d，期望 201", code)
	}
}

func TestNpmUnpublish(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	auth := "Bearer " + adminToken
	e.createNpmRepo(t, adminToken, "npm-up", "hosted", "", nil)
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if rec := e.rawReq(http.MethodPut, "/npm/npm-up/lodash", auth, "application/json",
			npmPublishBody(t, "lodash", v, "lodash-"+v+".tgz", []byte("tgz-"+v))); rec.Code != http.StatusCreated {
			t.Fatalf("publish %s 状态码 = %d", v, rec.Code)
		}
	}

	// 单版本 unpublish：修订 PUT 替换写（剔除 2.0.0）→ DELETE tarball。
	replaced := map[string]any{
		"name":      "lodash",
		"dist-tags": map[string]any{"latest": "1.0.0"},
		"versions": map[string]any{
			"1.0.0": map[string]any{
				"name": "lodash", "version": "1.0.0",
				"dist": map[string]any{"tarball": "http://x/lodash/-/lodash-1.0.0.tgz"},
			},
		},
	}
	if _, code := e.npmJSON(t, http.MethodPut, "/npm/npm-up/lodash/-rev/1-abc", auth, replaced); code != http.StatusCreated {
		t.Fatalf("修订 PUT 状态码 = %d，期望 201", code)
	}
	if _, code := e.npmJSON(t, http.MethodDelete, "/npm/npm-up/lodash/-/lodash-2.0.0.tgz/-rev/1-abc", auth, nil); code != http.StatusOK {
		t.Fatalf("删 tarball 状态码 = %d，期望 200", code)
	}
	body, code := e.npmJSON(t, http.MethodGet, "/npm/npm-up/lodash", auth, nil)
	if code != http.StatusOK || strings.Contains(string(body), "2.0.0") {
		t.Fatalf("unpublish 后 packument 仍含 2.0.0：%d %s", code, body)
	}
	if rec := e.rawReq(http.MethodGet, "/npm/npm-up/lodash/-/lodash-2.0.0.tgz", auth, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("已删 tarball 状态码 = %d，期望 404", rec.Code)
	}

	// 整包 unpublish：packument 与全部 tarball 一并删除。
	if _, code := e.npmJSON(t, http.MethodDelete, "/npm/npm-up/lodash/-rev/1-abc", auth, nil); code != http.StatusOK {
		t.Fatalf("整包删除状态码 = %d，期望 200", code)
	}
	if rec := e.rawReq(http.MethodGet, "/npm/npm-up/lodash", auth, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("整包删除后 packument 状态码 = %d，期望 404", rec.Code)
	}
	if rec := e.rawReq(http.MethodGet, "/npm/npm-up/lodash/-/lodash-1.0.0.tgz", auth, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("整包删除后 tarball 状态码 = %d，期望 404", rec.Code)
	}

	// 非 hosted 仓库拒绝 unpublish（409）。
	e.createNpmRepo(t, adminToken, "npm-up-proxy", "proxy", "http://upstream.invalid", nil)
	if _, code := e.npmJSON(t, http.MethodDelete, "/npm/npm-up-proxy/lodash/-rev/1-a", auth, nil); code != http.StatusConflict {
		t.Fatalf("proxy 仓 unpublish 状态码 = %d，期望 409", code)
	}
}

func TestNpmDeprecate(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	auth := "Bearer " + adminToken
	e.createNpmRepo(t, adminToken, "npm-dep", "hosted", "", nil)
	if rec := e.rawReq(http.MethodPut, "/npm/npm-dep/lodash", auth, "application/json",
		npmPublishBody(t, "lodash", "1.0.0", "lodash-1.0.0.tgz", []byte("t"))); rec.Code != http.StatusCreated {
		t.Fatalf("publish 状态码 = %d", rec.Code)
	}

	// npm deprecate 发无 _attachments 的 packument PUT，versions 覆盖合并。
	dep := map[string]any{
		"name":      "lodash",
		"dist-tags": map[string]any{"latest": "1.0.0"},
		"versions": map[string]any{
			"1.0.0": map[string]any{
				"name": "lodash", "version": "1.0.0",
				"deprecated": "use lodash-es instead",
				"dist":       map[string]any{"tarball": "http://x/lodash/-/lodash-1.0.0.tgz"},
			},
		},
	}
	if _, code := e.npmJSON(t, http.MethodPut, "/npm/npm-dep/lodash", auth, dep); code != http.StatusCreated {
		t.Fatalf("deprecate PUT 状态码 = %d，期望 201", code)
	}
	body, _ := e.npmJSON(t, http.MethodGet, "/npm/npm-dep/lodash", auth, nil)
	if !strings.Contains(string(body), `"deprecated":"use lodash-es instead"`) {
		t.Fatalf("deprecate 未合并进 packument：%s", body)
	}
}

func TestNpmSearch(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	auth := "Bearer " + adminToken
	e.createNpmRepo(t, adminToken, "npm-sr", "hosted", "", nil)
	for _, pkg := range []string{"lodash", "express"} {
		if rec := e.rawReq(http.MethodPut, "/npm/npm-sr/"+pkg, auth, "application/json",
			npmPublishBody(t, pkg, "1.0.0", pkg+"-1.0.0.tgz", []byte(pkg))); rec.Code != http.StatusCreated {
			t.Fatalf("publish %s 状态码 = %d", pkg, rec.Code)
		}
	}

	body, code := e.npmJSON(t, http.MethodGet, "/npm/npm-sr/-/v1/search?text=lod&size=20", auth, nil)
	if code != http.StatusOK {
		t.Fatalf("search 状态码 = %d", code)
	}
	var out struct {
		Objects []struct {
			Package struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"package"`
		} `json:"objects"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析 search 响应：%v（体：%s）", err, body)
	}
	if out.Total != 1 || len(out.Objects) != 1 || out.Objects[0].Package.Name != "lodash" || out.Objects[0].Package.Version != "1.0.0" {
		t.Fatalf("search 结果异常：%s", body)
	}

	// text 为空返回空集。
	body, _ = e.npmJSON(t, http.MethodGet, "/npm/npm-sr/-/v1/search?text=", auth, nil)
	if !strings.Contains(string(body), `"total":0`) {
		t.Fatalf("空 text search 应返回空集：%s", body)
	}
}

func TestNpmAudit(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	auth := "Bearer " + adminToken
	e.createNpmRepo(t, adminToken, "npm-au", "hosted", "", nil)

	body, code := e.npmJSON(t, http.MethodPost, "/npm/npm-au/-/npm/v1/security/advisories/bulk", auth, map[string]any{"lodash": []string{"1.0.0"}})
	if code != http.StatusOK || strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("advisories/bulk = %d %s，期望 200 {}", code, body)
	}
	body, code = e.npmJSON(t, http.MethodPost, "/npm/npm-au/-/npm/v1/security/audits/quick", auth, map[string]any{})
	if code != http.StatusOK || !strings.Contains(string(body), `"vulnerabilities"`) {
		t.Fatalf("audits/quick = %d %s，期望 200 全零报告", code, body)
	}
}

func TestNpmAbbreviatedPackument(t *testing.T) {
	e := newProtocolEnv(t)
	adminToken := e.bootstrapAdmin(t)
	e.createNpmRepo(t, adminToken, "npm-ab", "hosted", "", nil)
	if rec := e.rawReq(http.MethodPut, "/npm/npm-ab/lodash", "Bearer "+adminToken, "application/json",
		npmPublishBody(t, "lodash", "1.0.0", "lodash-1.0.0.tgz", []byte("t"))); rec.Code != http.StatusCreated {
		t.Fatalf("publish 状态码 = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/npm/npm-ab/lodash", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("abbreviated packument 状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/vnd.npm.install-v1+json") {
		t.Fatalf("Content-Type = %q，期望 install-v1 媒体类型", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析 abbreviated 文档：%v", err)
	}
	if _, has := doc["_id"]; has {
		t.Errorf("abbreviated 文档不应含顶层 _id：%s", rec.Body.String())
	}
	versions, _ := doc["versions"].(map[string]any)
	vm, _ := versions["1.0.0"].(map[string]any)
	if vm == nil || vm["dist"] == nil {
		t.Fatalf("abbreviated 文档缺 versions.1.0.0.dist：%s", rec.Body.String())
	}
}
