package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件钉住 FR-149 §3.6 的 npm `registrySearch` 放大修复：
//
//   - **语义不变**：group 多成员时结果按 `asset.path` 去重、按成员顺序聚合、`total` 口径不变；
//   - **只发一次搜索**：成员数增加不得让请求的 SQL 语句数增长（原本每个成员各跑一次完整搜索，
//     每次含 COUNT + 分页 SELECT + facet 聚合 + 可搜范围计算）。
//
// 计数用驱动级语句统计（persistence.OpenCountingForTest），比「耗时」或「语料规模」更可靠：
// 后者受机器负载影响，无法断言「没有随成员数增长」。

// npmSearchEnv 是带语句计数的 npm 协议测试环境。
type npmSearchEnv struct {
	engine  *gin.Engine
	assets  *domain.AssetService
	repos   *repository.RepoRepo
	counter *int64
}

// newNpmSearchEnv 打开计数库并装配 npm 路由（匿名可读：仓库都建成 public）。
func newNpmSearchEnv(t *testing.T) *npmSearchEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, counter, err := persistence.OpenCountingForTest(filepath.Join(t.TempDir(), "npm-search.db"))
	if err != nil {
		t.Fatalf("打开数据库：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移：%v", err)
	}
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	assetSvc := domain.NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()),
		upstream.NewTestClient(5*time.Second))
	repoSvc := domain.NewRepositoryService(repos, repository.NewAclRepo(db), assets,
		domain.NewSettingService(repository.NewSettingRepo(db)), repository.NewUserRepo(db))
	engine := gin.New()
	RegisterNpmRoutes(engine, NewNpmHandler(NewRawHandler(assetSvc, repoSvc), nil, nil, ""))
	return &npmSearchEnv{engine: engine, assets: assetSvc, repos: repos, counter: counter}
}

// publishPackument 直接写入一份 packument（路径即包名，内容为最小可解析文档）。
func (e *npmSearchEnv) publishPackument(t *testing.T, repoName, pkg string) {
	t.Helper()
	doc := fmt.Sprintf(`{"name":%q,"dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":%q,"version":"1.0.0","description":"测试包"}},"time":{"modified":"2024-01-01T00:00:00.000Z"}}`, pkg, pkg)
	if _, err := e.assets.Put(repoName, pkg, strings.NewReader(doc), "application/json"); err != nil {
		t.Fatalf("写入 packument %s/%s：%v", repoName, pkg, err)
	}
}

// search 发起一次 `npm search` 并返回解析后的响应与本次请求的 SQL 语句数。
func (e *npmSearchEnv) search(t *testing.T, path string) (npmSearchResponse, int64) {
	t.Helper()
	atomic.StoreInt64(e.counter, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	e.engine.ServeHTTP(rec, req)
	queries := atomic.LoadInt64(e.counter)
	if rec.Code != http.StatusOK {
		t.Fatalf("search 状态码 = %d（体：%s）", rec.Code, rec.Body.String())
	}
	var out npmSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 search 响应：%v（体：%s）", err, rec.Body.String())
	}
	return out, queries
}

// npmSearchResponse 是 npm 标准 search 响应的最小视图（字段名必须与 npm 一致）。
type npmSearchResponse struct {
	Objects []struct {
		Package struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Description string `json:"description"`
		} `json:"package"`
	} `json:"objects"`
	Total int    `json:"total"`
	Time  string `json:"time"`
}

// TestNpmSearchGroupDedupesAndMerges 验证 group 多成员的结果口径不变：
// 同一包名在两个成员里都存在时只出现一次，且成员顺序决定先后。
func TestNpmSearchGroupDedupesAndMerges(t *testing.T) {
	e := newNpmSearchEnv(t)
	for _, name := range []string{"npm-m1", "npm-m2", "npm-m3"} {
		if _, err := e.repos.Create(name, "npm", "hosted", "public", ""); err != nil {
			t.Fatalf("建成员仓库 %s：%v", name, err)
		}
	}
	cfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{
		Members: []string{"npm-m1", "npm-m2", "npm-m3"}})
	if err != nil {
		t.Fatalf("编码 group 配置：%v", err)
	}
	if _, err := e.repos.Create("npm-grp", "npm", "group", "public", cfg); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}
	// 三个成员：m1 有 alpha/beta，m2 有 alpha（与 m1 重名）与 gamma，m3 有 delta。
	e.publishPackument(t, "npm-m1", "pkg-alpha")
	e.publishPackument(t, "npm-m1", "pkg-beta")
	e.publishPackument(t, "npm-m2", "pkg-alpha")
	e.publishPackument(t, "npm-m2", "pkg-gamma")
	e.publishPackument(t, "npm-m3", "pkg-delta")
	// tarball 路径（含 /-/）不得进入搜索结果。
	e.publishPackument(t, "npm-m1", "pkg-alpha/-/pkg-alpha-1.0.0.tgz")

	out, _ := e.search(t, "/npm/npm-grp/-/v1/search?text=pkg&size=20")
	if out.Total != 4 {
		t.Fatalf("total = %d，期望 4（pkg-alpha 跨成员去重）：%+v", out.Total, out.Objects)
	}
	names := make([]string, 0, len(out.Objects))
	for _, obj := range out.Objects {
		names = append(names, obj.Package.Name)
	}
	want := []string{"pkg-alpha", "pkg-beta", "pkg-gamma", "pkg-delta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("结果顺序/内容 = %v，期望 %v（成员顺序 + 成员内 path 升序）", names, want)
	}
	if out.Time == "" {
		t.Fatal("响应缺少 npm 标准字段 time")
	}
	if out.Objects[0].Package.Version != "1.0.0" || out.Objects[0].Package.Description != "测试包" {
		t.Fatalf("packument 字段提取异常：%+v", out.Objects[0].Package)
	}
}

// TestNpmSearchGroupQueryCountIndependentOfMembers 是本批修复的核心断言：
// 成员数从 1 增加到 4（新增成员都没有命中），请求的 SQL 语句数必须**完全相同**。
//
// 语料刻意让额外成员不产生命中：这样 packument 读取次数不随成员数变化，语句数若有增长就
// 只能来自「每个成员各跑一次搜索」——正是要消掉的放大。
func TestNpmSearchGroupQueryCountIndependentOfMembers(t *testing.T) {
	e := newNpmSearchEnv(t)
	// 只有 npm-hit 有命中；npm-empty-1..3 建了但不含匹配内容。
	for _, name := range []string{"npm-hit", "npm-empty-1", "npm-empty-2", "npm-empty-3"} {
		if _, err := e.repos.Create(name, "npm", "hosted", "public", ""); err != nil {
			t.Fatalf("建成员仓库 %s：%v", name, err)
		}
	}
	e.publishPackument(t, "npm-hit", "pkg-only-here")
	for _, name := range []string{"npm-empty-1", "npm-empty-2", "npm-empty-3"} {
		e.publishPackument(t, name, "other-package")
	}
	createGroup := func(name string, members []string) {
		cfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{Members: members})
		if err != nil {
			t.Fatalf("编码 group 配置：%v", err)
		}
		if _, err := e.repos.Create(name, "npm", "group", "public", cfg); err != nil {
			t.Fatalf("建 group 仓库 %s：%v", name, err)
		}
	}
	createGroup("npm-one", []string{"npm-hit"})
	createGroup("npm-four", []string{"npm-hit", "npm-empty-1", "npm-empty-2", "npm-empty-3"})

	oneResult, oneQueries := e.search(t, "/npm/npm-one/-/v1/search?text=pkg-only-here&size=20")
	fourResult, fourQueries := e.search(t, "/npm/npm-four/-/v1/search?text=pkg-only-here&size=20")

	if oneResult.Total != 1 || fourResult.Total != 1 {
		t.Fatalf("两侧都应只命中 1 条：单成员=%d 四成员=%d", oneResult.Total, fourResult.Total)
	}
	if oneQueries != fourQueries {
		t.Fatalf("成员数从 1 增到 4 时语句数应保持不变（都只发一次搜索），实际 %d → %d",
			oneQueries, fourQueries)
	}
}

// TestNpmSearchKeepsPerMemberPagination 钉住「每个成员各取 from+size 条」的既有口径：
// total 统计的是**取回并按 packument 校验过的条目数**，因此天然受 from+size 截断
// （这是既有行为，本批只做加速不改口径）；objects 再按 size/from 切窗口。
func TestNpmSearchKeepsPerMemberPagination(t *testing.T) {
	e := newNpmSearchEnv(t)
	if _, err := e.repos.Create("npm-page", "npm", "hosted", "public", ""); err != nil {
		t.Fatalf("建仓库：%v", err)
	}
	for _, pkg := range []string{"page-a", "page-b", "page-c"} {
		e.publishPackument(t, "npm-page", pkg)
	}
	out, _ := e.search(t, "/npm/npm-page/-/v1/search?text=page-&size=2")
	if out.Total != 2 {
		t.Fatalf("total = %d，期望 2（total 受 from+size 截断，既有口径）", out.Total)
	}
	if len(out.Objects) != 2 {
		t.Fatalf("objects 条数 = %d，期望 2（size 生效）", len(out.Objects))
	}
	// from=1,size=2 → 每成员取 3 条，跳过第一条后返回两条。
	out, _ = e.search(t, "/npm/npm-page/-/v1/search?text=page-&size=2&from=1")
	if out.Total != 3 || len(out.Objects) != 2 {
		t.Fatalf("from=1 时 total/objects = %d/%d，期望 3/2", out.Total, len(out.Objects))
	}
	names := []string{out.Objects[0].Package.Name, out.Objects[1].Package.Name}
	if strings.Join(names, ",") != "page-b,page-c" {
		t.Fatalf("from=1 的结果 = %v，期望 [page-b page-c]", names)
	}
}
