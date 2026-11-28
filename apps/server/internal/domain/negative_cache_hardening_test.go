package domain_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// TestSnapshotJarAlsoProtectedBySiblingPom 是 M-4 回归测试。
//
// Maven 协议层会把 .../1.0-SNAPSHOT/foo-1.0-SNAPSHOT.jar 改写成带时间戳的
// foo-1.0-20260317.215705-3.jar 再解析。若守护按 "artifactId-version" 拼前缀，
// 时间戳文件名永远匹配不上，整个「有 pom 无 jar」防护对 SNAPSHOT 失效——
// 而「pom 先发布、jar 还在传」正是 SNAPSHOT 最典型的瞬时 404 场景。
func TestSnapshotJarAlsoProtectedBySiblingPom(t *testing.T) {
	t.Parallel()
	var jarReady atomic.Bool
	var jarHits atomic.Int32

	const pom = `<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>snap</artifactId><version>1.0-SNAPSHOT</version></project>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/com/example/snap/1.0-SNAPSHOT/snap-1.0-20260317.215705-3.pom":
			_, _ = w.Write([]byte(pom))
		case "/com/example/snap/1.0-SNAPSHOT/snap-1.0-20260317.215705-3.jar":
			jarHits.Add(1)
			if !jarReady.Load() {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("SNAPSHOT-JAR"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)
	if _, err := repos.Create("snap-proxy", "maven", "proxy", "public", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("创建 proxy 仓库：%v", err)
	}

	// 1) 先拉 POM 落缓存。
	_, rc, err := svc.Resolve(context.Background(), "snap-proxy", "com/example/snap/1.0-SNAPSHOT/snap-1.0-20260317.215705-3.pom")
	if err != nil {
		t.Fatalf("拉取 SNAPSHOT POM 失败：%v", err)
	}
	_ = rc.Close()

	// 2) 上游瞬时 404：不得写入负缓存。
	if _, _, err := svc.Resolve(context.Background(), "snap-proxy", "com/example/snap/1.0-SNAPSHOT/snap-1.0-20260317.215705-3.jar"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("首次 SNAPSHOT JAR 应返回 ErrNotFound，得 %v", err)
	}

	// 3) 上游恢复后必须立即成功，不能等 TTL 过期。
	jarReady.Store(true)
	_, rc, err = svc.Resolve(context.Background(), "snap-proxy", "com/example/snap/1.0-SNAPSHOT/snap-1.0-20260317.215705-3.jar")
	if err != nil {
		t.Fatalf("上游恢复后 SNAPSHOT JAR 应立即成功（SNAPSHOT 必须受守护）：%v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "SNAPSHOT-JAR" || jarHits.Load() != 2 {
		t.Fatalf("内容=%q hits=%d，期望 SNAPSHOT-JAR / 2", string(body), jarHits.Load())
	}
}

// TestPomOnSiblingMemberStillProtectsProxy 是 M-5 回归测试。
//
// 客户端通常经 group 拉 POM，POM 会落在最先服务它的那个成员上。
// 若守护只看本 proxy 的本地缓存，当 POM 在兄弟成员时本条 proxy 会判定「无证据」并照写负缓存，
// 之后 group 读在该成员上被负缓存直接 404、根本不回源——守护形同虚设。
func TestPomOnSiblingMemberStillProtectsProxy(t *testing.T) {
	t.Parallel()
	var jarReady atomic.Bool
	var jarHits atomic.Int32

	const pom = `<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>sib</artifactId><version>2.0.0</version></project>`

	// pomSrv 只提供 POM；jarSrv 只提供（先 404 后成功的）JAR。
	pomSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/com/example/sib/2.0.0/sib-2.0.0.pom" {
			_, _ = w.Write([]byte(pom))
			return
		}
		http.NotFound(w, r)
	}))
	defer pomSrv.Close()

	jarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/com/example/sib/2.0.0/sib-2.0.0.jar" {
			jarHits.Add(1)
			if !jarReady.Load() {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("SIBLING-JAR"))
			return
		}
		http.NotFound(w, r)
	}))
	defer jarSrv.Close()

	svc, repos := newAssetService(t)
	svc.SetNegativeCacheTTL(time.Hour)
	if _, err := repos.Create("pom-member", "maven", "proxy", "public", proxyConfigJSON(t, pomSrv.URL)); err != nil {
		t.Fatalf("创建 POM 成员：%v", err)
	}
	if _, err := repos.Create("jar-member", "maven", "proxy", "public", proxyConfigJSON(t, jarSrv.URL)); err != nil {
		t.Fatalf("创建 JAR 成员：%v", err)
	}
	// POM 成员排在前面：POM 会被缓存在它上面。
	if _, err := repos.Create("agg", "maven", "group", "public", groupConfigJSON(t, "pom-member", "jar-member")); err != nil {
		t.Fatalf("创建 group：%v", err)
	}

	// 1) 经 group 拉 POM，落缓存到 pom-member。
	_, rc, err := svc.Resolve(context.Background(), "agg", "com/example/sib/2.0.0/sib-2.0.0.pom")
	if err != nil {
		t.Fatalf("拉取 POM 失败：%v", err)
	}
	_ = rc.Close()

	// 2) JAR 在两个成员都 404。jar-member 自身无 POM，但兄弟成员有，必须命中守护。
	if _, _, err := svc.Resolve(context.Background(), "agg", "com/example/sib/2.0.0/sib-2.0.0.jar"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("首次 JAR 应返回 ErrNotFound，得 %v", err)
	}

	// 3) 上游恢复后经 group 必须能立即拿到，而不是被 jar-member 的负缓存钉死。
	jarReady.Store(true)
	_, rc, err = svc.Resolve(context.Background(), "agg", "com/example/sib/2.0.0/sib-2.0.0.jar")
	if err != nil {
		t.Fatalf("上游恢复后应立即可读（POM 在兄弟成员时守护仍须生效）：%v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "SIBLING-JAR" {
		t.Fatalf("内容 = %q，期望 SIBLING-JAR", string(body))
	}
}

// TestGlobalNegativeCacheDisableCannotBeReopened 是 M-6 回归测试：
// 全局 TTL=0 是运维的绝对关闭开关，仓库级正数覆盖不得把它重新打开；
// 否则「已经在设置页关掉了，却还在返回缓存的 404」。
func TestGlobalNegativeCacheDisableCannotBeReopened(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	// 仓库级显式正数 TTL（模拟管理员在仓库详情里配了 3600）。
	customTTL := 3600
	cfg := repository.RepositoryConfig{RemoteURL: srv.URL, NegativeCacheTTLSeconds: &customTTL}
	encoded, err := repository.EncodeRepositoryConfig(cfg)
	if err != nil {
		t.Fatalf("编码配置失败：%v", err)
	}
	if _, err := repos.Create("open-proxy", "raw", "proxy", "public", encoded); err != nil {
		t.Fatalf("创建 proxy：%v", err)
	}
	// 全局关闭。
	svc.SetNegativeCacheTTL(0)

	for i := 0; i < 3; i++ {
		if _, _, err := svc.Resolve(context.Background(), "open-proxy", "x/y.bin"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("第 %d 次应返回 ErrNotFound，得 %v", i+1, err)
		}
	}
	// 全局关闭时每次都应回源，不能被仓库级覆盖钉住。
	if hits.Load() != 3 {
		t.Fatalf("全局负缓存关闭后每次都应回源，实际命中 %d 次", hits.Load())
	}
	if n := svc.NegativeCacheEntriesCount(); n != 0 {
		t.Fatalf("全局关闭时不应产生任何负缓存条目，实际 %d", n)
	}
}

// TestGroupCycleIsRejectedOnWrite 是 B-3 回归测试：
// 允许 group 互指会让单个请求在 16 层深度内按分支因子指数放大
// （2-环约 2^16 个 goroutine）。写入侧必须拒绝，而不是靠深度上限兜底。
func TestGroupCycleIsRejectedOnWrite(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repoSvc := domain.NewRepositoryService(
		repository.NewRepoRepo(db),
		repository.NewAclRepo(db),
		repository.NewAssetRepo(db),
		domain.NewSettingService(repository.NewSettingRepo(db)),
		repository.NewUserRepo(db),
	)
	repos := repository.NewRepoRepo(db)

	if _, err := repos.Create("p-base", "raw", "proxy", "public", proxyConfigJSONFromRaw(t)); err != nil {
		t.Fatalf("创建基础 proxy：%v", err)
	}
	if _, err := repos.Create("g1", "raw", "group", "public", groupConfigJSON(t, "p-base")); err != nil {
		t.Fatalf("创建 g1：%v", err)
	}
	if _, err := repos.Create("g2", "raw", "group", "public", groupConfigJSON(t, "g1")); err != nil {
		t.Fatalf("创建 g2：%v", err)
	}

	// g2 -> g1 已存在；把 g1 的成员改成包含 g2 会形成环，必须被拒绝。
	cyclic := repository.RepositoryConfig{Members: []string{"g2"}}
	if _, err := repoSvc.Update("g1", "", nil, &cyclic, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("成环配置应返回 ErrValidation，得 %v", err)
	}
	// 自引用同样必须被拒绝。
	selfRef := repository.RepositoryConfig{Members: []string{"g1"}}
	if _, err := repoSvc.Update("g1", "", nil, &selfRef, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("自引用应返回 ErrValidation，得 %v", err)
	}
	// 合法的一层嵌套仍须可写入（证明校验没有过度收紧）。
	if _, err := repos.Create("g3", "raw", "group", "public", groupConfigJSON(t, "g2")); err != nil {
		t.Fatalf("合法嵌套 g3 -> g2 应可创建：%v", err)
	}
}

// TestCycleInStoredConfigDoesNotExplode 验证历史脏数据成环时读路径的 visited 兜底：
// 即便库里已有环（升级前写入），单次解析也必须在有限步内收敛，不能指数展开。
func TestCycleInStoredConfigDoesNotExplode(t *testing.T) {
	t.Parallel()
	svc, repos := newAssetService(t)
	if _, err := repos.Create("cyc-proxy", "raw", "proxy", "public", proxyConfigJSONFromRaw(t)); err != nil {
		t.Fatalf("创建 proxy：%v", err)
	}
	// 直接写库绕过校验，模拟已存在的环：cyc-a -> cyc-b -> cyc-a。
	if _, err := repos.Create("cyc-a", "raw", "group", "public", groupConfigJSON(t, "cyc-proxy", "cyc-b")); err != nil {
		t.Fatalf("创建 cyc-a：%v", err)
	}
	if _, err := repos.Create("cyc-b", "raw", "group", "public", groupConfigJSON(t, "cyc-a")); err != nil {
		t.Fatalf("创建 cyc-b：%v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = svc.Resolve(context.Background(), "cyc-a", "some/path.bin")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("成环配置下解析未在 10 秒内收敛，visited 防环未生效")
	}
}

// TestAliasMemberKeepsCascadeInvalidation 是 M-1 回归测试：
// group 成员允许写别名，级联失效必须能沿别名找到父边，
// 否则「成员上传后 group 仍返回 404」，症状与毒缓存完全一样。
func TestAliasMemberKeepsCascadeInvalidation(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)

	if _, err := repos.Create("hosted-a", "raw", "hosted", "public", "{}"); err != nil {
		t.Fatalf("创建 hosted：%v", err)
	}
	proxyID, err := repos.Create("alias-proxy", "raw", "proxy", "public", proxyConfigJSONFromRaw(t))
	if err != nil {
		t.Fatalf("创建 proxy：%v", err)
	}
	// group 用别名引用该 proxy。
	if _, err := repos.Create("alias-group", "raw", "group", "public", groupConfigJSON(t, "proxy-alias")); err != nil {
		t.Fatalf("创建 group：%v", err)
	}
	if err := repos.SetAliases(proxyID, []string{"proxy-alias"}); err != nil {
		t.Fatalf("设置别名：%v", err)
	}

	// 用同一个库构造 AssetService，保证看到的是同一份数据。
	assets := repository.NewAssetRepo(db)
	svc := domain.NewAssetService(repos, assets, blobstore.NewStore(t.TempDir()), upstream.NewTestClient(5*time.Second))

	// 断言级联失效能沿别名找到父 group。
	parents := svc.ParentGroupIDs(proxyID)
	found := false
	for _, id := range parents {
		name, nameErr := repositoriesNameByID(repos, id)
		if nameErr == nil && name == "alias-group" {
			found = true
		}
	}
	if !found {
		t.Fatalf("用别名描述的父 group 未被识别，ParentGroupIDs = %v（别名成员会丢失级联失效）", parents)
	}
}

// repositoriesNameByID 是测试内的小工具：按 ID 取仓库名。
func repositoriesNameByID(repos *repository.RepoRepo, id int64) (string, error) {
	repo, err := repos.GetByID(id)
	if err != nil || repo == nil {
		return "", err
	}
	return repo.Name, nil
}

// proxyConfigJSONFromRaw 构造一个必然 404 的 proxy 配置（只关心图结构、不关心回源的用例）。
func proxyConfigJSONFromRaw(t *testing.T) string {
	t.Helper()
	return proxyConfigJSON(t, "http://127.0.0.1:1")
}
