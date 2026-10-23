package domain

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/upstream"
)

// 本文件实现 FR-158（回源失败原因五分类）里「已拿到响应头之后」的那一半判定。
//
// 为什么必须与 classifyUpstreamFailure 分成两个函数（见 docs/specs/0.13.0-upstream-failure-classification.md）：
// 前者只能拿到 err，无法知道「是否已拿到响应头」，而这两件事的判据完全不同——
//   - 拿头之前失败：上游侧问题（不可达 / 没响应），其中「不可达」要阻断；
//   - 拿头之后失败：传输侧问题（慢 / 中途断 / 本地磁盘写不进去），一律不阻断。
//
// 两者共用 upstreamFailureVerdict 与 settleUpstreamFailure 收口，避免出现第二套副作用逻辑。

// countingReader 统计已成功从上游读出的字节数。
//
// 为什么不复用 blobstore 的返回值：blobstore.put 在 io.Copy 失败时按既有约定返回
// size = 0（它同时服务 hosted 上传等调用方，签名与内部逻辑不得为了本 FR 改动），
// 已传字节数在那里被丢弃。因此在 putStagedBlob 外层包一层计数 reader（见 docs/specs/0.13.0-upstream-failure-classification.md）。
//
// 语义是「已从上游读出并交给写入方的字节数」，比「真正落盘的字节数」略大（至多一个
// io.Copy 缓冲区），对「是否有传输进展」的判定没有影响。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// 回源失败的**可观测类别**（见 docs/specs/0.13.0-upstream-failure-classification.md）。
//
// 与 upstreamFailureKind 的分工：kind 面向断路器（阻断 / 不阻断），category 面向
// 日志与运维排障——「该调超时、该重试、该查上游、还是该查本地磁盘」由它回答。
// 只用于结构化日志，不参与任何状态判定，因此是自由字符串里受控的一小组常量。
const (
	// upstreamCategoryUnreachable 连接 / DNS / TLS 失败、上游 5xx / 401 / 403：真不可达。
	upstreamCategoryUnreachable = "unreachable"
	// upstreamCategoryNoResponse 未拿到响应头即超时，或拿到响应头却一个字节都没传出去。
	upstreamCategoryNoResponse = "no_response"
	// upstreamCategorySlowRate 已拿到响应头、有传输进展、最终超时：带宽不足。
	upstreamCategorySlowRate = "slow_rate"
	// upstreamCategoryTransferAborted 已拿到响应头、传输中途被中断（连接重置 / 提前 EOF）。
	upstreamCategoryTransferAborted = "transfer_aborted"
	// upstreamCategoryConfirmedMissing 上游明确 404 / 410：确实没有。
	upstreamCategoryConfirmedMissing = "confirmed_missing"
	// upstreamCategoryLocalWriteFailure 本地存储写入失败：**不是上游故障**。
	// 单列的目的就是不让它被误报成「上游慢」或「上游失败」，把运维引向错误方向。
	upstreamCategoryLocalWriteFailure = "local_write_failure"
	// upstreamCategoryCanceled 请求被取消（客户端断连、同组其它成员已成功、优雅停机）。
	// 这是正常的请求生命周期终点，既不是上游故障也不是本地故障，因此单列且不记日志。
	upstreamCategoryCanceled = "canceled"
)

// declaredContentLength 取响应头声明的正文长度；缺失或不可解析时返回 -1。
//
// 缺失（chunked 响应）时判定退化为「只看是否超时与是否有进展」，不因此把类判错（见 docs/specs/0.13.0-upstream-failure-classification.md）。
func declaredContentLength(header http.Header) int64 {
	if header == nil {
		return -1
	}
	raw := header.Get("Content-Length")
	if raw == "" {
		return -1
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return -1
	}
	return size
}

// classifyTransferFailure 判定「已拿到响应头之后」的传输失败（见 docs/specs/0.13.0-upstream-failure-classification.md）。
//
// 输入：传输错误、已传字节数、响应头声明的长度（缺失为 -1）。
// 只有一处实现，由 fetchProxyAsset 与 ResolveOCIProxy 共用，防止两条回源链路给出不同结论。
//
// 判定顺序（顺序本身是需求，不可重排）：
//  1. 请求取消 → 既非上游故障也非本地故障，不记事件、不记日志；
//  2. 超时 → 有进展是 slow_rate（带宽不足），无进展是 no_response（按没响应算）。
//     **超时优先于「中途断」**：限速到超时在字节层面也表现为「没传满」，若不先看
//     IsTimeout 就会把它误判成 transfer_aborted（见 docs/specs/0.13.0-upstream-failure-classification.md）；
//  3. 非超时的连接重置 / 提前 EOF → transfer_aborted；
//  4. 其余（含本地磁盘写满、权限失败）→ local_write_failure。
//
// 第 4 条是本次最重要的一条：本地写失败绝不能伪装成上游问题。
func classifyTransferFailure(err error, transferred, declared int64) upstreamFailureVerdict {
	// 客户端主动断开 / 同组其它成员已成功 / 停机：常见于 group 并行回源的落选者。
	// 它既没有上游判定价值，也不该被记成本地磁盘故障，因此先摘出来。
	if errors.Is(err, context.Canceled) {
		return upstreamFailureVerdict{kind: upstreamFailureCanceled, category: upstreamCategoryCanceled}
	}
	if upstream.IsTimeout(err) {
		if transferred > 0 {
			return upstreamFailureVerdict{
				kind:     upstreamFailureTimeout,
				reason:   UpstreamBlockReasonSlowTransfer,
				category: upstreamCategorySlowRate,
			}
		}
		return upstreamFailureVerdict{
			kind:     upstreamFailureTimeout,
			reason:   UpstreamBlockReasonTimeout,
			category: upstreamCategoryNoResponse,
		}
	}
	if isTransferAborted(err, transferred, declared) {
		return upstreamFailureVerdict{
			kind:     upstreamFailureTransferAborted,
			reason:   UpstreamBlockReasonTransferAborted,
			category: upstreamCategoryTransferAborted,
		}
	}
	return upstreamFailureVerdict{
		kind:     upstreamFailureLocalWrite,
		category: upstreamCategoryLocalWriteFailure,
	}
}

// isTransferAborted 判断非超时的传输错误是否属「上游侧中途断」。
//
// 依据（见 docs/specs/0.13.0-upstream-failure-classification.md）：
//   - io.ErrUnexpectedEOF：正文短于 Content-Length 时 Go 客户端给出的标准错误；
//   - 连接重置 / 被中止 / 管道破裂：中间设备或上游直接掐连接；
//   - 裸 io.EOF：只有在「声明长度还没传满」时才算上游断流；声明长度未知（chunked）时
//     同样按上游断开处理——本地写失败与提前 EOF 在错误值上无法区分，此时宁可让人去查
//     上游，也不要静默报 500 掩盖真实断流（R3）。
func isTransferAborted(err error, transferred, declared int64) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || isConnectionReset(err) {
		return true
	}
	if errors.Is(err, io.EOF) {
		// 「已按声明长度传满却仍报 EOF」在实践中不会出现（io.Copy 会把正常 EOF 当成功），
		// 走到那里只会落到本地写失败分支。
		return declared < 0 || declared > transferred
	}
	return false
}

// isConnectionReset 判断错误链里是否有「连接被重置」一类信号。
func isConnectionReset(err error) bool {
	// net.ErrClosed：连接已被本端关闭（看门狗取消、上下文结束），同样属于传输中断，
	// 不再是「上游不可达」的证据。
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

// logTransferFailure 输出一条传输阶段失败的结构化中文日志（见 docs/specs/0.13.0-upstream-failure-classification.md）。
//
// 脱敏与 FR-157 的诊断行同口径：只输出仓库名、路径（控制字符转义 + 截断）、类别、
// 已传 / 声明字节、耗时与上游 scheme://host；**绝不**输出凭据、完整上游 URL、
// 内网地址或请求头。
func (s *AssetService) logTransferFailure(repoName, path string, verdict upstreamFailureVerdict, transferred, declared int64, elapsed time.Duration) {
	if verdict.category == "" || verdict.category == upstreamCategoryCanceled {
		return
	}
	note, advice := transferFailureNote(verdict.category)
	var b strings.Builder
	b.WriteString("WARN 回源传输失败")
	b.WriteString(" repo=" + sanitizeDiagnosticField(repoName))
	b.WriteString(" path=" + SanitizeDiagnosticPath(path))
	b.WriteString(" category=" + verdict.category)
	b.WriteString(" transferred_bytes=" + strconv.FormatInt(transferred, 10))
	b.WriteString(" declared_bytes=" + strconv.FormatInt(declared, 10))
	b.WriteString(" elapsed_ms=" + strconv.FormatInt(elapsed.Milliseconds(), 10))
	if host := s.transferUpstreamHost(repoName); host != "" {
		b.WriteString(" upstream_host=" + host)
	}
	if note != "" {
		b.WriteString(" 说明=" + note)
	}
	if advice != "" {
		b.WriteString(" 建议=" + advice)
	}
	log.Print(b.String())
}

// transferUpstreamHost 取本仓库的上游 scheme://host；取不到返回空串（该字段整段省略）。
func (s *AssetService) transferUpstreamHost(repoName string) string {
	if s.repos == nil {
		return ""
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return ""
	}
	return upstreamHost(repo)
}

// transferFailureNote 返回各类别的一句话说明与处置建议。
//
// slow_rate 与 transfer_aborted 的文案必须能一眼区分，并给出**不同**的处置方向
// （见 docs/specs/0.13.0-upstream-failure-classification.md）：前者去调超时或等带宽，后者去查上游主动断连 / 中间设备。
func transferFailureNote(category string) (note, advice string) {
	switch category {
	case upstreamCategoryNoResponse:
		return "已拿到响应头，但一个字节都没传出来", "上游可能过载或卡死，检查上游服务是否存活、响应是否被阻塞"
	case upstreamCategorySlowRate:
		return "已拿到响应头且有传输进展，未在回源超时内传完（带宽不足）", "等待带宽恢复或稍后重试即可，长期偏慢可调大 JIAN_UPSTREAM_TIMEOUT"
	case upstreamCategoryTransferAborted:
		return "已拿到响应头且已传输一部分，连接在传满之前被中断", "检查上游是否主动断连（超时/限流），以及中间代理、网关是否掐断长连接"
	case upstreamCategoryLocalWriteFailure:
		return "本地存储写入失败，与上游无关", "检查磁盘剩余空间、目录权限与 inode 用量；该失败不计入上游判定"
	default:
		return "", ""
	}
}
