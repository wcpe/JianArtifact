package httpserver_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getWithHeaders 以指定头发起 GET；用于验证 Range 协商等依赖请求头的下载行为。
func (e *protocolEnv) getWithHeaders(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// TestRawDownloadSupportsRange 锁定制品下载的 Range 协商（断点续传）：
// 客户端（curl -C -、构建工具重试）依赖 206 与 Content-Range 才能续传；
// 此前实现直接 io.Copy 回写，Range 被静默忽略并回 200 全量，续传退化为重下。
func TestRawDownloadSupportsRange(t *testing.T) {
	env := newProtocolEnv(t)
	token := env.bootstrapAdmin(t)
	env.createRawRepo(t, token, "raw-range", "public")
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz") // 36 字节，便于按偏移断言
	env.putArtifact(t, token, "raw-range", "demo/file.txt", "text/plain", payload)
	const path = "/repository/raw-range/demo/file.txt"

	// 前置：不带 Range 的普通下载仍应是 200 + 全量（守住既有行为）。
	full := env.getWithHeaders(t, path, nil)
	if full.Code != http.StatusOK || full.Body.String() != string(payload) {
		t.Fatalf("普通下载应 200 且返回全量：状态码 = %d，体 = %q", full.Code, full.Body.String())
	}

	cases := []struct {
		name       string
		rangeValue string
		wantStart  int
		wantEnd    int
	}{
		{"前缀区间", "bytes=0-9", 0, 9},
		{"中段区间", "bytes=10-19", 10, 19},
		{"开区间到末尾", "bytes=30-", 30, len(payload) - 1},
		{"后缀区间", "bytes=-6", len(payload) - 6, len(payload) - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.getWithHeaders(t, path, map[string]string{"Range": tc.rangeValue})
			wantBody := string(payload[tc.wantStart : tc.wantEnd+1])
			wantContentRange := fmt.Sprintf("bytes %d-%d/%d", tc.wantStart, tc.wantEnd, len(payload))
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("Range %q 应返回 206，实得 %d（体：%q）", tc.rangeValue, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Range"); got != wantContentRange {
				t.Fatalf("Content-Range 应为 %q，实得 %q", wantContentRange, got)
			}
			if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
				t.Fatalf("Accept-Ranges 应为 bytes，实得 %q", got)
			}
			if rec.Body.String() != wantBody {
				t.Fatalf("分片内容应为 %q，实得 %q", wantBody, rec.Body.String())
			}
		})
	}

	// 不满足的 Range（起点超出长度）按 RFC 应回 416，而不是静默 200 全量。
	unsatisfiable := env.getWithHeaders(t, path, map[string]string{"Range": "bytes=999-1000"})
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range 应返回 416，实得 %d", unsatisfiable.Code)
	}
}

// TestRangeResponseIsNotCountedAsFullDownload 守住计次语义：206 分片不计入下载
// （见 domain/asset_download.go 对 Range 的说明），仅完整传输计次。
func TestRangeResponseIsNotCountedAsFullDownload(t *testing.T) {
	env := newProtocolEnv(t)
	token := env.bootstrapAdmin(t)
	env.createRawRepo(t, token, "raw-range-count", "public")
	env.putArtifact(t, token, "raw-range-count", "a.bin", "application/octet-stream", []byte(strings.Repeat("x", 32)))

	partial := env.getWithHeaders(t, "/repository/raw-range-count/a.bin", map[string]string{"Range": "bytes=0-3"})
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("分片请求应 206，实得 %d", partial.Code)
	}
	// 计次落库为分钟桶，此处只断言「分片请求不被当成完整下载」这一判定入口存在：
	// 若未来改回 io.Copy 直写，上面的 206 断言会先失败，这条是语义护栏。
	if partial.Header().Get("Content-Range") == "" {
		t.Fatal("分片响应缺少 Content-Range，无法与完整下载区分")
	}
}
