// Package upstream 提供 proxy 仓库的回源 HTTP 客户端。
//
// 分层（见 internal/doc.go）：位于依赖链底部，与 blobstore/persistence 平级，
// 被 domain 层编排使用。仅负责按 baseURL+path 发起只读 GET 并流式返回响应体，
// 不含缓存、并发收敛或格式语义（这些在 domain 层）。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout 是回源请求的缺省整体超时。
const DefaultTimeout = 30 * time.Second

// ErrNotFound 表示上游明确返回 404：对 proxy 视为未命中、对 group 继续下一成员。
var ErrNotFound = errors.New("upstream: 上游资源不存在")

// StatusError 表示上游返回了非 200/404 的 HTTP 状态码。
// domain 层据此映射为 502（坏网关）。
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream: 上游返回状态码 %d", e.Code)
}

// Client 是回源 HTTP 客户端，持有带超时的 *http.Client。
type Client struct {
	http *http.Client
}

// NewClient 构造回源客户端；timeout<=0 时取 DefaultTimeout。
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{http: &http.Client{Timeout: timeout}}
}

// Fetch 以 GET 拉取 baseURL 与 path 拼接后的资源，返回响应体（调用方负责关闭）与响应头。
// 上游 404 返回 ErrNotFound；其余非 2xx 返回 *StatusError；传输 / 超时错误原样返回。
func (c *Client) Fetch(ctx context.Context, baseURL, path string) (io.ReadCloser, http.Header, error) {
	full, err := joinURL(baseURL, path)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream: 构造请求：%w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, nil, &StatusError{Code: resp.StatusCode}
	}
	return resp.Body, resp.Header, nil
}

// IsTimeout 报告 err 是否源于超时（context 截止或网络超时）。
// domain 层据此把回源超时映射为 504。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// joinURL 以 baseURL 为基址拼接相对 path，去重分隔斜杠。baseURL 必须为绝对 http/https URL。
func joinURL(baseURL, path string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("upstream: 解析上游地址：%w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("upstream: 上游地址协议非法：%s", baseURL)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(path, "/")
	return base.String(), nil
}
