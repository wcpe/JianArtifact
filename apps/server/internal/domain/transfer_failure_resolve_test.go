package domain_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件覆盖「回源失败原因五分类」的**端到端**验收（spec §5.1）：
// 用真实上游夹具制造「慢」「没响应」「中途断」「本地写失败」四类现场，
// 断言类别、指标事件、阻断结论、负缓存与日志。
//
// 组织约定（与 resolve_observability_test.go 一致）：
//   - 抓日志的用例必须串行（log.SetOutput 是进程级写入），因此本文件全部用例不加 t.Parallel()；
//   - 日志断言一律先按唯一路径 marker 过滤，避免与并行用例的输出互相污染。

const (
	// fr158Declared 是「15MB 制品」的声明长度，与缺陷现场一致。
	fr158Declared = int64(15 << 20)
	// fr158Timeout 是回源整体超时：远小于「低速率滴流传完 15MB」所需时间，
	// 但足够让响应头与若干块正文到达，从而保证超时发生时已有传输进展。
	fr158Timeout = 600 * time.Millisecond
)

// newAssetServiceWithStore 装配一个可指定 blob 存储的 AssetService（本地写失败用例需要）。
func newAssetServiceWithStore(t *testing.T, blobs *blobstore.Store) (*domain.AssetService, *repository.RepoRepo) {
	t.Helper()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	return domain.NewAssetService(repos, assets, blobs, upstream.NewTestClient(fr158Timeout)), repos
}

// newFR158Service 装配一个回源超时为 fr158Timeout 的 AssetService 与事件记录桩。
func newFR158Service(t *testing.T) (*domain.AssetService, *repository.RepoRepo, *blockEventSpy) {
	t.Helper()
	svc, repos := newAssetServiceWithUpstreamTimeout(t, fr158Timeout)
	spy := &blockEventSpy{}
	svc.SetUpstreamBlockRecorder(spy)
	return svc, repos, spy
}

// trickleBodyServer 构造「响应头秒回、正文滴流」的上游夹具（slow_rate 的现场）：
// 立即回 200 + 声明 declared 字节，随后每 interval 写 chunk 字节。
//
// 客户端超时（或断开）后立即返回，保证 srv.Close 不被挂起请求拖住。
func trickleBodyServer(t *testing.T, declared int64, chunk int, interval time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", strconv.FormatInt(declared, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		flushNow(w)
		buf := make([]byte, chunk)
		for {
			if _, err := w.Write(buf); err != nil {
				return
			}
			flushNow(w)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// headerOnlyServer 构造「回了响应头却一个字节正文都不给」的上游夹具：
// 拿到 200 之后永久挂起，直到客户端放弃。
func headerOnlyServer(t *testing.T, declared int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(declared, 10))
		w.WriteHeader(http.StatusOK)
		flushNow(w)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// silentServer 构造「接受连接但永不返回响应头」的上游夹具（no_response 的前半段）。
func silentServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// truncatedBodyServer 构造「声明 15MB、只传 sent 字节就掐断连接」的上游夹具
// （transfer_aborted 现场）。用 http.ErrAbortHandler 中止响应，net/http 会直接关闭
// 连接而不补齐声明长度，客户端因此看到 io.ErrUnexpectedEOF。
func truncatedBodyServer(t *testing.T, declared, sent int64) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", strconv.FormatInt(declared, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, sent))
		flushNow(w)
		// 立刻掐断：不再补齐声明长度，直接中止响应并关闭连接。
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// singlePathLog 抓取本次请求产生的日志里含 path 的全部行（marker 过滤，避免并行污染）。
func singlePathLog(t *testing.T, path string, fn func()) string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	previous := log.Writer()
	log.SetOutput(sinkFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	defer log.SetOutput(previous)
	fn()
	mu.Lock()
	defer mu.Unlock()
	var lines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "path="+path) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// sinkFunc 适配 log.SetOutput 需要的 io.Writer。
type sinkFunc func([]byte) (int, error)

func (f sinkFunc) Write(p []byte) (int, error) { return f(p) }

// logField 从结构化日志行里取 key=value 的值；取不到返回空串。
func logField(line, key string) string {
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, key+"="); ok {
			return value
		}
	}
	return ""
}

// assertNotBlocked 断言仓库未进入阻断态（传输侧失败不得触发 auto-block）。
func assertNotBlocked(t *testing.T, svc *domain.AssetService, repoID int64) {
	t.Helper()
	if got := svc.Status(repoID); got.Status == domain.StatusAutoBlocked || got.Status == domain.StatusHalfOpen {
		t.Fatalf("传输侧失败不得让仓库进入阻断，实际状态 %s", got.Status)
	}
}

// TestResolveProxySlowRateCategory 覆盖 spec §5.1 第二条：上游立即 200 + 声明 15MB，
// 随后低速率滴流直到回源超时 → 类别 slow_rate、日志能读到 transferred > 0、
// 不阻断、下次仍重试（不写负缓存）。
func TestResolveProxySlowRateCategory(t *testing.T) {
	const path = "com/example/fr158/slow-15mb.jar"
	srv, hits := trickleBodyServer(t, fr158Declared, 1024, 20*time.Millisecond)
	svc, repos, spy := newFR158Service(t)
	repoID, err := repos.Create("slow-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	var resolveErr error
	logs := singlePathLog(t, path, func() {
		_, _, resolveErr = svc.Resolve(context.Background(), "slow-proxy", path)
	})

	// 返回码：仍是 504 语义，且不得同时满足 ErrUpstream（否则协议层会回 502）。
	if !errors.Is(resolveErr, domain.ErrUpstreamTimeout) {
		t.Fatalf("慢回源应返回 ErrUpstreamTimeout（对外 504），实际：%v", resolveErr)
	}
	if errors.Is(resolveErr, domain.ErrUpstream) {
		t.Fatalf("慢回源不应同时满足 ErrUpstream（会被映射成 502），实际：%v", resolveErr)
	}
	// 指标：只记 slow_transfer 一次，不产生阻断事件。
	if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != "slow_transfer" {
		t.Fatalf("应只记录一次 slow_transfer 判定事件，实际 %v", reasons)
	}
	assertNotBlocked(t, svc, repoID)

	// 日志：类别可读，且已传字节数 > 0（证明「有进展」这一判据真的取到了值）。
	if !strings.Contains(logs, "category=slow_rate") {
		t.Fatalf("日志应包含 category=slow_rate：%s", logs)
	}
	if !strings.Contains(logs, "declared_bytes=15728640") {
		t.Fatalf("日志应包含声明的 15MB 长度：%s", logs)
	}
	transferred, convErr := strconv.ParseInt(logField(logs, "transferred_bytes"), 10, 64)
	if convErr != nil {
		t.Fatalf("日志缺少可解析的 transferred_bytes：%s", logs)
	}
	if transferred <= 0 {
		t.Fatalf("slow_rate 必须记录到传输进展（transferred > 0），实际 %d：%s", transferred, logs)
	}

	// 负缓存：失败未确认不存在，第二次请求必须仍然回源（而不是命中 404 负缓存）。
	if _, _, err := svc.Resolve(context.Background(), "slow-proxy", path); !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("第二次请求应仍为 ErrUpstreamTimeout（未写负缓存），实际：%v", err)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("两次请求都应回源（共 2 次），实际 %d 次", n)
	}
}

// TestResolveProxyNoResponseCategories 覆盖 spec §5.1 第一条，并区分它的两个形态：
//   - 连上了但永不返回响应头（拿头之前超时）→ no_response，不阻断；
//   - 回了响应头却一个字节都没传出来（拿头之后零进展超时）→ 同样归 no_response。
func TestResolveProxyNoResponseCategories(t *testing.T) {
	t.Run("拿头之前超时", func(t *testing.T) {
		const path = "com/example/fr158/silent.jar"
		srv := silentServer(t)
		svc, repos, spy := newFR158Service(t)
		repoID, err := repos.Create("silent-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
		if err != nil {
			t.Fatalf("建 proxy 仓库：%v", err)
		}

		_, _, resolveErr := svc.Resolve(context.Background(), "silent-proxy", path)
		if !errors.Is(resolveErr, domain.ErrUpstreamTimeout) {
			t.Fatalf("上游不响应应为 ErrUpstreamTimeout（对外 504），实际：%v", resolveErr)
		}
		// 拿头之前的超时沿用既有 upstream_timeout 事件语义（既有语义不变）。
		if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != "upstream_timeout" {
			t.Fatalf("应只记录一次 upstream_timeout，实际 %v", reasons)
		}
		assertNotBlocked(t, svc, repoID)
	})

	t.Run("拿头之后零字节", func(t *testing.T) {
		const path = "com/example/fr158/header-only.jar"
		srv := headerOnlyServer(t, fr158Declared)
		svc, repos, spy := newFR158Service(t)
		repoID, err := repos.Create("header-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
		if err != nil {
			t.Fatalf("建 proxy 仓库：%v", err)
		}

		var resolveErr error
		logs := singlePathLog(t, path, func() {
			_, _, resolveErr = svc.Resolve(context.Background(), "header-proxy", path)
		})
		if !errors.Is(resolveErr, domain.ErrUpstreamTimeout) {
			t.Fatalf("零字节传输超时应为 ErrUpstreamTimeout，实际：%v", resolveErr)
		}
		if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != "upstream_timeout" {
			t.Fatalf("零字节传输超时应只记录 upstream_timeout，实际 %v", reasons)
		}
		assertNotBlocked(t, svc, repoID)
		// 类别必须是 no_response，不能因为「已拿到响应头」就错判成 slow_rate。
		if !strings.Contains(logs, "category=no_response") {
			t.Fatalf("零字节传输超时应归 no_response：%s", logs)
		}
		if got := logField(logs, "transferred_bytes"); got != "0" {
			t.Fatalf("零字节传输的 transferred_bytes 应为 0，实际 %q：%s", got, logs)
		}
	})
}

// TestResolveProxyTransferAbortedCategory 覆盖 spec §5.1 第三条：200 + 声明 15MB，
// 传约 1MB 后强制断开 → transfer_aborted，且与 slow_rate 在日志与事件上均可区分。
func TestResolveProxyTransferAbortedCategory(t *testing.T) {
	const path = "com/example/fr158/aborted-15mb.jar"
	const sent = int64(1 << 20)
	srv, _ := truncatedBodyServer(t, fr158Declared, sent)
	svc, repos, spy := newFR158Service(t)
	repoID, err := repos.Create("abort-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	var resolveErr error
	logs := singlePathLog(t, path, func() {
		_, _, resolveErr = svc.Resolve(context.Background(), "abort-proxy", path)
	})
	if resolveErr == nil {
		t.Fatal("上游中途断连时不得返回成功")
	}
	if errors.Is(resolveErr, domain.ErrNotFound) {
		t.Fatalf("中途断连绝不能折叠成 404，实际：%v", resolveErr)
	}
	if !errors.Is(resolveErr, domain.ErrUpstreamTimeout) {
		t.Fatalf("中途断连应落在 504 分支（传输阶段失败），实际：%v", resolveErr)
	}
	if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != "transfer_aborted" {
		t.Fatalf("应只记录一次 transfer_aborted 判定事件，实际 %v", reasons)
	}
	assertNotBlocked(t, svc, repoID)

	if !strings.Contains(logs, "category=transfer_aborted") {
		t.Fatalf("日志应包含 category=transfer_aborted：%s", logs)
	}
	// 与 slow_rate 可区分：不得出现慢速类别的任何痕迹。
	if strings.Contains(logs, "category=slow_rate") || strings.Contains(logs, "带宽") {
		t.Fatalf("中途断连不得被描述成带宽不足：%s", logs)
	}
	transferred, convErr := strconv.ParseInt(logField(logs, "transferred_bytes"), 10, 64)
	if convErr != nil {
		t.Fatalf("日志缺少可解析的 transferred_bytes：%s", logs)
	}
	// 已传字节必须真实反映传输进展（应接近上游实际发出的 1MB），
	// 而不是 blobstore 在 io.Copy 失败时丢弃的 0。
	if transferred < sent/2 || transferred > sent {
		t.Fatalf("已传字节应在 [%d, %d] 内，实际 %d：%s", sent/2, sent, transferred, logs)
	}
}

// TestResolveProxyLocalWriteFailureIsNotUpstreamFault 覆盖 spec §5.1 第四条：
// 本地 blob 写入失败 → 不得产生任何上游判定事件、对外 500（ErrLocalStorage），
// 且日志必须指向本地磁盘而不是上游。
//
// 两个形态分别对应「读正文之前就失败」和「已传完才失败」——后者尤其关键：
// 有传输进展 + 写失败绝不能被误判成 slow_rate。
func TestResolveProxyLocalWriteFailureIsNotUpstreamFault(t *testing.T) {
	payload := largePayload(64 << 10)
	// 直连 proxy：本地写失败必须对外 500，而不是 502/504。
	t.Run("临时目录不可创建", func(t *testing.T) {
		const path = "com/example/fr158/local-tmp.jar"
		// 让 blob 根指向一个普通文件：Store 在其下创建 tmp 目录时会失败（ENOTDIR）。
		root := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
			t.Fatalf("准备占位文件：%v", err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}))
		t.Cleanup(srv.Close)

		svc, repos, spy := newFR158ServiceWithStore(t, blobstore.NewStore(root))
		repoID, err := repos.Create("local-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
		if err != nil {
			t.Fatalf("建 proxy 仓库：%v", err)
		}

		var resolveErr error
		logs := singlePathLog(t, path, func() {
			_, _, resolveErr = svc.Resolve(context.Background(), "local-proxy", path)
		})

		if !errors.Is(resolveErr, domain.ErrLocalStorage) {
			t.Fatalf("本地写失败应返回 ErrLocalStorage（对外 500），实际：%v", resolveErr)
		}
		for _, unwanted := range []error{domain.ErrUpstream, domain.ErrUpstreamTimeout, domain.ErrNotFound} {
			if errors.Is(resolveErr, unwanted) {
				t.Fatalf("本地写失败不得满足 %v（会被误报成上游故障或假 404）：%v", unwanted, resolveErr)
			}
		}
		// 最关键的一条：**不产生任何上游判定事件**。
		if reasons := spy.reasons(); len(reasons) != 0 {
			t.Fatalf("本地写失败不得产生任何上游判定事件，实际 %v", reasons)
		}
		assertNotBlocked(t, svc, repoID)
		if !strings.Contains(logs, "category=local_write_failure") {
			t.Fatalf("日志类别应为 local_write_failure：%s", logs)
		}
	})

	t.Run("已传完才落盘失败", func(t *testing.T) {
		const path = "com/example/fr158/local-after-progress.jar"
		root := t.TempDir()
		// 预先在分片目录位置放一个普通文件：正文传完后创建分片目录必失败，
		// 从而构造「transferred == declared 且仍写失败」这一最容易被误判成 slow_rate 的现场。
		sum := sha256.Sum256(payload)
		hash := hex.EncodeToString(sum[:])
		if err := os.MkdirAll(filepath.Join(root, hash[0:2]), 0o750); err != nil {
			t.Fatalf("准备分片目录：%v", err)
		}
		if err := os.WriteFile(filepath.Join(root, hash[0:2], hash[2:4]), []byte("x"), 0o600); err != nil {
			t.Fatalf("准备分片占位文件：%v", err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
		}))
		t.Cleanup(srv.Close)

		svc, repos, spy := newFR158ServiceWithStore(t, blobstore.NewStore(root))
		repoID, err := repos.Create("local-progress-proxy", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
		if err != nil {
			t.Fatalf("建 proxy 仓库：%v", err)
		}

		var resolveErr error
		logs := singlePathLog(t, path, func() {
			_, _, resolveErr = svc.Resolve(context.Background(), "local-progress-proxy", path)
		})

		if !errors.Is(resolveErr, domain.ErrLocalStorage) {
			t.Fatalf("已传完才落盘失败仍应返回 ErrLocalStorage，实际：%v", resolveErr)
		}
		if reasons := spy.reasons(); len(reasons) != 0 {
			t.Fatalf("有传输进展的本地写失败同样不得产生上游判定事件，实际 %v", reasons)
		}
		assertNotBlocked(t, svc, repoID)
		if !strings.Contains(logs, "category=local_write_failure") {
			t.Fatalf("日志类别应为 local_write_failure：%s", logs)
		}
		if strings.Contains(logs, "category=slow_rate") {
			t.Fatalf("有进展的本地写失败不得被误判成 slow_rate：%s", logs)
		}
	})
}

// TestResolveGroupLocalWriteFailureSurfacesAsLocal 群组聚合读时，成员的本地写失败
// 不得被折叠成 502「上游不可达」：它是服务端自身故障，整体对外 500。
func TestResolveGroupLocalWriteFailureSurfacesAsLocal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备占位文件：%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("group artifact"))
	}))
	t.Cleanup(srv.Close)

	svc, repos, spy := newFR158ServiceWithStore(t, blobstore.NewStore(root))
	if _, err := repos.Create("group-member", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL)); err != nil {
		t.Fatalf("建成员仓库：%v", err)
	}
	if _, err := repos.Create("g-local", "raw", "group", "private", groupConfigJSON(t, "group-member")); err != nil {
		t.Fatalf("建 group 仓库：%v", err)
	}

	_, _, err := svc.Resolve(context.Background(), "g-local", "pkg/group-local.jar")
	if !errors.Is(err, domain.ErrLocalStorage) {
		t.Fatalf("group 的本地写失败应返回 ErrLocalStorage（对外 500），实际：%v", err)
	}
	if errors.Is(err, domain.ErrUpstream) {
		t.Fatalf("group 的本地写失败不得被折叠成 502 上游故障，实际：%v", err)
	}
	if reasons := spy.reasons(); len(reasons) != 0 {
		t.Fatalf("group 的本地写失败不得产生上游判定事件，实际 %v", reasons)
	}
}

// newFR158ServiceWithStore 与前者的区别仅在可注入 blob 存储。
func newFR158ServiceWithStore(t *testing.T, blobs *blobstore.Store) (*domain.AssetService, *repository.RepoRepo, *blockEventSpy) {
	t.Helper()
	svc, repos := newAssetServiceWithStore(t, blobs)
	spy := &blockEventSpy{}
	svc.SetUpstreamBlockRecorder(spy)
	return svc, repos, spy
}

// TestResolveOCIProxyTransferFailureClassified OCI 回源链路的传输阶段细分（spec §6 R4）：
// ReadOCIPath 返回 nil 错误即代表已拿到 2xx 响应头，因此与 raw/maven 同构，
// 慢回源应记 slow_transfer 且不阻断，而不是落到 500 / 502。
func TestResolveOCIProxyTransferFailureClassified(t *testing.T) {
	srv, _ := trickleBodyServer(t, fr158Declared, 1024, 20*time.Millisecond)
	svc, repos, spy := newFR158Service(t)
	repoID, err := repos.Create("oci-slow-proxy", "docker", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 OCI proxy 仓库：%v", err)
	}

	const path = "v2/lib/fr158/manifests/latest"
	_, _, resolveErr := svc.ResolveOCIProxy(context.Background(), "oci-slow-proxy", path, "repository:lib/fr158:pull", "")
	if !errors.Is(resolveErr, domain.ErrUpstreamTimeout) {
		t.Fatalf("OCI 慢回源应返回 ErrUpstreamTimeout（对外 504），实际：%v", resolveErr)
	}
	if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != "slow_transfer" {
		t.Fatalf("OCI 慢回源应记一次 slow_transfer，实际 %v", reasons)
	}
	assertNotBlocked(t, svc, repoID)
}
