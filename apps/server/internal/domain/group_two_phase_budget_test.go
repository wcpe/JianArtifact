package domain_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/blobstore"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件覆盖 group 回源预算与落盘预算解耦的验收用例。
//
// 预算模型：group 成员的 3s（groupMemberTimeout）只约束「该成员是否及时给出响应头」，
// 一旦拿到 2xx 响应头就撤销该预算，剩余落盘预算改由回源整体超时（JIAN_UPSTREAM_TIMEOUT）承担。
// 因此大制品用例必须用远超 3s 的回源客户端超时，这里另建助手而不复用 newAssetService（5s 过紧）。

// newAssetServiceWithUpstreamTimeout 装配一个回源整体超时可指定的 AssetService。
// 大制品用例需要落盘预算明显大于成员确认预算（3s），否则无法区分两者。
func newAssetServiceWithUpstreamTimeout(t *testing.T, timeout time.Duration) (*domain.AssetService, *repository.RepoRepo) {
	t.Helper()
	db := newTestDB(t)
	repos := repository.NewRepoRepo(db)
	assets := repository.NewAssetRepo(db)
	blobs := blobstore.NewStore(t.TempDir())
	return domain.NewAssetService(repos, assets, blobs, upstream.NewTestClient(timeout)), repos
}

// largePayload 生成确定性的大制品载荷（避免随机源让校验和断言不可复现）。
func largePayload(size int) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i * 31 % 251)
	}
	return buf
}

// flushNow 立即把已写内容推给客户端：确认信号依赖响应头真的到达客户端。
func flushNow(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// slowArtifactServer 构造「慢制品」上游夹具：立即回 200 响应头并 Flush，
// 随后按 chunk 分块滴流写 payload，块间 sleep interval。
//
// 这正是该解耦要解决的现场：响应头秒回（成员确认命中），但整份下载远超 3s 确认预算。
// 客户端断开（ctx 结束）时立即返回，保证 srv.Close 不被挂起请求拖住。
func slowArtifactServer(t *testing.T, payload []byte, chunk int, interval time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/java-archive")
		w.WriteHeader(http.StatusOK)
		flushNow(w)
		for off := 0; off < len(payload); off += chunk {
			if _, err := w.Write(payload[off:min(off+chunk, len(payload))]); err != nil {
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
	return srv, &hits
}

// proxyMember 描述一个待创建的 proxy 成员：名字 + 上游地址。
// 用切片而非 map 传入，保证成员顺序确定（组成员顺序即优先级，不能被 map 遍历打乱）。
type proxyMember struct {
	name      string
	remoteURL string
}

// createGroupWithProxyMembers 按给定顺序建 proxy 成员，再建引用它们的 group 仓库。
func createGroupWithProxyMembers(t *testing.T, repos *repository.RepoRepo, groupName string, members ...proxyMember) {
	t.Helper()
	names := make([]string, 0, len(members))
	for _, m := range members {
		if _, err := repos.Create(m.name, "raw", "proxy", "private", proxyConfigJSON(t, m.remoteURL)); err != nil {
			t.Fatalf("建 proxy 成员 %s：%v", m.name, err)
		}
		names = append(names, m.name)
	}
	if _, err := repos.Create(groupName, "raw", "group", "private", groupConfigJSON(t, names...)); err != nil {
		t.Fatalf("建 group 仓库 %s：%v", groupName, err)
	}
}

// TestGroupLargeArtifactSucceedsBeyondConfirmBudget 是大制品主用例（spec §5.1 第一条）：
// 15MB 制品、约 5MB/s 的慢上游（总耗时约 4.3s，明显超过 3s 确认预算），
// group 读必须成功返回，且字节数、sha256/sha1/md5 与上游一致。
//
// 改造前该用例必然失败：3s 成员预算在下载途中到期，resp.Body.Read 失败，
// 整个制品被折叠成 404（用户看到的「15MB universal jar 必挂」）。
func TestGroupLargeArtifactSucceedsBeyondConfirmBudget(t *testing.T) {
	t.Parallel()
	const size = 15 << 20 // 15MB
	payload := largePayload(size)
	// 256KB / 72ms ≈ 3.5MB/s，全程约 4.3s：明确越过 3s 确认预算，同时控制用例时长。
	srv, hits := slowArtifactServer(t, payload, 256<<10, 72*time.Millisecond)
	defer srv.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, 30*time.Second)
	createGroupWithProxyMembers(t, repos, "g-big", proxyMember{name: "big-proxy", remoteURL: srv.URL})

	start := time.Now()
	asset, rc, err := svc.Resolve(context.Background(), "g-big", "com/example/app/1.0.0/app-1.0.0-universal.jar")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("大制品应回源成功，实际：%v（耗时 %s）", err, elapsed)
	}
	body := readClose(t, rc)
	if len(body) != size {
		t.Fatalf("制品字节数 = %d，期望 %d", len(body), size)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("制品内容与上游不一致")
	}
	if elapsed <= 3*time.Second {
		t.Fatalf("用例未越过 3s 确认预算（耗时 %s），无法证明预算已解耦", elapsed)
	}
	// 校验和一致性：本地缓存元数据应与上游字节完全对应。
	sum256 := sha256.Sum256(payload)
	sum1 := sha1.Sum(payload)
	sumMD5 := md5.Sum(payload)
	if asset.Size != size {
		t.Fatalf("asset.Size = %d，期望 %d", asset.Size, size)
	}
	if asset.BlobHash != hex.EncodeToString(sum256[:]) {
		t.Fatalf("sha256 不一致：%q", asset.BlobHash)
	}
	if asset.Sha1 != hex.EncodeToString(sum1[:]) {
		t.Fatalf("sha1 不一致：%q", asset.Sha1)
	}
	if asset.Md5 != hex.EncodeToString(sumMD5[:]) {
		t.Fatalf("md5 不一致：%q", asset.Md5)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("大制品应只回源 1 次，实际 %d 次", got)
	}
}

// TestGroupConfirmedHitButStagingTimeoutIs504 覆盖 spec §3.6 的核心要求：
// 上游立即回 200 头（确认命中）但 body 长期滴流，落盘阶段撞上回源整体超时（1s）时，
// 必须返回 ErrUpstreamTimeout（对外 504），既不能是 ErrNotFound（假 404），
// 也不能是裸错误（会被协议层映射成 500）；且该路径不得写负缓存——同路径第二次仍会回源。
func TestGroupConfirmedHitButStagingTimeoutIs504(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/java-archive")
		w.WriteHeader(http.StatusOK)
		flushNow(w) // 立即给出响应头：成员已「确认命中」
		// body 长期滴流：落盘必然撞上 1s 的回源整体超时。
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
				if _, err := w.Write([]byte("drip")); err != nil {
					return
				}
				flushNow(w)
			}
		}
	}))
	defer srv.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, time.Second)
	createGroupWithProxyMembers(t, repos, "g-stage", proxyMember{name: "stage-proxy", remoteURL: srv.URL})

	_, _, err := svc.Resolve(context.Background(), "g-stage", "app.jar")
	if !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("确认命中但落盘超时应返回 ErrUpstreamTimeout（对外 504），实际：%v", err)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("确认命中后失败不得折叠成 ErrNotFound，实际：%v", err)
	}
	if errors.Is(err, domain.ErrUpstream) {
		t.Fatalf("超时不应同时满足 ErrUpstream（会被映射成 502），实际：%v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("首次应回源 1 次，实际 %d 次", got)
	}

	// 未写负缓存：第二次仍然回源（命中计数 +1），而不是直接 404。
	_, _, err2 := svc.Resolve(context.Background(), "g-stage", "app.jar")
	if !errors.Is(err2, domain.ErrUpstreamTimeout) {
		t.Fatalf("二次应仍为 ErrUpstreamTimeout，实际：%v", err2)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("落盘失败不得写负缓存，二次应重新回源（共 2 次），实际 %d 次", got)
	}
}

// TestGroupRealMissingStaysFast404 验证真实缺失路径不退化（I1）：
// 成员上游确认 404 时整体仍是 ErrNotFound、耗时在 3s 内，且 group 级负缓存照常写入。
func TestGroupRealMissingStaysFast404(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, 30*time.Second)
	createGroupWithProxyMembers(t, repos, "g-missing", proxyMember{name: "nf-proxy", remoteURL: srv.URL})

	start := time.Now()
	_, _, err := svc.Resolve(context.Background(), "g-missing", "missing.jar")
	elapsed := time.Since(start)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("真实缺失应返回 ErrNotFound，实际：%v", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("真实缺失应在 3s 内返回（成员确认命中的快速返回不得退化），实际耗时 %s", elapsed)
	}
	// 负缓存已写：第二次直接 404，不再探测成员。
	if _, _, err := svc.Resolve(context.Background(), "g-missing", "missing.jar"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("二次应命中负缓存返回 ErrNotFound，实际：%v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("负缓存命中后不应再探测成员，实际 %d 次", got)
	}
}

// TestGroupHangingMemberSkippedWithinWatchdog 验证无响应上游不拖慢整体（I1）：
// 一个成员完全挂起（直到 ctx 结束都不给响应头），另一个成员快速 404；
// 整体应在看门狗（3s）量级返回，且挂起成员的上游只被请求 1 次。
//
// 返回码口径按 FIX-1：挂起成员属「不可判定且是超时」→ ErrUpstreamTimeout（504），不得折叠成 404。
// 断言的是耗时上界（看门狗 3s，故上界取 4s）与上游命中次数，不是旧的 404 返回值。
func TestGroupHangingMemberSkippedWithinWatchdog(t *testing.T) {
	t.Parallel()
	var hangHits, fastHits int32
	hangSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hangHits, 1)
		<-r.Context().Done()
	}))
	defer hangSrv.Close()
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fastHits, 1)
		http.NotFound(w, r)
	}))
	defer fastSrv.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, 30*time.Second)
	createGroupWithProxyMembers(t, repos, "g-hang",
		proxyMember{name: "hang-proxy", remoteURL: hangSrv.URL},
		proxyMember{name: "fast-proxy", remoteURL: fastSrv.URL},
	)

	start := time.Now()
	_, _, err := svc.Resolve(context.Background(), "g-hang", "missing.jar")
	elapsed := time.Since(start)
	if !errors.Is(err, domain.ErrUpstreamTimeout) {
		t.Fatalf("挂起成员应判为超时（ErrUpstreamTimeout），实际：%v（耗时 %s）", err, elapsed)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("不可判定的超时不得折叠成 ErrNotFound，实际：%v（耗时 %s）", err, elapsed)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("挂起成员应被看门狗在 3s 量级跳过，实际耗时 %s", elapsed)
	}
	if got := atomic.LoadInt32(&hangHits); got != 1 {
		t.Fatalf("挂起成员只应被请求 1 次，实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&fastHits); got != 1 {
		t.Fatalf("快速 404 成员应被请求 1 次，实际 %d 次", got)
	}
}

// TestGroupLargeArtifactSingleFlight 验证 single-flight 不回退（I4）：
// N 个并发请求读同一 group 同路径大制品，各成员上游命中计数均为 1，且所有响应体一致。
func TestGroupLargeArtifactSingleFlight(t *testing.T) {
	t.Parallel()
	// 1MB / 64KB / 20ms ≈ 320ms：足以让全部并发请求落进同一 single-flight 临界区。
	payload := largePayload(1 << 20)
	srvA, hitsA := slowArtifactServer(t, payload, 64<<10, 20*time.Millisecond)
	defer srvA.Close()
	srvB, hitsB := slowArtifactServer(t, payload, 64<<10, 20*time.Millisecond)
	defer srvB.Close()

	svc, repos := newAssetServiceWithUpstreamTimeout(t, 30*time.Second)
	createGroupWithProxyMembers(t, repos, "g-sf",
		proxyMember{name: "sf-proxy-a", remoteURL: srvA.URL},
		proxyMember{name: "sf-proxy-b", remoteURL: srvB.URL},
	)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([][]byte, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, rc, err := svc.Resolve(context.Background(), "g-sf", "lib/big.jar")
			if err != nil {
				errs[idx] = err
				return
			}
			defer func() { _ = rc.Close() }()
			bodies[idx], errs[idx] = io.ReadAll(rc)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 Resolve[%d] 失败：%v", i, err)
		}
		if !bytes.Equal(bodies[i], payload) {
			t.Fatalf("并发 Resolve[%d] 内容不一致（%d 字节）", i, len(bodies[i]))
		}
	}
	if got := atomic.LoadInt32(hitsA); got != 1 {
		t.Fatalf("成员 A 应只回源 1 次（single-flight），实际 %d 次", got)
	}
	if got := atomic.LoadInt32(hitsB); got != 1 {
		t.Fatalf("成员 B 应只回源 1 次（single-flight），实际 %d 次", got)
	}
}
