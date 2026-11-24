package main

// query 子命令的单元测试：全部使用 httptest 假服务端，不发起真实网络请求。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeQueryEnv 构造一个只认识给定键值的环境读取函数，避免依赖真实环境变量。
func fakeQueryEnv(values map[string]string) queryEnv {
	return queryEnv{getenv: func(key string) string { return values[key] }}
}

// serveQueryJSON 让假服务端按 JSON 返回给定响应体。
func serveQueryJSON(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// runQueryCapture 执行 query 子命令并返回标准输出内容。
func runQueryCapture(t *testing.T, env queryEnv, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runQuery(args, &out, env)
	return out.String(), err
}

// TestQueryReposParsesList 校验 repos 子命令能解析并打印仓库列表。
func TestQueryReposParsesList(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/public/repositories" {
			t.Errorf("路径应为 /api/v1/public/repositories，实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[
			{"name":"maven-public","format":"maven","type":"group","visibility":"public",
			 "aliases":["mp"],"artifactCount":12,"totalSize":2048,"online":true},
			{"name":"raw-private","format":"raw","type":"hosted","visibility":"private"}
		],"total":2}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"repos", "--server", server.URL)
	if err != nil {
		t.Fatalf("query repos 应当成功：%v", err)
	}
	for _, want := range []string{"maven-public", "raw-private", "mp", "2.0 KB", "共 2 个仓库"} {
		if !strings.Contains(out, want) {
			t.Errorf("人类可读输出缺少 %q：\n%s", want, out)
		}
	}
}

// TestQueryLsUsesPrefixParam 是核心用例：tree 必须用 prefix 参数，
// 且服务端返回的完整目录路径在表格里裁成末段。
func TestQueryLsUsesPrefixParam(t *testing.T) {
	var gotQuery string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v1/repositories/maven-public/tree" {
			t.Errorf("路径应为 /api/v1/repositories/maven-public/tree，实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"directories":["org/springframework/spring-core/6.1.0"],
			"files":[{"path":"org/springframework/spring-core/pom.xml","size":512,
			          "hash":"abc","sha1":"def","updatedAt":"2024-01-02T03:04:05Z",
			          "downloadCount":7}]
		}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"ls", "maven-public/org/springframework/spring-core", "--server", server.URL)
	if err != nil {
		t.Fatalf("query ls 应当成功：%v", err)
	}
	// 参数名必须精确是 prefix；写成 path 时服务端会静默返回根目录。
	// 且值必须以 "/" 结尾：服务端按「字符数+1」切层级，不带斜杠时切分起点落在目录名
	// 内部、instr 找不到 "/" 而返回空结果——静默空列表比报错更难发现，故钉死在用例里。
	if gotQuery != "prefix=org%2Fspringframework%2Fspring-core%2F" {
		t.Fatalf("tree 请求参数应为 prefix=org%%2Fspringframework%%2Fspring-core%%2F，实际 %q", gotQuery)
	}
	if strings.Contains(gotQuery, "path=") {
		t.Fatalf("tree 请求不应带 path 参数，实际 %q", gotQuery)
	}
	// 目录显示名裁成末段，但完整路径仍出现在表格里。
	if !strings.Contains(out, "6.1.0") {
		t.Errorf("目录显示名应裁成末段 6.1.0：\n%s", out)
	}
	if !strings.Contains(out, "org/springframework/spring-core/6.1.0") {
		t.Errorf("完整路径应保留：\n%s", out)
	}
	if !strings.Contains(out, "1 个目录、1 个文件") {
		t.Errorf("缺少统计行：\n%s", out)
	}
}

// TestQueryFindMapsRepoAndLimit 校验 find 把 --repo 映射到 repository、--limit 映射到 page_size。
func TestQueryFindMapsRepoAndLimit(t *testing.T) {
	var gotQuery string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[{"repository":"maven-public",
			"path":"org/demo/a.jar","size":100,"hash":"h","sha1":"s",
			"updatedAt":"2024-02-03T04:05:06Z"}],"total":9}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"find", "spring-core", "--repo", "maven-public", "--limit", "20", "--server", server.URL)
	if err != nil {
		t.Fatalf("query find 应当成功：%v", err)
	}
	for _, want := range []string{"q=spring-core", "repository=maven-public", "page_size=20"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("搜索请求参数缺少 %q，实际 %q", want, gotQuery)
		}
	}
	if !strings.Contains(out, "org/demo/a.jar") || !strings.Contains(out, "匹配 9 条") {
		t.Errorf("搜索结果输出不完整：\n%s", out)
	}
}

// TestQueryRejectsHTMLResponse 是核心用例：收到前端页面必须立即报错，绝不当作数据输出。
func TestQueryRejectsHTMLResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body>SPA 首页</body></html>"))
	}))
	t.Cleanup(server.Close)

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"ls", "maven-public/some/path", "--server", server.URL)
	if err == nil {
		t.Fatal("收到 text/html 时必须报错")
	}
	if out != "" {
		t.Errorf("报错时不应输出任何数据，实际输出：%q", out)
	}
	if !strings.Contains(err.Error(), "服务端返回了前端页面而非数据，通常是路径拼写错误；制品路径形如 /repository/{repo}/{path}") {
		t.Errorf("错误消息缺少既定提示语：%v", err)
	}
	if !strings.Contains(err.Error(), "--server") {
		t.Errorf("错误消息应提示用 --server 检查基址：%v", err)
	}
	if code := queryExitCode(err); code != 1 {
		t.Errorf("HTML 响应应映射为退出码 1，实际 %d", code)
	}
}

// TestQueryNotFoundReportsServerError 校验 404 时非零退出、带上服务端错误 JSON 与可操作提示。
func TestQueryNotFoundReportsServerError(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"资源不存在"}}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"ls", "missing-repo", "--server", server.URL)
	if err == nil {
		t.Fatal("404 必须返回错误")
	}
	if out != "" {
		t.Errorf("404 时不应有数据输出，实际 %q", out)
	}
	for _, want := range []string{"404", "not_found", "资源不存在", "jianartifact query repos"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("404 错误消息缺少 %q：%v", want, err)
		}
	}
	if code := queryExitCode(err); code != 1 {
		t.Errorf("404 应映射为退出码 1，实际 %d", code)
	}
}

// TestQueryUnauthorizedHintsToken 校验 401 提示补 --token 或 JIAN_TOKEN。
func TestQueryUnauthorizedHintsToken(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"未认证"}}`))
	})

	_, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"repos", "--server", server.URL)
	if err == nil {
		t.Fatal("401 必须返回错误")
	}
	if !strings.Contains(err.Error(), "--token") || !strings.Contains(err.Error(), envQueryToken) {
		t.Errorf("401 错误消息应提示 --token 或 %s：%v", envQueryToken, err)
	}
}

// TestQueryTokenAndServerFromEnv 校验环境变量提供 base URL 与令牌。
func TestQueryTokenAndServerFromEnv(t *testing.T) {
	var gotAuth string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})

	env := fakeQueryEnv(map[string]string{
		envQueryServer: server.URL,
		envQueryToken:  "secret-token",
	})
	if _, err := runQueryCapture(t, env, "repos"); err != nil {
		t.Fatalf("依环境变量执行 repos 应当成功：%v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("应带上 Bearer 令牌，实际 %q", gotAuth)
	}
}

// TestQueryJSONFieldSelection 校验 --json 字段列表：对数组逐条筛选，缺失字段填 null。
func TestQueryJSONFieldSelection(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"name":"a","format":"maven","type":"hosted"},
			{"name":"b","format":"raw"}
		],"total":2}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"repos", "--server", server.URL, "--json", "name,type")
	if err != nil {
		t.Fatalf("--json 字段筛选应当成功：%v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("输出应为 JSON 数组：%v\n%s", err, out)
	}
	if len(decoded) != 2 {
		t.Fatalf("应得到 2 条记录，实际 %d：\n%s", len(decoded), out)
	}
	if decoded[0]["name"] != "a" || decoded[0]["type"] != "hosted" {
		t.Errorf("第 1 条字段筛选不正确：%v", decoded[0])
	}
	if _, present := decoded[0]["format"]; present {
		t.Errorf("未请求的字段不应出现：%v", decoded[0])
	}
	if value, present := decoded[1]["type"]; !present || value != nil {
		t.Errorf("缺失字段应以 null 出现，实际 %v（存在=%t）", value, present)
	}
}

// TestQueryJSONWholeObject 校验无字段列表的 --json 输出完整对象。
func TestQueryJSONWholeObject(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"name":"a","format":"maven","type":"hosted"}],"total":1}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"repos", "--server", server.URL, "--json")
	if err != nil {
		t.Fatalf("--json 完整输出应当成功：%v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("输出应为 JSON 对象：%v\n%s", err, out)
	}
	if _, present := decoded["items"]; !present {
		t.Errorf("完整对象应保留 items：\n%s", out)
	}
	if decoded["total"] != float64(1) {
		t.Errorf("完整对象应保留 total，实际 %v", decoded["total"])
	}
}

// TestQueryJQFiltersOutput 校验 --jq 过滤生效。
func TestQueryJQFiltersOutput(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"path":"a/one.jar","size":1},
			{"path":"a/two.jar","size":2}
		],"total":2}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"find", "jar", "--server", server.URL, "--jq", ".items[].path")
	if err != nil {
		t.Fatalf("--jq 过滤应当成功：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != `"a/one.jar"` || lines[1] != `"a/two.jar"` {
		t.Errorf("--jq 应逐条输出路径，实际：\n%s", out)
	}
}

// TestQueryGetMatchesFileAndBuildsDownloadURL 校验 get 在父目录 tree 中精确匹配并拼出下载地址。
func TestQueryGetMatchesFileAndBuildsDownloadURL(t *testing.T) {
	var gotQuery string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"directories":[],"files":[
			{"path":"org/demo/1.0/demo-1.0.jar","size":4096,"hash":"blob",
			 "sha1":"s1","md5":"m1","contentType":"application/java-archive",
			 "createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z",
			 "downloadCount":3},
			{"path":"org/demo/1.0/demo-1.0.pom","size":1024,"hash":"blob2"}
		]}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"get", "maven-public/org/demo/1.0/demo-1.0.jar", "--server", server.URL)
	if err != nil {
		t.Fatalf("query get 应当成功：%v", err)
	}
	if gotQuery != "prefix=org%2Fdemo%2F1.0%2F" {
		t.Fatalf("get 应向父目录要 tree（prefix=org%%2Fdemo%%2F1.0%%2F），实际 %q", gotQuery)
	}
	for _, want := range []string{
		"org/demo/1.0/demo-1.0.jar",
		server.URL + "/repository/maven-public/org/demo/1.0/demo-1.0.jar",
		"4.0 KB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("get 输出缺少 %q：\n%s", want, out)
		}
	}
}

// TestQueryGetMissingFileFails 校验 get 未找到文件时非零退出并给出下一步提示。
func TestQueryGetMissingFileFails(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"directories":[],"files":[]}`))
	})

	_, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"get", "maven-public/org/demo/1.0/nope.jar", "--server", server.URL)
	if err == nil {
		t.Fatal("未找到文件时必须报错")
	}
	if !strings.Contains(err.Error(), "query ls") {
		t.Errorf("未找到时应提示用 query ls 确认文件名：%v", err)
	}
	if code := queryExitCode(err); code != 1 {
		t.Errorf("未找到文件应映射为退出码 1，实际 %d", code)
	}
}

// TestQueryGetJSONOutputsSingleFile 校验 get --json 输出的是匹配到的那一条制品（含下载地址），
// 而不是整份目录树。
func TestQueryGetJSONOutputsSingleFile(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"directories":["org/demo/1.0"],"files":[
			{"path":"org/demo/1.0/other.jar","size":1},
			{"path":"org/demo/1.0/target.jar","size":2048,"hash":"blob","downloadCount":5}
		]}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"get", "maven-public/org/demo/1.0/target.jar", "--server", server.URL, "--json")
	if err != nil {
		t.Fatalf("get --json 应当成功：%v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("输出应为单个 JSON 对象：%v\n%s", err, out)
	}
	if decoded["path"] != "org/demo/1.0/target.jar" {
		t.Errorf("应输出目标制品，实际 %v", decoded)
	}
	wantURL := server.URL + "/repository/maven-public/org/demo/1.0/target.jar"
	if decoded["downloadUrl"] != wantURL {
		t.Errorf("应带上拼好的下载地址 %q，实际 %v", wantURL, decoded["downloadUrl"])
	}
	if _, present := decoded["files"]; present {
		t.Errorf("不应输出整份目录树：%v", decoded)
	}

	// 字段列表同样作用在单条制品上。
	filtered, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"get", "maven-public/org/demo/1.0/target.jar", "--server", server.URL, "--json", "path,downloadUrl")
	if err != nil {
		t.Fatalf("get --json 字段筛选应当成功：%v", err)
	}
	var picked map[string]any
	if err := json.Unmarshal([]byte(filtered), &picked); err != nil {
		t.Fatalf("输出应为 JSON 对象：%v\n%s", err, filtered)
	}
	if len(picked) != 2 || picked["path"] == nil || picked["downloadUrl"] == nil {
		t.Errorf("字段筛选应只留 path 与 downloadUrl，实际 %v", picked)
	}
}

// TestQueryUsagePrintsSnippets 校验 usage 子命令解析片段。
func TestQueryUsagePrintsSnippets(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repositories/maven-public/usage" {
			t.Errorf("路径应为 usage 端点，实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"format":"maven","type":"hosted","snippets":[
			{"group":"resolve","title":"Maven 解析依赖","tool":"maven",
			 "description":"加到 pom.xml","code":"<dependency>demo</dependency>"}
		]}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"usage", "maven-public", "--server", server.URL)
	if err != nil {
		t.Fatalf("query usage 应当成功：%v", err)
	}
	for _, want := range []string{"maven", "resolve", "Maven 解析依赖", "<dependency>demo</dependency>"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage 输出缺少 %q：\n%s", want, out)
		}
	}
}

// TestQueryAPIPassesMethodBodyAndEscapesQuery 校验万能兜底：方法、请求体、查询串参数。
func TestQueryAPIPassesMethodBodyAndEscapesQuery(t *testing.T) {
	var gotMethod, gotBody, gotQuery, gotPath string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"api", "/api/v1/search?q=a b&page_size=5", "--server", server.URL,
		"-X", "POST", "-d", `{"ping":1}`, "--jq", ".ok")
	if err != nil {
		t.Fatalf("query api 应当成功：%v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("-X 应把方法设为 POST，实际 %s", gotMethod)
	}
	if gotPath != "/api/v1/search" {
		t.Errorf("路径应为 /api/v1/search，实际 %s", gotPath)
	}
	// 查询串由 net/url 重新编码：空格必须转成 + 或 %20，而不是原样发出。
	if !strings.Contains(gotQuery, "q=a+b") && !strings.Contains(gotQuery, "q=a%20b") {
		t.Errorf("查询串应被正确转义，实际 %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "page_size=5") {
		t.Errorf("查询串应保留 page_size，实际 %q", gotQuery)
	}
	if gotBody != `{"ping":1}` {
		t.Errorf("-d 请求体应原样发送，实际 %q", gotBody)
	}
	if strings.TrimSpace(out) != "true" {
		t.Errorf("--jq .ok 应输出 true，实际 %q", out)
	}
}

// TestQueryAPIRequiresLeadingSlash 校验 api 的 path 必须以 / 开头（用法错误，退出码 2）。
func TestQueryAPIRequiresLeadingSlash(t *testing.T) {
	_, err := runQueryCapture(t, fakeQueryEnv(map[string]string{envQueryServer: "https://example.invalid"}),
		"api", "api/v1/status")
	if err == nil {
		t.Fatal("path 不以 / 开头时应报用法错误")
	}
	if code := queryExitCode(err); code != 2 {
		t.Errorf("用法错误应映射为退出码 2，实际 %d（%v）", code, err)
	}
}

// TestQueryServerValidation 校验基址校验：缺 scheme、带 /api/v1 都要快速失败。
func TestQueryServerValidation(t *testing.T) {
	cases := []struct {
		name   string
		server string
	}{
		{name: "缺少 scheme", server: "jian.example.com"},
		{name: "基址带 /api/v1", server: "https://jian.example.com/api/v1"},
		{name: "基址带 /repository", server: "https://jian.example.com/repository"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := runQueryCapture(t, fakeQueryEnv(map[string]string{envQueryServer: testCase.server}), "repos")
			if err == nil {
				t.Fatalf("基址 %q 应被拒绝", testCase.server)
			}
			if code := queryExitCode(err); code != 2 {
				t.Errorf("基址校验失败应映射为退出码 2，实际 %d（%v）", code, err)
			}
		})
	}
}

// TestQueryMissingServerIsUsageError 校验未提供基址时给出用 --server 的用法错误。
func TestQueryMissingServerIsUsageError(t *testing.T) {
	_, err := runQueryCapture(t, queryEnv{
		getenv:     func(string) string { return "" },
		configPath: filepath.Join(t.TempDir(), "absent.json"),
	}, "repos")
	if err == nil {
		t.Fatal("缺少 --server 时应报错")
	}
	if !strings.Contains(err.Error(), "--server") || !strings.Contains(err.Error(), envQueryServer) {
		t.Errorf("错误应提示 --server 或 %s：%v", envQueryServer, err)
	}
	if code := queryExitCode(err); code != 2 {
		t.Errorf("缺参数应映射为退出码 2，实际 %d", code)
	}
}

// TestQueryReadsConfigFile 校验可选配置文件提供 server/token/timeout。
func TestQueryReadsConfigFile(t *testing.T) {
	var gotAuth string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})

	configPath := filepath.Join(t.TempDir(), "config.json")
	content := `{"server":"` + server.URL + `","token":"file-token","timeout":"5s"}`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置：%v", err)
	}

	env := queryEnv{getenv: func(string) string { return "" }, configPath: configPath}
	if _, err := runQueryCapture(t, env, "repos"); err != nil {
		t.Fatalf("按配置文件查询应当成功：%v", err)
	}
	if gotAuth != "Bearer file-token" {
		t.Errorf("应使用配置文件里的令牌，实际 %q", gotAuth)
	}
}

// TestQueryRepoSpecSplit 校验 <repo>[/<prefix>] 与父目录前缀的拆分规则。
func TestQueryRepoSpecSplit(t *testing.T) {
	cases := []struct {
		spec       string
		wantRepo   string
		wantPrefix string
	}{
		{spec: "maven-public", wantRepo: "maven-public", wantPrefix: ""},
		{spec: "maven-public/", wantRepo: "maven-public", wantPrefix: ""},
		{spec: "maven-public/org/demo", wantRepo: "maven-public", wantPrefix: "org/demo"},
		{spec: "/maven-public/org/demo/", wantRepo: "maven-public", wantPrefix: "org/demo"},
	}
	for _, testCase := range cases {
		repo, prefix := splitQueryRepoSpec(testCase.spec)
		if repo != testCase.wantRepo || prefix != testCase.wantPrefix {
			t.Errorf("%q 拆分应得 repo=%q prefix=%q，实际 repo=%q prefix=%q",
				testCase.spec, testCase.wantRepo, testCase.wantPrefix, repo, prefix)
		}
	}

	parentCases := map[string]string{
		"a.jar":             "",
		"org/demo/a.jar":    "org/demo",
		"/org/demo/a.jar":   "org/demo",
		"org/demo/deep/a.j": "org/demo/deep",
	}
	for path, want := range parentCases {
		if got := queryParentPrefix(path); got != want {
			t.Errorf("%q 的父目录应为 %q，实际 %q", path, want, got)
		}
	}
}

// TestQueryRejectsNon2xxWithoutJSON 校验非 2xx 且响应体非 JSON 时仍非零退出。
func TestQueryRejectsNon2xxWithoutJSON(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})

	_, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"repos", "--server", server.URL)
	if err == nil {
		t.Fatal("500 必须返回错误")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("错误消息应包含状态码 500：%v", err)
	}
	if code := queryExitCode(err); code != 1 {
		t.Errorf("500 应映射为退出码 1，实际 %d", code)
	}
}

// TestQueryJSONFlagAmbiguity 校验 --json 的可选取值判定：
// 紧随其后的令牌只有在「摘掉它仍满足位置参数个数」时才被当作字段列表。
func TestQueryJSONFlagAmbiguity(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.12.0"}`))
	})

	// api 的 path 必须留在位置参数里，不能被 --json 吃掉。
	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"api", "--json", "/api/v1/status", "--server", server.URL)
	if err != nil {
		t.Fatalf("api --json <path> 应当成功：%v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("应输出完整 JSON 对象：%v\n%s", err, out)
	}
	if decoded["version"] != "0.12.0" {
		t.Errorf("应输出完整对象，实际 %v", decoded)
	}

	// repos 没有位置参数，紧跟的令牌就应被当作字段列表。
	repoServer := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"name":"a","format":"maven","type":"hosted"}],"total":1}`))
	})
	picked, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"repos", "--json", "name", "--server", repoServer.URL)
	if err != nil {
		t.Fatalf("repos --json <字段> 应当成功：%v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(picked), &items); err != nil {
		t.Fatalf("应输出 JSON 数组：%v\n%s", err, picked)
	}
	if len(items) != 1 || len(items[0]) != 1 || items[0]["name"] != "a" {
		t.Errorf("应只留 name 字段，实际 %v", items)
	}
}

// TestQueryHelpPrintsUsage 校验 query help 打印用法且不访问网络。
func TestQueryHelpPrintsUsage(t *testing.T) {
	out, err := runQueryCapture(t, fakeQueryEnv(nil), "help")
	if err != nil {
		t.Fatalf("query help 应当成功：%v", err)
	}
	for _, want := range []string{"query repos", "query ls", "query find", "--jq EXPR", envQueryServer} {
		if !strings.Contains(out, want) {
			t.Errorf("帮助缺少 %q：\n%s", want, out)
		}
	}
}

// TestQueryTreePrefixNormalization 钉住 tree 端点的 prefix 归一化：
// 服务端按「字符数(prefix)+1」切层级，故列出某目录内容必须带尾部 "/"；
// 不带斜杠不会报错、只会静默返回空列表，正是最需要由 CLI 兜住的形态。
func TestQueryTreePrefixNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},                          // 仓库根：空前缀
		{"/", ""},                         // 只有斜杠也归一到根
		{"org/demo", "org/demo/"},         // 不带斜杠 → 补上
		{"org/demo/", "org/demo/"},        // 已带斜杠 → 不重复补
		{"/org/demo//", "org/demo/"},      // 首尾多余斜杠被裁掉
		{"org/demo/1.0", "org/demo/1.0/"}, // 多层路径
	}
	for _, c := range cases {
		if got := queryTreePrefix(c.in); got != c.want {
			t.Errorf("queryTreePrefix(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestQueryConfigTokenNotSentToOtherServer 校验切换 --server 时不再沿用配置文件里的令牌：
// 令牌属于配置文件登记的那台实例，发给另一实例等于把凭据交给第三方。
func TestQueryConfigTokenNotSentToOtherServer(t *testing.T) {
	var gotAuth string
	other := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})

	configPath := filepath.Join(t.TempDir(), "config.json")
	content := `{"server":"https://home.example","token":"home-token"}`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置：%v", err)
	}
	env := queryEnv{getenv: func(string) string { return "" }, configPath: configPath}
	if _, err := runQueryCapture(t, env, "repos", "--server", other.URL); err != nil {
		t.Fatalf("查询其他实例应当成功：%v", err)
	}
	if gotAuth != "" {
		t.Errorf("切换到未登记的实例时不应携带配置文件令牌，实际 %q", gotAuth)
	}
}

// TestQueryTokenRejectsPlainHTTP 校验带令牌时拒绝非回环的明文 HTTP 目标。
func TestQueryTokenRejectsPlainHTTP(t *testing.T) {
	env := fakeQueryEnv(map[string]string{envQueryServer: "http://jian.example.com", envQueryToken: "tok"})
	_, err := runQueryCapture(t, env, "repos")
	if err == nil {
		t.Fatal("带令牌请求明文 HTTP 应被拒绝")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("错误应指明明文 HTTP 被拒，实际 %v", err)
	}
}

// TestQueryAPIEmptyBodyReportsAccepted 校验空响应体（204/202）不再按 JSON 解析失败谎报。
func TestQueryAPIEmptyBodyReportsAccepted(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	out, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"api", "/api/v1/repositories/demo", "--server", server.URL, "-X", "DELETE")
	if err != nil {
		t.Fatalf("204 空响应体应视为成功：%v", err)
	}
	if !strings.Contains(out, "204") {
		t.Errorf("输出应报告状态码，实际 %q", out)
	}
}

// TestQueryEndpointDoesNotDoubleEscape 校验含转义段的路径不会被二次转义。
func TestQueryEndpointDoesNotDoubleEscape(t *testing.T) {
	var gotEscaped, gotPath string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotEscaped, gotPath = r.URL.EscapedPath(), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if _, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"api", "/api/v1/repositories/a%20b/status", "--server", server.URL); err != nil {
		t.Fatalf("query api 应当成功：%v", err)
	}
	if gotPath != "/api/v1/repositories/a b/status" {
		t.Errorf("解码后路径应为 a b，实际 %q", gotPath)
	}
	// 二次转义时服务端看到的是字面 %2520（Path 里带 %20），单次转义则是 %20。
	if gotEscaped != "/api/v1/repositories/a%20b/status" {
		t.Errorf("转义后路径应保留单次转义，实际 %q", gotEscaped)
	}
}

// TestQueryEndpointBasePathNotDoubleDecoded 校验基址里字面 %20 不会被再次解码：
// 只有 CLI 生成的 endpointPath 需要解码，base.Path 已是解码形态。
func TestQueryEndpointBasePathNotDoubleDecoded(t *testing.T) {
	var gotEscaped, gotPath string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotEscaped, gotPath = r.URL.EscapedPath(), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// 基址写 %2520 表示路径段里字面含 %20：解码一次得 "/sub%20dir"，
	// 若被再次解码会变成 "/sub dir"——正是要防的重复解码。
	if _, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"api", "/api/v1/repositories/a%20b/status", "--server", server.URL+"/sub%2520dir"); err != nil {
		t.Fatalf("query api 应当成功：%v", err)
	}
	if gotPath != "/sub%20dir/api/v1/repositories/a b/status" {
		t.Errorf("解码后路径应保留字面 %%20，实际 %q", gotPath)
	}
	if gotEscaped != "/sub%2520dir/api/v1/repositories/a%20b/status" {
		t.Errorf("应保留单次转义，实际 %q", gotEscaped)
	}
}

// TestQueryConfigTokenSameServerWithDefaultPort 校验显式默认端口不判为切换实例。
func TestQueryConfigTokenSameServerWithDefaultPort(t *testing.T) {
	var gotAuth string
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	configPath := filepath.Join(t.TempDir(), "config.json")
	content := `{"server":"` + server.URL + `","token":"file-token"}`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置：%v", err)
	}
	env := queryEnv{getenv: func(string) string { return "" }, configPath: configPath}
	if _, err := runQueryCapture(t, env, "repos", "--server", server.URL); err != nil {
		t.Fatalf("同实例查询应当成功：%v", err)
	}
	if gotAuth != "Bearer file-token" {
		t.Errorf("同实例应沿用配置文件令牌，实际 %q", gotAuth)
	}
}

// TestCanonicalQueryBaseStripsDefaultPort 校验默认端口规范化：显式 :443/:80 与省略等价。
func TestCanonicalQueryBaseStripsDefaultPort(t *testing.T) {
	cases := []struct{ a, b string }{
		{"https://h.example", "https://h.example:443"},
		{"http://h.example", "http://h.example:80"},
		{"https://h.example/p", "https://h.example:443/p/"},
	}
	for i, testCase := range cases {
		ua, err := url.Parse(testCase.a)
		if err != nil {
			t.Fatalf("case %d 解析 %q：%v", i, testCase.a, err)
		}
		ub, err := url.Parse(testCase.b)
		if err != nil {
			t.Fatalf("case %d 解析 %q：%v", i, testCase.b, err)
		}
		if canonicalQueryBase(ua) != canonicalQueryBase(ub) {
			t.Errorf("case %d：%q 与 %q 规范化后应相等（%q != %q）", i, testCase.a, testCase.b, canonicalQueryBase(ua), canonicalQueryBase(ub))
		}
	}
}

// TestQueryReplyOverLimitFails 校验超限响应报错而不是静默截断。
func TestQueryReplyOverLimitFails(t *testing.T) {
	server := serveQueryJSON(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", queryMaxReplyBytes+100)))
	})
	_, err := runQueryCapture(t, queryEnv{getenv: func(string) string { return "" }},
		"api", "/api/v1/big", "--server", server.URL)
	if err == nil {
		t.Fatal("超限响应应报错而非输出截断内容")
	}
}
