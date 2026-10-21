package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住 FR-150 §3.7 的**响应契约**：
//
//   - `group_by=file`（缺省）下响应形状与改造前逐字段一致，只多出 items[].sha1 这个纯追加键；
//   - `group_by=package` 才追加 packages / packageTotal / aggregation / appliedOrder；
//   - file 模式下收到 order=relevance 会静默回落 path asc 并用 appliedOrder 回报；
//   - packages 的三层结构与计数口径按规格 JSON 示例。

// searchAPIEnv 是搜索响应测试的最小环境。
type searchAPIEnv struct {
	handlers *Handlers
	svc      *domain.RepositoryService
	repos    *repository.RepoRepo
	assets   *repository.AssetRepo
}

// newSearchAPIEnv 建库、装配处理器，并以管理员身份注入主体。
func newSearchAPIEnv(t *testing.T) *searchAPIEnv {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	users := repository.NewUserRepo(db)
	if _, err := users.Create("admin", "hash", "admin"); err != nil {
		t.Fatalf("创建 admin：%v", err)
	}
	repoRepo := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	settings := domain.NewSettingService(repository.NewSettingRepo(db))
	svc := domain.NewRepositoryService(repoRepo, repository.NewAclRepo(db), assets, settings, users)
	return &searchAPIEnv{
		handlers: NewHandlers(Deps{Repos: svc, Settings: settings}),
		svc:      svc,
		repos:    repoRepo,
		assets:   assets,
	}
}

// seedMaven 建一个 maven 仓库并写入语料。
func (e *searchAPIEnv) seedMaven(t *testing.T, name string, paths ...string) int64 {
	t.Helper()
	repoID, err := e.repos.Create(name, "maven", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建仓库 %s：%v", name, err)
	}
	for i, path := range paths {
		// 时间按序号递增，便于验证「无版本信号时按 updated desc」。
		updated := "2026-10-1" + string(rune('0'+i%10)) + " 09:00:00"
		if err := e.assets.UpsertWithTime(repoID, path, "h", int64(100+i), "application/octet-stream",
			"sha1-"+path[:1], "md5", updated, updated); err != nil {
			t.Fatalf("写入资产 %s：%v", path, err)
		}
	}
	return repoID
}

// serve 以管理员身份发起一次 /api/v1/search 请求。
func (e *searchAPIEnv) serve(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{UserID: 1, Role: "admin", Kind: auth.KindSession})
	})
	router.GET("/api/v1/search", e.handlers.SearchAssets)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?"+query, nil)
	router.ServeHTTP(rec, req)
	return rec
}

// serveJSON 发起请求并把响应体解析为键值映射。
func (e *searchAPIEnv) serveJSON(t *testing.T, query string) map[string]json.RawMessage {
	t.Helper()
	rec := e.serve(t, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("请求 %s 状态码 = %d（体：%s）", query, rec.Code, rec.Body.String())
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败：%v（体：%s）", err, rec.Body.String())
	}
	return out
}

// TestSearchFileModeResponseShapeUnchanged 是**回归保护**：
// group_by 缺省（file）时顶层键集必须仍是 items / total / facets，排序仍是 path asc。
func TestSearchFileModeResponseShapeUnchanged(t *testing.T) {
	e := newSearchAPIEnv(t)
	e.seedMaven(t, "shape-maven",
		"com/example/foo/1.17.12/foo-1.17.12.jar",
		"com/example/foo/1.17.12/foo-1.17.12.pom",
		"com/example/foo/1.17.120/foo-1.17.120.jar",
	)

	out := e.serveJSON(t, "q=1.17.12")
	keys := sortedKeys(out)
	want := []string{"facets", "items", "total"}
	if len(keys) != len(want) {
		t.Fatalf("顶层键集 = %v，期望 %v（file 模式不得新增字段）", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("顶层键集 = %v，期望 %v", keys, want)
		}
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(out["items"], &items); err != nil {
		t.Fatalf("解析 items：%v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items 条数 = %d，期望 3", len(items))
	}
	// 每个 item 的键集：既有的 5 个键 + 纯追加的 sha1。
	wantItemKeys := []string{"hash", "path", "repository", "sha1", "size", "updatedAt"}
	if got := sortedKeys(items[0]); !equalStrings(got, wantItemKeys) {
		t.Fatalf("item 键集 = %v，期望 %v", got, wantItemKeys)
	}
	// 排序仍是 path asc（仓库内搜索依赖它建树）。
	var paths []string
	for _, item := range items {
		var path string
		if err := json.Unmarshal(item["path"], &path); err != nil {
			t.Fatalf("解析 path：%v", err)
		}
		paths = append(paths, path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("file 模式默认排序应为 path asc，得 %v", paths)
	}
	// sha1 是 asset.sha1 的真值，不是 hash 的别名。
	var sha1 string
	if err := json.Unmarshal(items[0]["sha1"], &sha1); err != nil {
		t.Fatalf("解析 sha1：%v", err)
	}
	if sha1 != "sha1-c" {
		t.Fatalf("sha1 = %q，期望 asset.sha1（sha1-c）", sha1)
	}

	// 与领域层文件级搜索逐条一致（items / total / facets 三处）。
	direct, err := e.svc.SearchAssets(t.Context(), "1.17.12", "", 0, true, "", "asc", 20, 0)
	if err != nil {
		t.Fatalf("领域层搜索失败：%v", err)
	}
	var total int
	if err := json.Unmarshal(out["total"], &total); err != nil {
		t.Fatalf("解析 total：%v", err)
	}
	if total != direct.Total || len(items) != len(direct.Items) {
		t.Fatalf("响应 total/items = %d/%d，期望 %d/%d", total, len(items), direct.Total, len(direct.Items))
	}
}

// TestSearchPackageModeResponse 钉住 group_by=package 的响应结构（规格 §3.7 的 JSON 示例）。
func TestSearchPackageModeResponse(t *testing.T) {
	e := newSearchAPIEnv(t)
	e.seedMaven(t, "agg-maven",
		"com/example/foo/1.17.12/foo-1.17.12.jar",
		"com/example/foo/1.17.12/foo-1.17.12.pom",
		"com/example/foo/1.17.12-rc1/foo-1.17.12-rc1.jar",
		"com/example/foo/1.17.120/foo-1.17.120.jar",
	)
	rawID, err := e.repos.Create("agg-raw", "raw", "hosted", "public", "")
	if err != nil {
		t.Fatalf("建 raw 仓库：%v", err)
	}
	if err := e.assets.Upsert(rawID, "docs/1.17.12/notes.txt", "h", 7, "text/plain", "raw-sha1", "m"); err != nil {
		t.Fatalf("写入裸路径：%v", err)
	}

	out := e.serveJSON(t, "q=1.17.12&group_by=package&order=relevance")
	keys := sortedKeys(out)
	want := []string{"aggregation", "appliedOrder", "facets", "items", "packageTotal", "packages", "total"}
	if !equalStrings(keys, want) {
		t.Fatalf("顶层键集 = %v，期望 %v", keys, want)
	}

	var total, packageTotal int
	if err := json.Unmarshal(out["total"], &total); err != nil {
		t.Fatalf("解析 total：%v", err)
	}
	if err := json.Unmarshal(out["packageTotal"], &packageTotal); err != nil {
		t.Fatalf("解析 packageTotal：%v", err)
	}
	if total != 5 || packageTotal != 2 {
		t.Fatalf("total/packageTotal = %d/%d，期望 5/2（foo 四条 + raw 一条；两个包节点）", total, packageTotal)
	}

	var appliedOrder string
	if err := json.Unmarshal(out["appliedOrder"], &appliedOrder); err != nil {
		t.Fatalf("解析 appliedOrder：%v", err)
	}
	if appliedOrder != "relevance" {
		t.Fatalf("appliedOrder = %q，期望 relevance", appliedOrder)
	}

	var aggregation struct {
		Scanned   int  `json:"scanned"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(out["aggregation"], &aggregation); err != nil {
		t.Fatalf("解析 aggregation：%v", err)
	}
	if aggregation.Scanned != 5 || aggregation.Truncated {
		t.Fatalf("aggregation = %+v，期望 scanned=5 truncated=false", aggregation)
	}

	var packages []struct {
		Coordinate    string   `json:"coordinate"`
		Kind          string   `json:"kind"`
		Repositories  []string `json:"repositories"`
		VersionCount  int      `json:"versionCount"`
		FileCount     int      `json:"fileCount"`
		BestPrecision string   `json:"bestPrecision"`
		TopVersion    string   `json:"topVersion"`
		Versions      []struct {
			Version   string `json:"version"`
			Precision string `json:"precision"`
			FileCount int    `json:"fileCount"`
			Size      int64  `json:"size"`
			Files     []struct {
				Name       string `json:"name"`
				Path       string `json:"path"`
				Repository string `json:"repository"`
				Size       int64  `json:"size"`
				Sha1       string `json:"sha1"`
				UpdatedAt  string `json:"updatedAt"`
				FileKind   string `json:"fileKind"`
			} `json:"files"`
		} `json:"versions"`
		Files []struct {
			Name     string `json:"name"`
			FileKind string `json:"fileKind"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out["packages"], &packages); err != nil {
		t.Fatalf("解析 packages：%v", err)
	}
	if len(packages) != 2 {
		t.Fatalf("packages 条数 = %d，期望 2", len(packages))
	}
	// relevance 下精确包在前、无坐标伪包在后。
	foo := packages[0]
	if foo.Coordinate != "com.example:foo" || foo.Kind != "maven" {
		t.Fatalf("首个包 = %s/%s，期望 com.example:foo/maven", foo.Kind, foo.Coordinate)
	}
	if foo.BestPrecision != domain.PrecisionExact || foo.TopVersion != "1.17.12" {
		t.Fatalf("首个包 precision/topVersion = %s/%s，期望 exact/1.17.12", foo.BestPrecision, foo.TopVersion)
	}
	if foo.VersionCount != 3 || foo.FileCount != 4 {
		t.Fatalf("首个包 versionCount/fileCount = %d/%d，期望 3/4", foo.VersionCount, foo.FileCount)
	}
	if len(foo.Repositories) != 1 || foo.Repositories[0] != "agg-maven" {
		t.Fatalf("首个包 repositories = %v，期望 [agg-maven]", foo.Repositories)
	}
	wantVersions := []struct{ version, precision string }{
		{"1.17.12", domain.PrecisionExact},
		{"1.17.12-rc1", domain.PrecisionPrerelease},
		{"1.17.120", domain.PrecisionSubstring},
	}
	for i, want := range wantVersions {
		got := foo.Versions[i]
		if got.Version != want.version || got.Precision != want.precision {
			t.Fatalf("第 %d 个版本 = %s[%s]，期望 %s[%s]", i, got.Version, got.Precision, want.version, want.precision)
		}
		if got.FileCount != len(got.Files) {
			t.Fatalf("版本 %s 的 fileCount=%d 与 files 长度 %d 不一致", got.Version, got.FileCount, len(got.Files))
		}
	}
	// 文件行字段齐全（详情面板要用 sha1）。
	jar := foo.Versions[0].Files[0]
	if jar.Name != "foo-1.17.12.jar" || jar.FileKind != "jar" || jar.Sha1 != "sha1-c" || jar.Repository != "agg-maven" {
		t.Fatalf("首个文件 = %+v，期望 jar 且 sha1/repository 齐全", jar)
	}

	// 无坐标伪包：坐标即仓库名、kind=file、跳过版本层、文件直挂包下。
	raw := packages[1]
	if raw.Coordinate != "agg-raw" || raw.Kind != domain.PackageKindFile {
		t.Fatalf("伪包 = %s/%s，期望 agg-raw/file", raw.Kind, raw.Coordinate)
	}
	if raw.VersionCount != 0 || len(raw.Versions) != 0 {
		t.Fatalf("伪包应跳过版本层：versionCount=%d versions=%d", raw.VersionCount, len(raw.Versions))
	}
	if raw.FileCount != 1 || len(raw.Files) != 1 {
		t.Fatalf("伪包文件 = %d/%d，期望 1/1", raw.FileCount, len(raw.Files))
	}
	if raw.BestPrecision != domain.PrecisionNone {
		t.Fatalf("伪包 bestPrecision = %q，期望 none", raw.BestPrecision)
	}

	// 有坐标的包不输出包级 files 键（文件都在 versions[].files 里），
	// 使 JSON 形状与规格 §3.7 的示例一致。
	var rawPackages []map[string]json.RawMessage
	if err := json.Unmarshal(out["packages"], &rawPackages); err != nil {
		t.Fatalf("解析 packages 原始键：%v", err)
	}
	if _, ok := rawPackages[0]["files"]; ok {
		t.Fatal("有坐标的包不应输出 files 键")
	}
	if _, ok := rawPackages[1]["files"]; !ok {
		t.Fatal("无坐标伪包必须输出 files 键（文件直挂包下）")
	}
}

// TestSearchRelevanceFallsBackInFileMode 钉住 §3.5：group_by=file 下 order=relevance
// 静默回落 path asc，并用 appliedOrder 回报实际生效值。
func TestSearchRelevanceFallsBackInFileMode(t *testing.T) {
	e := newSearchAPIEnv(t)
	e.seedMaven(t, "fallback-maven",
		"com/example/zzz/2.0.0/zzz-2.0.0.jar",
		"com/example/aaa/1.0.0/aaa-1.0.0.jar",
	)
	out := e.serveJSON(t, "q=.jar&order=relevance")
	var appliedOrder string
	if err := json.Unmarshal(out["appliedOrder"], &appliedOrder); err != nil {
		t.Fatalf("解析 appliedOrder：%v（体：%v）", err, out)
	}
	if appliedOrder != "asc" {
		t.Fatalf("appliedOrder = %q，期望 asc（回落 path asc）", appliedOrder)
	}
	// 确实是 path asc，而不是 updated desc 或包序。
	var items []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(out["items"], &items); err != nil {
		t.Fatalf("解析 items：%v", err)
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.Path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("回落之后应是 path asc，得 %v", paths)
	}
	// 回落模式下不输出聚合字段。
	if _, ok := out["packages"]; ok {
		t.Fatal("file 模式不得输出 packages")
	}
	if _, ok := out["packageTotal"]; ok {
		t.Fatal("file 模式不得输出 packageTotal")
	}
}

// TestSearchAppliedOrderOmittedByDefault 钉住「普通排序请求不带 appliedOrder」，
// 避免 file 模式的响应形状被无谓地改写。
func TestSearchAppliedOrderOmittedByDefault(t *testing.T) {
	e := newSearchAPIEnv(t)
	e.seedMaven(t, "plain-maven", "com/example/foo/1.0.0/foo-1.0.0.jar")
	for _, query := range []string{"q=foo", "q=foo&sort=updated&order=desc", "q=foo&sort=name"} {
		out := e.serveJSON(t, query)
		if _, ok := out["appliedOrder"]; ok {
			t.Fatalf("请求 %s 不应输出 appliedOrder", query)
		}
	}
}

// sortedKeys 返回 map 的排序键列表。
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// equalStrings 比较两个字符串切片是否逐项相等。
func equalStrings(a, b []string) bool {
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
