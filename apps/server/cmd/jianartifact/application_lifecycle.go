package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

func (s *appServices) shutdownApplication(stopAudit, cancel context.CancelFunc, servers ...*http.Server) error {
	return s.shutdownApplicationWithin(15*time.Second, stopAudit, cancel, servers...)
}

func (s *appServices) shutdownApplicationWithin(grace time.Duration, stopAudit, cancel context.CancelFunc, servers ...*http.Server) error {
	drained := s.ingressGate.Seal()
	ingressCtx, cancelIngress := context.WithTimeout(context.Background(), grace)
	ingressErr := drainApplicationHTTP(ingressCtx, servers)
	cancelIngress()
	if ingressErr != nil {
		// 强制断连只用于中止传输；中止审计采集，不把尾段标为干净关停。
		stopAudit()
		fmt.Fprintln(os.Stderr, "HTTP 入口未能优雅排空，仍在等待已准入的处理器退出。")
	}
	// 连接关闭不代表处理器已归还数据库或文件资源；这里不承诺硬截止。
	<-drained
	auditCtx, cancelAuditDeadline := context.WithTimeout(context.Background(), 5*time.Second)
	auditErr := s.securityAudit.Close(auditCtx)
	cancelAuditDeadline()
	stopAudit()
	cancel()
	if auditErr != nil {
		auditErr = fmt.Errorf("关停安全审计：%w", auditErr)
	}
	// 受管 Mihomo 子进程必须在这里回收，不能只依赖调用方的 defer：
	// 一键重启走 syscall.Exec 替换进程映像，成功后所有 defer 都不会执行，
	// 放在 defer 里会让每次重启都留下上一代 mihomo 进程与含明文凭据的工作目录。
	if s.outboundProxySvc != nil {
		s.outboundProxySvc.Close()
	}
	return errors.Join(ingressErr, auditErr, s.db.Close())
}

func drainApplicationHTTP(ctx context.Context, servers []*http.Server) error {
	results := make(chan error, len(servers))
	active := 0
	for _, server := range servers {
		if server == nil {
			continue
		}
		active++
		go func(server *http.Server) {
			if err := server.Shutdown(ctx); err != nil {
				results <- errors.Join(fmt.Errorf("排空 HTTP 入口 %s：%w", server.Addr, err), server.Close())
				return
			}
			results <- nil
		}(server)
	}
	var err error
	for range active {
		err = errors.Join(err, <-results)
	}
	return err
}
