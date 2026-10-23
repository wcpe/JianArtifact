package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// logSinkFunc 适配 log.SetOutput 需要的 io.Writer（package domain 内自用，
// 与 package domain_test 里的同名助手互不影响）。
type logSinkFunc func([]byte) (int, error)

func (f logSinkFunc) Write(p []byte) (int, error) { return f(p) }

// 本文件覆盖 FR-158（回源失败原因五分类）的**判定层**用例：
// countingReader、declaredContentLength、classifyTransferFailure 的类别表、
// 以及「三类传输失败都不阻断」的收口结论。
//
// 端到端（真实上游夹具 + 日志 + 指标）的用例见 transfer_failure_resolve_test.go。

// timeoutErr 复用 upstream_failure_verdict_test.go 里既有的 timeoutError，
// 这里只补齐「超时包装了 mid-stream 断流」这一 R1 边界所需的组合错误。
func timeoutWrapped(eof error) error {
	return fmt.Errorf("读取上游正文：%w", errors.Join(eof, timeoutError{}))
}

// TestClassifyTransferFailure 传输阶段判定表（spec §3.2 与 §6 R1/R3）。
//
// 关键约束：block 必须恒为 false——三类传输失败（no_response / slow_rate /
// transfer_aborted）都不是「上游不可达」的证据，不得触发仓库级阻断。
func TestClassifyTransferFailure(t *testing.T) {
	t.Parallel()
	const declared15M = int64(15 << 20)
	cases := []struct {
		name         string
		err          error
		transferred  int64
		declared     int64
		wantKind     upstreamFailureKind
		wantReason   UpstreamBlockReason
		wantCategory string
	}{
		{
			name:         "超时且有传输进展：带宽不足 → slow_rate",
			err:          &url.Error{Op: "Get", URL: "http://upstream.example/big.jar", Err: timeoutError{}},
			transferred:  1 << 20,
			declared:     declared15M,
			wantKind:     upstreamFailureTimeout,
			wantReason:   UpstreamBlockReasonSlowTransfer,
			wantCategory: upstreamCategorySlowRate,
		},
		{
			name:         "超时且一字节未传出：拿到头也没用 → no_response",
			err:          &url.Error{Op: "Get", URL: "http://upstream.example/big.jar", Err: timeoutError{}},
			transferred:  0,
			declared:     declared15M,
			wantKind:     upstreamFailureTimeout,
			wantReason:   UpstreamBlockReasonTimeout,
			wantCategory: upstreamCategoryNoResponse,
		},
		{
			name:         "R1 超时优先：超时错误同时带 mid-stream EOF 仍判 slow_rate",
			err:          timeoutWrapped(io.ErrUnexpectedEOF),
			transferred:  4 << 20,
			declared:     declared15M,
			wantKind:     upstreamFailureTimeout,
			wantReason:   UpstreamBlockReasonSlowTransfer,
			wantCategory: upstreamCategorySlowRate,
		},
		{
			name:         "超时且无声明长度、零进展：仍判 no_response（R2）",
			err:          context.DeadlineExceeded,
			transferred:  0,
			declared:     -1,
			wantKind:     upstreamFailureTimeout,
			wantReason:   UpstreamBlockReasonTimeout,
			wantCategory: upstreamCategoryNoResponse,
		},
		{
			name:         "正文短于声明长度（io.ErrUnexpectedEOF）：中途断 → transfer_aborted",
			err:          fmt.Errorf("写入临时 blob：%w", io.ErrUnexpectedEOF),
			transferred:  1 << 20,
			declared:     declared15M,
			wantKind:     upstreamFailureTransferAborted,
			wantReason:   UpstreamBlockReasonTransferAborted,
			wantCategory: upstreamCategoryTransferAborted,
		},
		{
			name:         "连接被重置：中途断 → transfer_aborted",
			err:          &url.Error{Op: "Get", URL: "http://upstream.example/big.jar", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
			transferred:  1 << 20,
			declared:     -1,
			wantKind:     upstreamFailureTransferAborted,
			wantReason:   UpstreamBlockReasonTransferAborted,
			wantCategory: upstreamCategoryTransferAborted,
		},
		{
			name:         "裸 io.EOF 且声明未传满：上游提前断流 → transfer_aborted",
			err:          io.EOF,
			transferred:  1 << 20,
			declared:     declared15M,
			wantKind:     upstreamFailureTransferAborted,
			wantReason:   UpstreamBlockReasonTransferAborted,
			wantCategory: upstreamCategoryTransferAborted,
		},
		{
			name:         "裸 io.EOF 且无声明长度：偏向上游断开而非 500（R3）",
			err:          io.EOF,
			transferred:  1 << 20,
			declared:     -1,
			wantKind:     upstreamFailureTransferAborted,
			wantReason:   UpstreamBlockReasonTransferAborted,
			wantCategory: upstreamCategoryTransferAborted,
		},
		{
			name:         "本地磁盘写满：不是上游故障 → local_write_failure",
			err:          fmt.Errorf("写入临时 blob：%w", &os.PathError{Op: "write", Path: "/data/blobs/tmp/blob-1", Err: syscall.ENOSPC}),
			transferred:  2 << 20,
			declared:     declared15M,
			wantKind:     upstreamFailureLocalWrite,
			wantReason:   "",
			wantCategory: upstreamCategoryLocalWriteFailure,
		},
		{
			name:         "本地目录权限不足：不是上游故障 → local_write_failure",
			err:          fmt.Errorf("创建临时目录：%w", &os.PathError{Op: "mkdir", Path: "/data/blobs/tmp", Err: syscall.EACCES}),
			transferred:  0,
			declared:     declared15M,
			wantKind:     upstreamFailureLocalWrite,
			wantReason:   "",
			wantCategory: upstreamCategoryLocalWriteFailure,
		},
		{
			name:         "请求被取消：既不记上游事件也不记本地故障",
			err:          fmt.Errorf("写入临时 blob：%w", context.Canceled),
			transferred:  1 << 20,
			declared:     declared15M,
			wantKind:     upstreamFailureCanceled,
			wantReason:   "",
			wantCategory: upstreamCategoryCanceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyTransferFailure(tc.err, tc.transferred, tc.declared)
			if got.kind != tc.wantKind {
				t.Fatalf("语义类别 = %v，期望 %v", got.kind, tc.wantKind)
			}
			if got.reason != tc.wantReason {
				t.Fatalf("reason = %q，期望 %q", got.reason, tc.wantReason)
			}
			if got.category != tc.wantCategory {
				t.Fatalf("可观测类别 = %q，期望 %q", got.category, tc.wantCategory)
			}
			// 传输阶段的任何失败都不得阻断仓库：它们都不是「上游不可达」的证据。
			if got.block {
				t.Fatal("传输阶段失败不得触发仓库级阻断")
			}
		})
	}
}

// TestClassifyUpstreamFailureCategories 「拿头之前」的分类同样带出可观测类别，
// 保证五分类在两个阶段用同一套词汇（不新增第二套命名）。
func TestClassifyUpstreamFailureCategories(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		err          error
		wantCategory string
	}{
		{name: "404 确认不存在", err: fmt.Errorf("回源：%w", upstream.ErrNotFound), wantCategory: upstreamCategoryConfirmedMissing},
		{name: "超时未拿到响应头", err: context.DeadlineExceeded, wantCategory: upstreamCategoryNoResponse},
		{name: "连接失败", err: dialFailure(), wantCategory: upstreamCategoryUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyUpstreamFailure(tc.err).category; got != tc.wantCategory {
				t.Fatalf("可观测类别 = %q，期望 %q", got, tc.wantCategory)
			}
		})
	}
}

// TestCountingReaderCountsReadBytes 计数 reader 只累加成功读出的字节数，
// 错误读完的那一次也要计入（上游确实已经把那些字节发过来了）。
func TestCountingReaderCountsReadBytes(t *testing.T) {
	t.Parallel()
	reader := &countingReader{r: strings.NewReader("0123456789")}
	buf := make([]byte, 4)
	var total int
	for {
		n, err := reader.Read(buf)
		total += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读取：%v", err)
		}
	}
	if total != 10 {
		t.Fatalf("共读出 %d 字节，期望 10", total)
	}
	if reader.n != 10 {
		t.Fatalf("计数 = %d，期望 10", reader.n)
	}

	// 传输中途失败时，已读出的字节数必须保留——这正是 blobstore 丢弃、
	// 而本 FR 必须补回来的那份信息。
	boom := &countingReader{r: &failAfterReader{remaining: 3, err: io.ErrUnexpectedEOF}}
	_, err := io.Copy(io.Discard, boom)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("期望中途失败，实际 %v", err)
	}
	if boom.n != 3 {
		t.Fatalf("失败后计数 = %d，期望 3", boom.n)
	}
}

// failAfterReader 先给出 remaining 字节，随后返回自定错误。
type failAfterReader struct {
	remaining int
	err       error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, r.err
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

// TestDeclaredContentLength 声明长度取自已持有的响应头；缺失或非法一律 -1（spec §6 R2）。
func TestDeclaredContentLength(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		header http.Header
		want   int64
	}{
		{name: "无响应头", header: nil, want: -1},
		{name: "缺失（chunked）", header: http.Header{}, want: -1},
		{name: "正常声明", header: http.Header{"Content-Length": []string{"15728640"}}, want: 15728640},
		{name: "非法值", header: http.Header{"Content-Length": []string{"abc"}}, want: -1},
		{name: "负数", header: http.Header{"Content-Length": []string{"-5"}}, want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := declaredContentLength(tc.header); got != tc.want {
				t.Fatalf("声明长度 = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// TestTransferFailureNotesAreDistinguishable slow_rate 与 transfer_aborted 的日志文案
// 与处置建议必须一眼可分（spec §2.4）：调超时 / 等带宽 vs 查上游断连 / 中间设备。
func TestTransferFailureNotesAreDistinguishable(t *testing.T) {
	t.Parallel()
	slowNote, slowAdvice := transferFailureNote(upstreamCategorySlowRate)
	abortedNote, abortedAdvice := transferFailureNote(upstreamCategoryTransferAborted)
	if slowNote == "" || slowAdvice == "" || abortedNote == "" || abortedAdvice == "" {
		t.Fatal("两类传输失败的说明与建议都必须非空，否则日志无法区分")
	}
	if slowNote == abortedNote {
		t.Fatal("slow_rate 与 transfer_aborted 的说明不得相同")
	}
	if slowAdvice == abortedAdvice {
		t.Fatal("slow_rate 与 transfer_aborted 的建议不得相同")
	}
	if !strings.Contains(slowAdvice, "JIAN_UPSTREAM_TIMEOUT") {
		t.Fatalf("slow_rate 应建议调大回源超时或等带宽，实际：%q", slowAdvice)
	}
	if !strings.Contains(abortedAdvice, "上游") || !strings.Contains(abortedAdvice, "中间代理") {
		t.Fatalf("transfer_aborted 应建议查上游主动断连与中间设备，实际：%q", abortedAdvice)
	}

	// 本地写失败的建议必须指向磁盘，且明确说明不计入上游判定——
	// 这是「不把运维引向查上游」的最后一道防线。
	localNote, localAdvice := transferFailureNote(upstreamCategoryLocalWriteFailure)
	if !strings.Contains(localNote, "本地存储") || strings.Contains(localNote, "上游慢") {
		t.Fatalf("本地写失败的说明必须明确指向本地存储，实际：%q", localNote)
	}
	if !strings.Contains(localAdvice, "磁盘") || !strings.Contains(localAdvice, "不计入上游判定") {
		t.Fatalf("本地写失败应建议查磁盘并声明不计入上游判定，实际：%q", localAdvice)
	}
}

// TestSettleTransferFailureNeverBlocks 三类传输失败统一走 settleUpstreamFailure 收口：
// 只记事件 + 释放半开名额，状态与退避窗口一律不动。
func TestSettleTransferFailureNeverBlocks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		verdict    upstreamFailureVerdict
		wantReason UpstreamBlockReason
	}{
		{
			name:       "no_response 只记 upstream_timeout",
			verdict:    classifyTransferFailure(&url.Error{Err: timeoutError{}}, 0, 1<<20),
			wantReason: UpstreamBlockReasonTimeout,
		},
		{
			name:       "slow_rate 只记 slow_transfer",
			verdict:    classifyTransferFailure(&url.Error{Err: timeoutError{}}, 1<<20, 15<<20),
			wantReason: UpstreamBlockReasonSlowTransfer,
		},
		{
			name:       "transfer_aborted 只记 transfer_aborted",
			verdict:    classifyTransferFailure(io.ErrUnexpectedEOF, 1<<20, 15<<20),
			wantReason: UpstreamBlockReasonTransferAborted,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repoID := int64(500 + i)
			spy := &recorderSpy{}
			h := newProxyHealth(func(context.Context, string, string) error { return nil })
			h.setBase(deterministicBase)
			h.setBlockRecorder(spy)
			stopProbe(t, h, repoID)
			newHalfOpenRepo(h, repoID)
			if !h.beginUpstream(repoID) {
				t.Fatal("半开且名额空闲时应放行本轮探测")
			}
			svc := &AssetService{health: h}

			before := h.status(repoID)
			svc.settleUpstreamFailure(repoID, "http://upstream", "", tc.verdict)
			after := h.status(repoID)

			if after != before {
				t.Fatalf("传输失败不得改动状态与退避窗口：before=%+v after=%+v", before, after)
			}
			if got := spy.reasons(); len(got) != 1 || got[0] != string(tc.wantReason) {
				t.Fatalf("应只记录一次 %s，实际 %v", tc.wantReason, got)
			}
			if !h.beginUpstream(repoID) {
				t.Fatal("传输失败必须释放半开名额，否则后续请求会被无限快速失败")
			}
		})
	}
}

// TestLocalWriteFailureLogLineIsRedactedAndSpecific 日志必须带齐排障字段、类别可区分，
// 且不泄露凭据 / 完整上游 URL / 内网地址；本地写失败一行不得出现任何「上游慢/断」措辞。
func TestLocalWriteFailureLogLineIsRedactedAndSpecific(t *testing.T) {
	// 抓日志的用例必须串行：log.SetOutput 是进程级写入。
	line := captureSingleTransferLog(t, "pkg/local-write.jar", func(svc *AssetService) {
		verdict := classifyTransferFailure(
			fmt.Errorf("写入临时 blob：%w", &os.PathError{Op: "write", Err: syscall.ENOSPC}),
			2<<20, 15<<20,
		)
		svc.logTransferFailure("raw-proxy", "pkg/local-write.jar", verdict, 2<<20, 15<<20, 1234*time.Millisecond)
	})

	for _, want := range []string{
		"WARN 回源传输失败",
		"repo=raw-proxy",
		"path=pkg/local-write.jar",
		"category=" + upstreamCategoryLocalWriteFailure,
		"transferred_bytes=2097152",
		"declared_bytes=15728640",
		"elapsed_ms=1234",
		"本地存储写入失败",
		"不计入上游判定",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("日志缺少 %q：%s", want, line)
		}
	}
	for _, unwanted := range []string{"category=" + upstreamCategorySlowRate, "category=" + upstreamCategoryTransferAborted, "带宽"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("本地写失败日志不得出现 %q（会把运维引向查上游）：%s", unwanted, line)
		}
	}
	// 取消类不产生日志：它是请求生命周期的正常终点。
	canceledLines := captureSingleTransferLog(t, "", func(svc *AssetService) {
		svc.logTransferFailure("raw-proxy", "pkg/canceled.jar",
			classifyTransferFailure(fmt.Errorf("%w", context.Canceled), 10, 100), 10, 100, time.Millisecond)
	})
	if canceledLines != "" {
		t.Fatalf("取消不应产生传输失败日志，实际：%s", canceledLines)
	}
}

// captureSingleTransferLog 抓一条含 marker 的日志行；marker 为空时返回全部捕获内容。
// 调用方用例必须串行（log.SetOutput 是进程级写入）。
func captureSingleTransferLog(t *testing.T, marker string, fn func(*AssetService)) string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	previous := log.Writer()
	log.SetOutput(logSinkFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })

	// 只装配日志需要的依赖：repos 为 nil 时 upstream_host 字段整段省略，
	// 因此这里可以断言「无上游信息也不影响其它字段」。
	fn(&AssetService{})

	mu.Lock()
	defer mu.Unlock()
	out := buf.String()
	if marker == "" {
		return strings.TrimSpace(out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}
