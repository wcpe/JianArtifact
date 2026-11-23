package main

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wcpe/jianartifact/apps/server/internal/httpserver"
)

func TestApplicationForcedDrainJoinsBothIngressHandlers(t *testing.T) {
	_, svc := newSourceTestApplication(t, "raw")
	if _, err := svc.db.Exec(`CREATE TABLE test_drain_writes (name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	auditCtx, stopAudit := context.WithCancel(context.Background())
	defer stopAudit()
	if err := svc.securityAudit.Start(auditCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.securityAudit.Close(ctx)
	})

	type lane struct {
		name                  string
		entered, exiting      chan struct{}
		write, leave          func()
		dbResult, clientError chan error
	}
	lanes := make([]lane, 2)
	var readinessCalls atomic.Int32
	handler := httpserver.New("test", httpserver.WithIngressGate(&svc.ingressGate), httpserver.WithReadinessCheck(func() error {
		readinessCalls.Add(1)
		return nil
	}), httpserver.WithProtocolRoutes(func(router gin.IRouter) {
		for i, name := range []string{"http", "tls"} {
			write, leave := make(chan struct{}), make(chan struct{})
			entry := lane{
				name: name, entered: make(chan struct{}), exiting: make(chan struct{}),
				write: sync.OnceFunc(func() { close(write) }), leave: sync.OnceFunc(func() { close(leave) }),
				dbResult: make(chan error, 1), clientError: make(chan error, 1),
			}
			lanes[i] = entry
			router.POST("/drain/"+name, func(c *gin.Context) {
				defer close(entry.exiting)
				close(entry.entered)
				<-write
				// 模拟已经进入不接受请求取消的持久化阶段。
				_, err := svc.db.Exec(`INSERT INTO test_drain_writes(name) VALUES (?)`, entry.name)
				entry.dbResult <- err
				<-leave
				c.Status(http.StatusNoContent)
			})
		}
	})).Handler(nil)
	plain, secure := httptest.NewServer(handler), httptest.NewTLSServer(handler)
	t.Cleanup(plain.Close)
	t.Cleanup(secure.Close)
	for i, server := range []*httptest.Server{plain, secure} {
		entry := lanes[i]
		t.Cleanup(entry.leave)
		t.Cleanup(entry.write)
		go func() {
			resp, err := server.Client().Post(server.URL+"/drain/"+entry.name, "", nil)
			if resp != nil {
				_ = resp.Body.Close()
			}
			entry.clientError <- err
		}()
	}
	for _, entry := range lanes {
		waitDrainTest(t, entry.entered)
	}

	forced := make(chan struct{})
	result := make(chan error, 1)
	type checkpointResult struct {
		clean int
		err   error
	}
	checkpoint := make(chan checkpointResult, 1)
	observeForced := sync.OnceFunc(func() { close(forced) })
	go func() {
		result <- svc.shutdownApplicationWithin(40*time.Millisecond, func() {
			stopAudit()
			observeForced()
		}, func() {
			var cp checkpointResult
			cp.err = svc.db.Get(&cp.clean, `SELECT COUNT(*) FROM security_audit_checkpoint WHERE clean_stop = 1`)
			checkpoint <- cp
		}, plain.Config, secure.Config)
	}()
	waitDrainTest(t, forced)
	for _, entry := range lanes {
		if err := waitDrainTest(t, entry.clientError); err == nil {
			t.Errorf("%s 客户端没有观察到强制断连", entry.name)
		}
	}

	assertPending := func() {
		t.Helper()
		select {
		case err := <-result:
			t.Fatalf("连接已断开但仍有处理器未返回，停机却提前结束：%v；DB Ping：%v", err, svc.db.Ping())
		case <-time.After(100 * time.Millisecond):
		}
	}
	assertPending()
	seenIDs := make(map[string]bool)
	for _, server := range []*httptest.Server{plain, secure} {
		request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		const clientID = "00000000000000000000000000000001"
		request.Header.Set("X-Request-ID", clientID)
		response := httptest.NewRecorder()
		// 监听器已关；直接验证抵达两个入口的处理器仍会拒绝新准入。
		server.Config.Handler.ServeHTTP(response, request)
		id := response.Header().Get("X-Request-ID")
		decoded, err := hex.DecodeString(id)
		if response.Code != http.StatusServiceUnavailable || err != nil || len(decoded) != 16 || id == clientID || seenIDs[id] || readinessCalls.Load() != 0 {
			t.Errorf("封口应在业务前拒绝，只返回新标识：状态=%d，标识=%q，就绪调用=%d", response.Code, id, readinessCalls.Load())
		}
		seenIDs[id] = true
	}
	for _, entry := range lanes {
		entry.write()
		if err := waitDrainTest(t, entry.dbResult); err != nil {
			t.Errorf("%s 处理器断连后写入 SQLite 失败：%v", entry.name, err)
		}
	}
	lanes[0].leave()
	waitDrainTest(t, lanes[0].exiting)
	assertPending()
	var writes int
	if err := svc.db.Get(&writes, `SELECT COUNT(*) FROM test_drain_writes`); err != nil || writes != len(lanes) {
		t.Errorf("另一处理器退出前 SQLite 应可读且保有两笔写入：写入=%d，错误=%v", writes, err)
	}
	lanes[1].leave()
	waitDrainTest(t, lanes[1].exiting)
	shutdownErr := waitDrainTest(t, result)
	replaced := false
	err := finishApplication(shutdownErr, true, func() error {
		replaced = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || replaced {
		t.Errorf("强制停机应返回入口超时且不自替换：错误=%v，自替换=%v", err, replaced)
	}
	if cp := waitDrainTest(t, checkpoint); cp.err != nil || cp.clean != 0 || svc.securityAudit.Health().Coverage != "unknown" {
		t.Errorf("强制停机不得标记干净或声称覆盖完整：干净标记=%d，错误=%v，审计=%+v", cp.clean, cp.err, svc.securityAudit.Health())
	}
	if err := svc.db.Ping(); err == nil {
		t.Error("停机返回后数据库仍开放")
	}
}

func waitDrainTest[T any](t *testing.T, signal <-chan T) T {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("等待关停时序证据超时")
		var zero T
		return zero
	}
}
