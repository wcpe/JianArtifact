package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestSetTimeout 验证 SetTimeout 后回源整体超时按新值生效（FR-89 回源超时动态配置）。
func TestSetTimeout(t *testing.T) {
	// 慢服务器：固定 200ms 后才响应。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(DefaultTimeout)
	// 默认超时（30s）下慢服务器可完成。
	if _, _, err := c.Fetch(context.Background(), srv.URL, "/"); err != nil {
		t.Fatalf("默认超时下回源应成功：%v", err)
	}
	// SetTimeout 收紧到 50ms 后应超时。
	c.SetTimeout(50 * time.Millisecond)
	if _, _, err := c.Fetch(context.Background(), srv.URL, "/"); !IsTimeout(err) {
		t.Errorf("SetTimeout 后回源应超时，得 %v", err)
	}
	// SetTimeout(<=0) 回退默认超时：慢服务器恢复成功。
	c.SetTimeout(0)
	if _, _, err := c.Fetch(context.Background(), srv.URL, "/"); err != nil {
		t.Fatalf("SetTimeout(0) 应回退默认超时：%v", err)
	}
}

// TestClientConcurrentSetTimeoutAndFetch 并发 SetTimeout 与 Fetch 无数据竞争（-race 验证）。
func TestClientConcurrentSetTimeoutAndFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = c.Fetch(context.Background(), srv.URL, "/")
		}()
		go func(n int) {
			defer wg.Done()
			c.SetTimeout(time.Duration(n+1) * time.Millisecond)
		}(i)
	}
	wg.Wait()
}
