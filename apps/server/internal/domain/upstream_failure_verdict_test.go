package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件覆盖 FR-155 的核心判据：回源失败的「慢 / 不可达」分类与收口。
// 分类只有一处实现（classifyUpstreamFailure），由 fetchProxyAsset 与 ResolveOCIProxy 共用，
// 因此这里直接对判定函数与收口函数做表驱动断言，避免两个调用点各写一套判定后漂移。

// timeoutError 模拟「网络层超时」错误（实现 Timeout() 契约），
// 用于钉住 upstream.IsTimeout 的第二条来源：回源客户端自身超时（*url.Error）。
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// dialFailure 构造「连接层失败」错误：*url.Error 内是 net.OpError，
// 语义为连不上（非超时），是必须仍触发仓库级阻断的一侧。
func dialFailure() error {
	return &url.Error{
		Op:  "Get",
		URL: "http://upstream.example/artifact.jar",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
	}
}

// TestClassifyUpstreamFailure 分类表（spec §3.2）：判定顺序必须与 mapUpstreamErr 一致，
// 断言 (reason, block) 与语义类别，五类核心样例 + 包装/来源变体。
func TestClassifyUpstreamFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantKind   upstreamFailureKind
		wantReason UpstreamBlockReason
		wantBlock  bool
	}{
		{
			name:       "404 上游资源缺失：不阻断且不记事件",
			err:        upstream.ErrNotFound,
			wantKind:   upstreamFailureNotFound,
			wantReason: "",
			wantBlock:  false,
		},
		{
			name:       "包装后的 404 仍判为资源缺失（判定顺序优先 errors.Is）",
			err:        fmt.Errorf("回源读取：%w", upstream.ErrNotFound),
			wantKind:   upstreamFailureNotFound,
			wantReason: "",
			wantBlock:  false,
		},
		{
			name:       "410 上游资源下架：不阻断且不记事件",
			err:        upstream.ErrGone,
			wantKind:   upstreamFailureGone,
			wantReason: "",
			wantBlock:  false,
		},
		{
			name:       "超时（调用方预算耗尽 context.DeadlineExceeded）：不阻断，记 upstream_timeout",
			err:        fmt.Errorf("回源中断：%w", context.DeadlineExceeded),
			wantKind:   upstreamFailureTimeout,
			wantReason: UpstreamBlockReasonTimeout,
			wantBlock:  false,
		},
		{
			name:       "超时（回源客户端自身超时 *url.Error）：同样不阻断",
			err:        &url.Error{Op: "Get", URL: "http://upstream.example/big.jar", Err: timeoutError{}},
			wantKind:   upstreamFailureTimeout,
			wantReason: UpstreamBlockReasonTimeout,
			wantBlock:  false,
		},
		{
			name:       "连接失败：阻断，记 upstream_error",
			err:        dialFailure(),
			wantKind:   upstreamFailureUnreachable,
			wantReason: UpstreamBlockReasonUpstreamError,
			wantBlock:  true,
		},
		{
			name:       "DNS 解析失败：阻断",
			err:        &url.Error{Op: "Get", URL: "http://nope.invalid/a.jar", Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}},
			wantKind:   upstreamFailureUnreachable,
			wantReason: UpstreamBlockReasonUpstreamError,
			wantBlock:  true,
		},
		{
			name:       "上游 5xx：阻断",
			err:        &upstream.StatusError{Code: 503},
			wantKind:   upstreamFailureUnreachable,
			wantReason: UpstreamBlockReasonUpstreamError,
			wantBlock:  true,
		},
		{
			name:       "上游 401：阻断（与 shouldBlockStatus 口径一致）",
			err:        &upstream.StatusError{Code: 401},
			wantKind:   upstreamFailureUnreachable,
			wantReason: UpstreamBlockReasonUpstreamError,
			wantBlock:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyUpstreamFailure(tc.err)
			if got.kind != tc.wantKind {
				t.Fatalf("语义类别 = %v，期望 %v", got.kind, tc.wantKind)
			}
			if got.reason != tc.wantReason {
				t.Fatalf("reason = %q，期望 %q", got.reason, tc.wantReason)
			}
			if got.block != tc.wantBlock {
				t.Fatalf("block = %v，期望 %v", got.block, tc.wantBlock)
			}
			// 分类只做判定，不得有副作用：404/410 不应带出阻断事件类别。
			if !got.block && got.kind != upstreamFailureTimeout && got.reason != "" {
				t.Fatalf("上游可达的豁免类别不应产生事件类别，实际 %q", got.reason)
			}
		})
	}
}

// TestSettleUpstreamFailureTimeoutKeepsStateAndFreesProbe 超时收口：
// 不改状态、不推进退避、不启动探测线程，但必须释放半开名额并记 upstream_timeout 事件。
func TestSettleUpstreamFailureTimeoutKeepsStateAndFreesProbe(t *testing.T) {
	t.Parallel()
	const repoID int64 = 41
	spy := &recorderSpy{}
	h := newProxyHealth(func(context.Context, string, string) error { return nil })
	h.setBlockRecorder(spy)
	newHalfOpenRepo(h, repoID)
	if !h.beginUpstream(repoID) {
		t.Fatal("半开且名额空闲时应放行本轮探测")
	}
	if h.beginUpstream(repoID) {
		t.Fatal("名额占用期间应拒绝其它请求（释放效果的对照基线）")
	}
	svc := &AssetService{health: h}

	before := h.status(repoID)
	svc.settleUpstreamFailure(repoID, "http://upstream", "", classifyUpstreamFailure(context.DeadlineExceeded))
	after := h.status(repoID)

	if after != before {
		t.Fatalf("超时不得改动状态与退避窗口：before=%+v after=%+v", before, after)
	}
	if !h.beginUpstream(repoID) {
		t.Fatal("超时必须释放半开名额，否则后续请求会在 probeInFlight 处被无限快速失败")
	}
	if got := spy.reasons(); len(got) != 1 || got[0] != string(UpstreamBlockReasonTimeout) {
		t.Fatalf("应记录一次 upstream_timeout，实际 %v", got)
	}
	h.mu.Lock()
	stopCh := h.repos[repoID].stopCh
	h.mu.Unlock()
	if stopCh != nil {
		t.Fatal("超时不是状态判据，不应启动后台探测线程")
	}
}

// TestSettleUpstreamFailureUnreachableBlocks 不可达收口：仍有仓库级阻断，
// 并按既有退避规则推进一档、记 upstream_error——快速失败的保护不回退。
func TestSettleUpstreamFailureUnreachableBlocks(t *testing.T) {
	t.Parallel()
	const repoID int64 = 43
	spy := &recorderSpy{}
	h := newProxyHealth(func(context.Context, string, string) error { return errors.New("boom") })
	h.setBase(deterministicBase)
	h.setBlockRecorder(spy)
	stopProbe(t, h, repoID)
	seedStateSuppressingProbe(h, repoID)
	svc := &AssetService{health: h}

	svc.settleUpstreamFailure(repoID, "http://upstream", "", classifyUpstreamFailure(&upstream.StatusError{Code: 503}))

	got := h.status(repoID)
	if got.Status != StatusAutoBlocked {
		t.Fatalf("5xx 应进入 AUTO_BLOCKED，实际 %s", got.Status)
	}
	if got.BlockedFor != deterministicBase || got.BlockedUntil.IsZero() {
		t.Fatalf("应开启起始档阻止窗口，实际 %+v", got)
	}
	if reasons := spy.reasons(); len(reasons) != 1 || reasons[0] != string(UpstreamBlockReasonUpstreamError) {
		t.Fatalf("应记录一次 upstream_error，实际 %v", reasons)
	}
}
