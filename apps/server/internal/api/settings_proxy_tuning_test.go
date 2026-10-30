package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/auth"
	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/persistence"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件覆盖两项 proxy/group 调优的**设置面**：边界夹取与「保存后不需重启即生效」。
//
// 为什么必须做到端到端：这两项此前只有「环境变量 → 启动期 setter」一条链路，
// 设置页的字段在代码里根本不存在（只写在规格文档里）。只断言「setting 表里存了值」
// 无法区分「接线成功」与「存了个没人读的数字」——负缓存 TTL 就曾经是一个定义了 setter
// 但全仓没人调用的死配置。因此这里从 Resolve 的外部可观测行为（上游命中次数、耗时上界）
// 反向证明回调真的改了运行时状态。
//
// 回调的接线方式与 wiring.handlers() 一致：设置 API → 回调 → AssetService 的 setter。

// newTuningTestEnv 组装一套带真实 AssetService 与设置服务的测试环境。
// 返回 handlers、仓库 Repo、领域服务，以及 HTTP 录制用的 gin 引擎（带 admin 主体）。
func newTuningTestEnv(t *testing.T) (*Handlers, *repository.RepoRepo, *domain.AssetService, *gin.Engine, *persistence.DB) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	db := openAPITestDB(t)
	repoRepo := repository.NewRepoRepo(db)
	settingSvc := domain.NewSettingService(repository.NewSettingRepo(db))
	assets := domain.NewAssetService(repoRepo, repository.NewAssetRepo(db),
		blobstore.NewStore(t.TempDir()), upstream.NewTestClient(30*time.Second))

	h := NewHandlers(Deps{
		Settings: settingSvc,
		Assets:   assets,
		// 与装配层同构的热生效回调：保存设置后立刻推给持有运行时状态的 AssetService。
		OnGroupMemberTimeoutChange: assets.SetGroupMemberTimeout,
		OnNegativeCacheTTLChange:   assets.SetNegativeCacheTTL,
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth.principal", &auth.Principal{Role: "admin", Username: "admin", UserID: 1})
		c.Next()
	})
	router.GET("/api/v1/settings", h.GetSettings)
	router.PUT("/api/v1/settings", h.PutSettings)
	return h, repoRepo, assets, router, db
}

// putSettingsJSON 发一次 PUT /api/v1/settings 并解析响应；返回响应与状态码。
func putSettingsJSON(t *testing.T, router *gin.Engine, body string) (SettingsResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	var out SettingsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析响应：%v", err)
		}
	}
	return out, rec.Code
}

// TestClampProxyTuningSecs 覆盖两项的边界口径（0 / 负数 / 超上限）。
//
// 口径刻意与 config 层同名解析函数一致：这两项在环境变量侧就是「非法值回退默认、
// 超上限夹取并留警告」，设置页作为同一配置的第二个入口给出不同结论（例如报 400）
// 会让「哪个入口才是真源」变成一次无谓的争论。
func TestClampProxyTuningSecs(t *testing.T) {
	t.Parallel()
	groupCases := []struct {
		name string
		in   int
		want int
	}{
		{"0 回退默认（本项无关闭语义）", 0, 3},
		{"负数回退默认", -5, 3},
		{"下界 1 原样保留", 1, 1},
		{"上界 3600 原样保留", 3600, 3600},
		{"超上限夹到 3600", 99999, 3600},
	}
	for _, tc := range groupCases {
		if got := clampGroupMemberTimeoutSecs(tc.in); got != tc.want {
			t.Errorf("group_member_timeout %s：入参 %d 得 %d，期望 %d", tc.name, tc.in, got, tc.want)
		}
	}

	negCases := []struct {
		name string
		in   int
		want int
	}{
		{"0 是合法的关闭值，必须原样保留", 0, 0},
		{"负数回退默认（负数不等于关闭）", -600, 60},
		{"1 原样保留", 1, 1},
		{"上界 3600 原样保留", 3600, 3600},
		{"超上限夹到 3600", 60000, 3600},
	}
	for _, tc := range negCases {
		if got := clampNegativeCacheTTLSecs(tc.in); got != tc.want {
			t.Errorf("negative_cache_ttl %s：入参 %d 得 %d，期望 %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestSettingsSnapshotProxyTuningDefaults 钉住「键缺失时界面显示什么」：
// 回退到与 config 同值的默认（3s / 60s），而不是 0。
//
// 0 在这里是危险默认：负缓存 TTL 显示 0 会被读成「已关闭」，而实际仍在缓存。
func TestSettingsSnapshotProxyTuningDefaults(t *testing.T) {
	t.Parallel()
	_, _, _, router, _ := newTuningTestEnv(t)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var got SettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if got.GroupMemberTimeout != 3 || got.NegativeCacheTTL != 60 {
		t.Fatalf("键缺失时应回填默认 3/60，实际 %d/%d", got.GroupMemberTimeout, got.NegativeCacheTTL)
	}

	// 保存 0 后必须**照实回显 0**，否则界面无法把开关拨回关闭态。
	after, code := putSettingsJSON(t, router, `{"negativeCacheTTL":0}`)
	if code != http.StatusOK {
		t.Fatalf("PUT 0 应 200，得 %d", code)
	}
	if after.NegativeCacheTTL != 0 {
		t.Fatalf("保存 0 后回显应为 0（关闭），实际 %d", after.NegativeCacheTTL)
	}
}

// countingUpstream 起一个按固定状态码应答的上游，并统计命中次数。
func countingUpstream(t *testing.T, status int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// hangingUpstream 构造「永不给响应头」的上游：请求进来后一直挂到 ctx 结束。
// 这正是成员确认预算要挡的场景（跨地域慢成员 / 黑洞路由）。
func hangingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPutSettingsNegativeCacheHotApplies 验证「保存 negativeCacheTTL=0 → 不重启即关闭负缓存」。
//
// 判据是上游命中**次数**：关闭态下同一条缺失路径读两次必须回源两次。
// 若回调没接上（TTL 仍是默认 60s），第二次会命中负缓存、计数停在 1。
func TestPutSettingsNegativeCacheHotApplies(t *testing.T) {
	// 不并行：本用例断言运行时状态被就地改写，与其它用例共享进程级无状态假设更清晰。
	_, repoRepo, assets, router, _ := newTuningTestEnv(t)
	srv, hits := countingUpstream(t, http.StatusNotFound)
	if _, err := repoRepo.Create("hot-nc", "raw", "proxy", "private", proxyConfigJSONForTest(t, srv.URL)); err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	// 前置：默认 TTL 下负缓存确实生效（第一次回源、第二次命中缓存）。
	if _, _, err := assets.Resolve(t.Context(), "hot-nc", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("首次读应 ErrNotFound，实际：%v", err)
	}
	if _, _, err := assets.Resolve(t.Context(), "hot-nc", "missing.jar"); err != domain.ErrNotFound {
		t.Fatalf("第二次读应 ErrNotFound，实际：%v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("默认 TTL 下第二次应命中负缓存，上游命中计数 = %d，期望 1", got)
	}

	// 经设置 API 关闭负缓存：此后**不重启**，同路径每次都要真实回源。
	if resp, code := putSettingsJSON(t, router, `{"negativeCacheTTL":0}`); code != http.StatusOK || resp.NegativeCacheTTL != 0 {
		t.Fatalf("关闭负缓存失败：code=%d resp=%+v", code, resp)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := assets.Resolve(t.Context(), "hot-nc", "missing2.jar"); err != domain.ErrNotFound {
			t.Fatalf("关闭态下读应 ErrNotFound，实际：%v", err)
		}
	}
	// 第一次回源 1 次 + 关闭后两次都回源 = 3；若回调没生效会是 2。
	if got := atomic.LoadInt32(hits); got != 3 {
		t.Fatalf("关闭负缓存后上游命中计数 = %d，期望 3（每次读都回源）：回调可能没生效", got)
	}
	// 关闭态下负缓存表不得因这些 404 而增长（先清理上面默认 TTL 时期写入的那条）。
	assets.PurgeNegativeCache(domain.NegativeCacheFilter{})
	if got := assets.NegativeCacheEntriesCount(); got != 0 {
		t.Fatalf("关闭态下负缓存条目数应为 0，实际 %d", got)
	}
}

// TestPutSettingsGroupMemberTimeoutHotApplies 验证「保存 groupMemberTimeout=1 → 不重启即缩短确认预算」。
//
// 判据用耗时：一个永久挂起的成员 + 一个快速 404 的成员，默认预算 3s 时整体约 3s；
// 保存 1s 后整体应落到 1s 量级。若回调没生效，耗时仍在 3s 量级。
func TestPutSettingsGroupMemberTimeoutHotApplies(t *testing.T) {
	_, repoRepo, assets, router, _ := newTuningTestEnv(t)
	hangSrv := hangingUpstream(t)
	fastSrv, _ := countingUpstream(t, http.StatusNotFound)
	if _, err := repoRepo.Create("hot-g-hang", "raw", "proxy", "private", proxyConfigJSONForTest(t, hangSrv.URL)); err != nil {
		t.Fatalf("建挂起成员：%v", err)
	}
	if _, err := repoRepo.Create("hot-g-fast", "raw", "proxy", "private", proxyConfigJSONForTest(t, fastSrv.URL)); err != nil {
		t.Fatalf("建快速成员：%v", err)
	}
	groupCfg, err := repository.EncodeRepositoryConfig(repository.RepositoryConfig{
		Members: []string{"hot-g-hang", "hot-g-fast"},
	})
	if err != nil {
		t.Fatalf("编码 group 配置：%v", err)
	}
	if _, err := repoRepo.Create("hot-group", "raw", "group", "private", groupCfg); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}

	if resp, code := putSettingsJSON(t, router, `{"groupMemberTimeout":1}`); code != http.StatusOK || resp.GroupMemberTimeout != 1 {
		t.Fatalf("保存成员确认预算失败：code=%d resp=%+v", code, resp)
	}

	start := time.Now()
	_, _, err = assets.Resolve(t.Context(), "hot-group", "missing.jar")
	elapsed := time.Since(start)
	if err != domain.ErrUpstreamTimeout {
		t.Fatalf("挂起成员应判为超时（ErrUpstreamTimeout），实际：%v（耗时 %s）", err, elapsed)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("保存 1s 预算后整体耗时 = %s，仍接近默认 3s：热生效回调可能没生效", elapsed)
	}
	if elapsed < 800*time.Millisecond {
		t.Fatalf("保存 1s 预算后整体耗时 = %s，短于预算：看门狗未按新预算等待", elapsed)
	}
}

// TestPutSettingsProxyTuningClampOverHTTP 端到端确认越界输入在**存进表之前**就被夹取：
// 响应回显与后续读取都应是夹取后的值，不存在「表里 99999、运行时 3600」的分叉。
func TestPutSettingsProxyTuningClampOverHTTP(t *testing.T) {
	t.Parallel()
	_, _, _, router, _ := newTuningTestEnv(t)

	cases := []struct {
		name    string
		body    string
		want    int
		isGroup bool
	}{
		{"成员预算 0 回退默认 3s", `{"groupMemberTimeout":0}`, 3, true},
		{"成员预算负数回退默认 3s", `{"groupMemberTimeout":-5}`, 3, true},
		{"成员预算超上限夹到 3600", `{"groupMemberTimeout":99999}`, 3600, true},
		{"负缓存 TTL 0 原样保留（关闭）", `{"negativeCacheTTL":0}`, 0, false},
		{"负缓存 TTL 负数回退默认 60s", `{"negativeCacheTTL":-600}`, 60, false},
		{"负缓存 TTL 超上限夹到 3600", `{"negativeCacheTTL":60000}`, 3600, false},
	}
	for _, tc := range cases {
		resp, code := putSettingsJSON(t, router, tc.body)
		if code != http.StatusOK {
			t.Fatalf("%s：应 200，得 %d", tc.name, code)
		}
		got := resp.NegativeCacheTTL
		if tc.isGroup {
			got = resp.GroupMemberTimeout
		}
		if got != tc.want {
			t.Errorf("%s：回显 %d，期望 %d", tc.name, got, tc.want)
		}
	}
}
