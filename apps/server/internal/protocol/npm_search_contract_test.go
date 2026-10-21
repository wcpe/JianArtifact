package protocol

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// 本文件钉住的**npm 端点零变化**：`GET /-/v1/search` 的响应体不得因为
// 新增包级聚合而多出任何字段，也不得因为 `version:` / `group_by` 之类的新参数而改变口径。
//
// 三条独立理由（任一成立即够，规格 §3.8）：路径不同（registrySearch 直接调领域层
// SearchAssets，不经过 HTTP handler）、参数显式（sort=path&order=asc 永远不进 relevance
// 分支）、响应自建（objects/total/time 由 searchObject 自算，只挑它读的字段）。
// 本用例把第 1、3 条变成可执行断言：**顶层键集必须精确等于 {objects,total,time}**。

// TestNpmSearchResponseKeysUnchanged 断言响应顶层键集与每个 object 的键集都没有新字段泄漏。
func TestNpmSearchResponseKeysUnchanged(t *testing.T) {
	e := newNpmSearchEnv(t)
	if _, err := e.repos.Create("npm-contract", "npm", "hosted", "public", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	e.publishPackument(t, "npm-contract", "contract-pkg")
	// tarball 路径（含 /-/）必须继续被排除在搜索结果之外。
	e.publishPackument(t, "npm-contract", "contract-pkg/-/contract-pkg-1.0.0.tgz")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/npm/npm-contract/-/v1/search?text=contract&size=20", nil)
	e.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Fatalf("解析响应：%v（体：%s）", err, rec.Body.String())
	}
	if got, want := sortedJSONKeys(top), []string{"objects", "time", "total"}; !equalKeySlices(got, want) {
		t.Fatalf("顶层键集 = %v，期望 %v（npm 响应不得新增字段）", got, want)
	}

	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(top["objects"], &objects); err != nil {
		t.Fatalf("解析 objects：%v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("objects 条数 = %d，期望 1（tarball 路径被排除、同名 packument 去重）", len(objects))
	}
	if got, want := sortedJSONKeys(objects[0]), []string{"package", "score", "searchScore"}; !equalKeySlices(got, want) {
		t.Fatalf("object 键集 = %v，期望 %v", got, want)
	}

	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(objects[0]["package"], &pkg); err != nil {
		t.Fatalf("解析 package：%v", err)
	}
	want := []string{"date", "description", "links", "maintainers", "name", "version"}
	if got := sortedJSONKeys(pkg); !equalKeySlices(got, want) {
		t.Fatalf("package 键集 = %v，期望 %v", got, want)
	}
}

// TestNpmSearchIgnoresNewQueryParams 钉住新参数不会改变 npm 端点的语义：
// registrySearch 不接受 group_by / order，多传也只是被忽略。
func TestNpmSearchIgnoresNewQueryParams(t *testing.T) {
	e := newNpmSearchEnv(t)
	if _, err := e.repos.Create("npm-params", "npm", "hosted", "public", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	e.publishPackument(t, "npm-params", "param-pkg")

	base, _ := e.search(t, "/npm/npm-params/-/v1/search?text=param&size=20")
	withNew, _ := e.search(t, "/npm/npm-params/-/v1/search?text=param&size=20&group_by=package&order=relevance")
	if base.Total != withNew.Total || len(base.Objects) != len(withNew.Objects) {
		t.Fatalf("新参数改变了 npm 结果：%d/%d vs %d/%d",
			base.Total, len(base.Objects), withNew.Total, len(withNew.Objects))
	}
	if base.Objects[0].Package.Name != withNew.Objects[0].Package.Name {
		t.Fatalf("新参数改变了 npm 首条结果：%s vs %s",
			base.Objects[0].Package.Name, withNew.Objects[0].Package.Name)
	}
}

// sortedJSONKeys 返回 map 的排序键列表。
func sortedJSONKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// equalKeySlices 比较两个键切片。
func equalKeySlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
