package domain_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 本文件钉住「真实回源请求结束时记一次采样」这条接入（规格 §2.1 / §3.2）：
// 采样必须复用五分类的判定结果、类别落列正确，且 local_write_failure 与 canceled 不计入。
//
// 走的是真实的 proxy 回源链路（Resolve → fetchProxyAsset → upstream 客户端），
// 不是直接调采样服务——否则「接了线但分类没跟着走」这类缺陷测不出来。

// requestSamples 取出确认落盘的 request 系列采样（按仓库过滤）。
func requestSamples(t *testing.T, recorder *recordingUpstreamHealthRecorder, healthSvc *domain.UpstreamHealthService, repoID int64) []repository.UpstreamHealthDelta {
	t.Helper()
	if err := healthSvc.Flush(); err != nil {
		t.Fatalf("落盘采样：%v", err)
	}
	var out []repository.UpstreamHealthDelta
	for _, delta := range recorder.snapshot() {
		if delta.RepositoryID == repoID && delta.Source == repository.UpstreamHealthSourceRequest {
			out = append(out, delta)
		}
	}
	return out
}

// 成功回源记一次 ok 采样，并带上成功样本的延迟。
func TestProxyResolveRecordsOkSample(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream payload"))
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)
	repoID, err := repos.Create("sample-ok", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	// 必须读并关闭返回的 reader：它持有落盘文件的句柄，不关会在 Windows 上
	// 让临时目录无法清理（tempdir cleanup 失败会把用例判成失败）。
	_, rc, err := svc.Resolve(context.Background(), "sample-ok", "dir/file.txt")
	if err != nil {
		t.Fatalf("回源应成功：%v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	samples := requestSamples(t, recorder, healthSvc, repoID)
	if len(samples) != 1 {
		t.Fatalf("应记录 1 条 request 采样，实际 %d 条：%+v", len(samples), samples)
	}
	if samples[0].OK != 1 || samples[0].Samples != 1 {
		t.Fatalf("成功回源应计入 ok 列：%+v", samples[0])
	}
	if samples[0].LatencyCountMs != 1 {
		t.Fatalf("成功样本应计入延迟统计：%+v", samples[0])
	}
}

// 上游 5xx 记一次 unreachable 采样（与 classifyUpstreamFailure 同源）。
func TestProxyResolveRecordsUnreachableSample(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)
	repoID, err := repos.Create("sample-500", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "sample-500", "dir/file.txt"); err == nil {
		t.Fatalf("上游 500 时回源应失败")
	}
	samples := requestSamples(t, recorder, healthSvc, repoID)
	if len(samples) != 1 {
		t.Fatalf("应记录 1 条 request 采样，实际 %d 条：%+v", len(samples), samples)
	}
	if samples[0].Unreachable != 1 {
		t.Fatalf("上游 5xx 应计入 unreachable 列：%+v", samples[0])
	}
}

// 上游 404 记一次 ok 采样：上游「可达但没这个制品」证明的是连通性，
// 把它算成失败会让连通率把「上游好好的，只是没这个文件」误报成上游故障。
func TestProxyResolveRecordsConfirmedMissingAsOk(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)
	repoID, err := repos.Create("sample-404", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	if _, _, err := svc.Resolve(context.Background(), "sample-404", "dir/missing.txt"); err == nil {
		t.Fatalf("上游 404 时回源应返回未命中")
	}
	samples := requestSamples(t, recorder, healthSvc, repoID)
	if len(samples) != 1 {
		t.Fatalf("应记录 1 条 request 采样，实际 %d 条：%+v", len(samples), samples)
	}
	if samples[0].OK != 1 {
		t.Fatalf("404 应计入 ok 列（上游可达）：%+v", samples[0])
	}
}

// 请求被取消时**不记录任何列**：取消是正常的请求生命周期终点，不是上游健康信号。
func TestProxyResolveCanceledRecordsNothing(t *testing.T) {
	t.Parallel()
	// 上游先拿到请求再长时间不响应，让调用方有时间取消。
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	svc, repos := newAssetService(t)
	recorder := &recordingUpstreamHealthRecorder{}
	healthSvc := domain.NewUpstreamHealthService(recorder)
	svc.SetUpstreamHealthService(healthSvc)
	repoID, err := repos.Create("sample-cancel", "raw", "proxy", "private", proxyConfigJSON(t, srv.URL))
	if err != nil {
		t.Fatalf("建 proxy 仓库：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, _, _ = svc.Resolve(ctx, "sample-cancel", "dir/file.txt")

	if samples := requestSamples(t, recorder, healthSvc, repoID); len(samples) != 0 {
		t.Fatalf("取消的请求不得产生采样，实际 %+v", samples)
	}
}
